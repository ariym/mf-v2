package modules

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
)

func TestUpdateStagesBeforeReplacementAndHonorsDependencies(t *testing.T) {
	ctx := context.Background()
	s, err := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	payload := archive(t, []*tar.Header{{Name: "bin/tool", Mode: 0755, Typeflag: tar.TypeReg}}, []string{"#!/bin/sh\nexit 0\n"})
	sum := sha256.Sum256(payload)
	manifest := func(version int) hub.Program {
		return hub.Program{ProgramName: "tool", Version: version, Desc: "Tool", GitRepo: "https://github.com/example/tool", InputFormats: []hub.MediaInputFormat{{Container: ".wav"}}, Artifacts: []hub.Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://example.com/tool.tar.gz", SHA256: hex.EncodeToString(sum[:]), Entrypoint: "bin/tool"}}}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := 1
		for _, n := range []int{2, 3} {
			if strings.HasSuffix(r.URL.Path, fmt.Sprintf("/versions/%d", n)) {
				version = n
			}
		}
		_ = json.NewEncoder(w).Encode(manifest(version))
	}))
	defer upstream.Close()
	downloads := 0
	i := Installer{Store: s, Root: filepath.Join(t.TempDir(), "modules"), Hub: hub.Client{URL: upstream.URL}, Download: func(_ context.Context, _ string, w io.Writer) error {
		downloads++
		_, err := w.Write(payload)
		return err
	}}
	ref := hub.Ref{Kind: hub.Programs, Identifier: "tool", Version: 1}
	if _, err = i.Install(ctx, ref); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Module(ctx, hub.Programs, "tool")
	i.Download = func(context.Context, string, io.Writer) error { return fmt.Errorf("simulated network failure") }
	ref.Version = 2
	if _, err = i.Update(ctx, ref); err == nil {
		t.Fatal("accepted failed download")
	}
	after, _ := s.Module(ctx, hub.Programs, "tool")
	if after.Version != 1 {
		t.Fatal("failed update removed existing record")
	}
	if err = Executable(before.Executable); err != nil {
		t.Fatal("failed update removed executable")
	}
	i.Download = func(_ context.Context, _ string, w io.Writer) error {
		downloads++
		_, err := io.Copy(w, bytes.NewReader(payload))
		return err
	}
	if _, err = i.Update(ctx, ref); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Module(ctx, hub.Programs, "tool")
	if after.Version != 2 {
		t.Fatal("update did not replace version")
	}
	if _, err = os.Stat(before.InstallDir); !os.IsNotExist(err) {
		t.Fatal("old installation not cleaned")
	}
	owner := hub.Flow{Key: "owner", Title: "Owner", Version: 1, Desc: "Depends on tool", GHRepo: "https://github.com/example/owner", Programs: hub.Versions{"tool": 2}, Artifacts: manifest(1).Artifacts}
	raw, _ := json.Marshal(owner)
	if err = s.RegisterInstall(ctx, nil, []store.Module{{Kind: hub.Flows, Key: "owner", Version: 1, Manifest: raw, Source: "test", InstallDir: "", Executable: after.Executable, OS: runtime.GOOS, Arch: runtime.GOARCH}}, "", "test"); err != nil {
		t.Fatal(err)
	}
	count := downloads
	ref.Version = 3
	if _, err = i.Update(ctx, ref); err == nil || !strings.Contains(err.Error(), "required by") {
		t.Fatalf("dependency blocker lost: %v", err)
	}
	if downloads != count {
		t.Fatal("downloaded despite dependency blocker")
	}
	after, _ = s.Module(ctx, hub.Programs, "tool")
	if after.Version != 2 {
		t.Fatal("blocked update changed version")
	}
	// Recover a crash after commit but before cleaning the previous directory.
	if err = os.MkdirAll(before.InstallDir, 0700); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(i.Root)
	i.Root = canonical
	paths, _ := json.Marshal([]installPath{{Stage: filepath.Join(canonical, ".stage-crashed"), Final: after.InstallDir, Old: before.InstallDir}})
	if _, err = s.DB.Exec("INSERT INTO install_journal VALUES('replacement',?,'now')", string(paths)); err != nil {
		t.Fatal(err)
	}
	if err = i.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(before.InstallDir); !os.IsNotExist(err) {
		t.Fatal("replacement recovery left old files")
	}
	if err = Executable(after.Executable); err != nil {
		t.Fatal("recovery removed committed executable")
	}
}
