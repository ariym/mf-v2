package modules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func archive(t *testing.T, headers []*tar.Header, bodies []string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, h := range headers {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[n]))
		}
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			if _, e := tw.Write([]byte(bodies[n])); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestExtractContainsLinksAndRejectsUnsafeArchives(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		bodies  []string
		valid   bool
	}{
		{"runtime", []*tar.Header{{Name: "bin/tool", Mode: 0755, Typeflag: tar.TypeReg}, {Name: "lib/runtime", Typeflag: tar.TypeSymlink, Linkname: "../bin/tool"}}, []string{"tool", ""}, true},
		{"hardlink", []*tar.Header{{Name: "bin/tool", Mode: 0755, Typeflag: tar.TypeReg}, {Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}, []string{"tool", ""}, true},
		{"traversal", []*tar.Header{{Name: "../escape", Mode: 0755, Typeflag: tar.TypeReg}}, []string{"bad"}, false},
		{"absolute", []*tar.Header{{Name: "/escape", Mode: 0755, Typeflag: tar.TypeReg}}, []string{"bad"}, false},
		{"escaping-link", []*tar.Header{{Name: "bin/tool", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}, []string{""}, false},
		{"write-through-link", []*tar.Header{{Name: "dir", Typeflag: tar.TypeSymlink, Linkname: "safe"}, {Name: "dir/tool", Typeflag: tar.TypeReg, Mode: 0755}}, []string{"", "bad"}, false},
		{"device", []*tar.Header{{Name: "device", Typeflag: tar.TypeChar}}, []string{""}, false},
		{"duplicate", []*tar.Header{{Name: "tool", Typeflag: tar.TypeReg, Mode: 0755}, {Name: "tool", Typeflag: tar.TypeReg, Mode: 0755}}, []string{"one", "two"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			err := Extract(context.Background(), bytes.NewReader(archive(t, tc.headers, tc.bodies)), root)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
func TestInstallConflictRecoveryAndRemoval(t *testing.T) {
	ctx := context.Background()
	s, e := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	root := filepath.Join(t.TempDir(), "mf_modules")
	payload := archive(t, []*tar.Header{{Name: "bin/tool", Mode: 0755, Typeflag: tar.TypeReg}}, []string{"#!/bin/sh\nexit 0\n"})
	digest := sha256.Sum256(payload)
	asset := hub.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://example.com/tool.tar.gz", SHA256: hex.EncodeToString(digest[:]), Entrypoint: "bin/tool"}
	def := hub.TagType{Key: "confidence", Version: 1, DataType: "number"}
	program := hub.Program{ProgramName: "tool", Version: 1, Desc: "Tool", GitRepo: "https://github.com/example/tool", Artifacts: []hub.Artifact{asset}, InputFormats: []hub.MediaInputFormat{{Container: ".wav"}}}
	flow := hub.Flow{Key: "test", Title: "Test", Version: 1, Desc: "Test", GHRepo: "https://github.com/example/flow", Programs: hub.Versions{"tool": 1}, Flows: hub.Versions{}, TagTypes: hub.Versions{"confidence": 1}, Artifacts: []hub.Artifact{asset}}
	records := map[hub.Ref]hub.Record{}
	for kind, value := range map[string]any{hub.TagTypes: def, hub.Programs: program, hub.Flows: flow} {
		raw, _ := json.Marshal(value)
		r, e := hub.Decode(kind, raw)
		if e != nil {
			t.Fatal(e)
		}
		records[r.Ref] = r
	}
	lookup := func(_ context.Context, r hub.Ref) (hub.Record, error) {
		record, ok := records[r]
		if !ok {
			return record, fmt.Errorf("missing")
		}
		return record, nil
	}
	plan, e := hub.Resolve(ctx, records[hub.Ref{Kind: hub.Flows, Identifier: "test", Version: 1}], lookup, runtime.GOOS, runtime.GOARCH)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resolve") {
			json.NewEncoder(w).Encode(plan)
			return
		}
		json.NewEncoder(w).Encode(flow)
	}))
	defer server.Close()
	downloads := 0
	i := Installer{Store: s, Root: root, Hub: hub.Client{URL: server.URL}, Download: func(_ context.Context, _ string, w io.Writer) error { downloads++; _, e := w.Write(payload); return e }}
	ref := hub.Ref{Kind: hub.Flows, Identifier: "test", Version: 1}
	for range 2 {
		if _, e = i.Install(ctx, ref); e != nil {
			t.Fatal(e)
		}
	}
	if downloads != 2 {
		t.Fatalf("downloaded installed artifacts again: %d", downloads)
	}
	p, e := Pin(ctx, s, ref)
	if e != nil || len(p.Programs) != 1 || len(p.Entities) != 3 {
		t.Fatalf("snapshot %+v %v", p, e)
	}
	if p.Root.OS != runtime.GOOS || p.Root.Arch != runtime.GOARCH || p.Root.ArtifactSHA256 != asset.SHA256 {
		t.Fatalf("selected artifact metadata missing: %+v", p.Root)
	}
	if _, e = s.DB.Exec("UPDATE flow SET platform_os='other' WHERE key='test'"); e != nil {
		t.Fatal(e)
	}
	if _, e = Pin(ctx, s, ref); e == nil {
		t.Fatal("accepted an installed artifact from another platform")
	}
	if _, e = s.DB.Exec("UPDATE flow SET platform_os=? WHERE key='test'", runtime.GOOS); e != nil {
		t.Fatal(e)
	}
	// A removal interrupted before its database commit must preserve the
	// registered executable when journal recovery runs.
	journalRoot, err := filepath.EvalSymlinks(i.Root)
	if err != nil {
		t.Fatal(err)
	}
	journalPaths, _ := json.Marshal([]installPath{{Stage: filepath.Join(journalRoot, ".remove-interrupted"), Final: p.Root.InstallDir}})
	if _, e = s.DB.Exec("INSERT INTO install_journal VALUES('remove-interrupted',?,'now')", string(journalPaths)); e != nil {
		t.Fatal(e)
	}
	unlockRecovery, e := i.Lock()
	if e != nil {
		t.Fatal(e)
	}
	e = i.recover(ctx)
	unlockRecovery()
	if e != nil {
		t.Fatal(e)
	}
	if e = Executable(p.Root.Executable); e != nil {
		t.Fatalf("recovery removed registered module: %v", e)
	}
	if e = i.Remove(ctx, hub.Programs, "tool"); e == nil {
		t.Fatal("removed flow dependency")
	}
	if e = i.Remove(ctx, hub.Flows, "test"); e != nil {
		t.Fatal(e)
	}
	if e = i.Remove(ctx, hub.Programs, "tool"); e != nil {
		t.Fatal(e)
	}
	// A checksum failure leaves neither registered records nor final directories.
	downloads = 0
	i.Download = func(_ context.Context, _ string, w io.Writer) error { _, e := w.Write([]byte("bad")); return e }
	if _, e = i.Install(ctx, ref); e == nil {
		t.Fatal("accepted wrong checksum")
	}
	if _, e = s.Module(ctx, hub.Flows, "test"); e == nil {
		t.Fatal("partial database registration")
	}
	if _, e = os.Stat(filepath.Join(root, "flows", "test", "1")); !os.IsNotExist(e) {
		t.Fatalf("left partial files: %v", e)
	}
	i.Download = func(context.Context, string, io.Writer) error { return fmt.Errorf("artifact download HTTP 404") }
	if _, e = i.Install(ctx, ref); e == nil || !strings.Contains(e.Error(), "download programs/tool@1: artifact download HTTP 404") {
		t.Fatalf("missing dependency context: %v", e)
	}
	// Simulate a crash after a rename but before database registration.
	canonical, _ := filepath.EvalSymlinks(root)
	stage := filepath.Join(canonical, "programs", "orphan", ".stage-x")
	final := filepath.Join(canonical, "programs", "orphan", "1")
	if e = os.MkdirAll(final, 0700); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal([]installPath{{Stage: stage, Final: final}})
	if _, e = s.DB.Exec("INSERT INTO install_journal VALUES('interrupted',?,'now')", string(raw)); e != nil {
		t.Fatal(e)
	}
	i.Root = canonical
	unlock, e := i.Lock()
	if e != nil {
		t.Fatal(e)
	}
	e = i.recover(ctx)
	unlock()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(final); !os.IsNotExist(e) {
		t.Fatal("recovery left unregistered directory")
	}
}
func TestCompatibilityMatchesAudioRestrictionsOnOneStream(t *testing.T) {
	rate := 16000
	p := hub.Program{ProgramName: "tool", Version: 1, InputFormats: []hub.MediaInputFormat{{Container: ".wav", AudioCodec: "pcm_s16le", SampleRateHz: &rate}}}
	wrong := 48000
	m := store.Media{FilePath: "/media/X.WAV", MediaType: "audio", Audio: []store.Audio{{Codec: "pcm_s16le", SampleRate: &wrong}, {Codec: "aac", SampleRate: &rate}}}
	if Compatible(p, m) == nil {
		t.Fatal("combined restrictions across streams")
	}
	m.Audio[0].SampleRate = &rate
	if e := Compatible(p, m); e != nil {
		t.Fatal(e)
	}
	p.InputFormats = nil
	if Compatible(p, m) == nil {
		t.Fatal("treated unknown formats as unrestricted")
	}
}
