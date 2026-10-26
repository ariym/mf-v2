package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/kballard/go-shellquote"
)

// This test binary also serves as a real protocol-v1 executable subprocess.
func TestDownloadedChild(t *testing.T) {
	mode := os.Getenv("MF_TEST_DOWNLOADED_MODE")
	if mode == "" {
		return
	}
	var request protocolRequest
	if e := json.NewDecoder(os.Stdin).Decode(&request); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
	if mode == "graph" {
		graphChild(request)
		return
	}
	if request.ProtocolVersion != 1 || request.JobID == "" || !filepath.IsAbs(request.DBPath) || request.Programs == nil || request.Flows == nil {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "test flow diagnostic")
	if mode == "malformed" {
		fmt.Print("not JSON")
		os.Exit(0)
	}
	if mode == "slow" || mode == "resume" {
		marker := filepath.Join(os.Getenv("MF_TEST_DOWNLOADED_MARKERS"), request.JobID)
		if _, e := os.Stat(marker); os.IsNotExist(e) {
			if e = os.WriteFile(marker, []byte(request.JobID), 0600); e != nil {
				os.Exit(4)
			}
			time.Sleep(30 * time.Second)
		}
	}
	if mode == "scalar" {
		fmt.Print(`{"protocol_version":1,"result":9007199254740993}`)
		os.Exit(0)
	}
	var input struct {
		Media []string `json:"media"`
	}
	_ = json.Unmarshal(request.Input, &input)
	if len(input.Media) != 1 {
		os.Exit(5)
	}
	result := map[string]any{"tags": []any{map[string]any{"key": "confidence", "version": 1, "media_path": input.Media[0], "value": 0.8}}, "unknown_upstream": map[string]any{"retained": true}}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol_version": 1, "result": result})
	os.Exit(0)
}
func externalFixture(t *testing.T, mode string) (*store.Store, string) {
	t.Helper()
	t.Setenv("MF_TEST_DOWNLOADED_MODE", mode)
	t.Setenv("MF_TEST_DOWNLOADED_MARKERS", t.TempDir())
	s, e := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	wrapper := filepath.Join(t.TempDir(), "flow")
	if e = os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellquote.Join(exe, "-test.run=^TestDownloadedChild$")+"\n"), 0755); e != nil {
		t.Fatal(e)
	}
	def := hub.TagType{Key: "confidence", Version: 1, DataType: "number", Format: &hub.TagTypeFormat{Range: []float64{0, 1}}}
	f := hub.Flow{Key: "test_external", Title: "Test", Version: 1, Desc: "Test", GHRepo: "https://github.com/example/flow", Programs: hub.Versions{}, Flows: hub.Versions{}, TagTypes: hub.Versions{"confidence": 1}, Artifacts: []hub.Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://example.com/flow.tar.gz", SHA256: strings.Repeat("a", 64), Entrypoint: "flow"}}}
	raw, _ := json.Marshal(f)
	if e = s.RegisterInstall(context.Background(), []hub.TagType{def}, []store.Module{{Kind: hub.Flows, Key: f.Key, Version: 1, Manifest: raw, Source: "test", Executable: wrapper}}, "", "test"); e != nil {
		t.Fatal(e)
	}
	audio := filepath.Join(t.TempDir(), "audio.wav")
	if _, e = exec.LookPath("ffmpeg"); e != nil {
		t.Skip("FFmpeg unavailable")
	}
	if out, e := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "pcm_s16le", audio).CombinedOutput(); e != nil {
		t.Fatalf("fixture: %v %s", e, out)
	}
	return s, audio
}
func TestDownloadedFlowPathImportIngestionAndLogs(t *testing.T) {
	ctx := context.Background()
	s, audio := externalFixture(t, "good")
	e := newTestEngine(t, s)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	id, err := e.SubmitPaths(ctx, "test_external", json.RawMessage(`{}`), []string{audio})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, e, id, "completed")
	if j.Flow != "test_external" {
		t.Fatalf("metadata %+v", j)
	}
	tags, err := s.Tags(ctx, 0)
	if err != nil || len(tags) != 1 {
		t.Fatalf("tags %+v %v", tags, err)
	}
	segments, err := s.Segments(ctx, 0)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments %+v %v", segments, err)
	}
	logs, err := e.Logs(ctx, id, 0)
	if err != nil || !strings.Contains(string(logs), "test flow diagnostic") {
		t.Fatalf("logs %s %v", logs, err)
	}
	listed, err := e.List(ctx, filepath.Dir(audio), "completed", 10, 0)
	if err != nil || len(listed) != 1 || listed[0].ID != id {
		t.Fatalf("list %+v %v", listed, err)
	}
	if _, err = e.Submit(ctx, downloadedFlowName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("exposed internal adapter")
	}
}
func TestDownloadedCancellationAndRetry(t *testing.T) {
	ctx := context.Background()
	s, audio := externalFixture(t, "slow")
	e := newTestEngine(t, s)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	id, err := e.SubmitPaths(ctx, "test_external", json.RawMessage(`{}`), []string{audio})
	if err != nil {
		t.Fatal(err)
	}
	waitMarker(t, id)
	if err = s.RemoveModule(ctx, hub.Flows, "test_external"); err == nil {
		t.Fatal("removed active pinned flow")
	}
	if err = e.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, "cancelled")
	tags, err := s.Tags(ctx, 0)
	if err != nil || len(tags) != 0 {
		t.Fatalf("cancelled flow wrote tags: %+v %v", tags, err)
	}
	// The second job has a new ID and the same immutable executable/input pins.
	retry, err := e.Retry(ctx, id)
	if err != nil || retry == id {
		t.Fatalf("retry %s %v", retry, err)
	}
	if err = e.CancelJob(ctx, retry); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, retry, "cancelled")
}
func waitMarker(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(os.Getenv("MF_TEST_DOWNLOADED_MARKERS"), id)); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("flow never started")
}
func TestDownloadedRestartAndSubmissionRecovery(t *testing.T) {
	ctx := context.Background()
	s, audio := externalFixture(t, "resume")
	first := newTestEngine(t, s)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := first.SubmitPaths(ctx, "test_external", json.RawMessage(`{}`), []string{audio})
	if err != nil {
		t.Fatal(err)
	}
	waitMarker(t, id)
	stopEngine(t, first)
	second := newTestEngine(t, s)
	if err = second.Start(); err != nil {
		t.Fatal(err)
	}
	waitJob(t, second, id, "completed")
	stopEngine(t, second)
	tags, err := s.Tags(ctx, 0)
	if err != nil || len(tags) != 1 {
		t.Fatalf("restart duplicated instances %+v %v", tags, err)
	}
	// Reconcile a submission saved before its engine instance was created.
	t.Setenv("MF_TEST_DOWNLOADED_MODE", "scalar")
	third := newTestEngine(t, s)
	pending, err := third.Submit(ctx, "test_external", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DELETE FROM instances WHERE id=?", pending); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE job_metadata SET submitted=0 WHERE id=?", pending); err != nil {
		t.Fatal(err)
	}
	third.Abort()
	fourth := newTestEngine(t, s)
	if err = fourth.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, fourth)
	job := waitJob(t, fourth, pending, "completed")
	if job.Result != json.Number("9007199254740993") {
		t.Fatalf("arbitrary JSON result lost: %+v", job)
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM instances WHERE id=?", pending).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery duplicated job %d %v", count, err)
	}
}
func TestDownloadedMalformedResponseFails(t *testing.T) {
	s, _ := externalFixture(t, "malformed")
	e := newTestEngine(t, s)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	id, err := e.Submit(context.Background(), "test_external", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, e, id, "failed")
	if !strings.Contains(job.Error, "malformed") {
		t.Fatalf("wrong error: %+v", job)
	}
}

func TestDirectoryJobsUseScanResultsAndExcludeModuleRuntimes(t *testing.T) {
	ctx := context.Background()
	s, audio := externalFixture(t, "good")
	root := t.TempDir()
	canonicalRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(audio, filepath.Join(root, "linked.wav")); e != nil {
		t.Fatal(e)
	}
	modulesRoot := filepath.Join(root, "mf_modules")
	if e = os.MkdirAll(modulesRoot, 0700); e != nil {
		t.Fatal(e)
	}
	source, e := os.ReadFile(audio)
	if e != nil {
		t.Fatal(e)
	}
	bundled := filepath.Join(modulesRoot, "runtime-example.wav")
	if e = os.WriteFile(bundled, source, 0600); e != nil {
		t.Fatal(e)
	}
	duration := 1.0
	if _, e = s.Upsert(ctx, store.Media{FilePath: filepath.Join(canonicalRoot, "deleted.wav"), MediaType: "audio", Duration: &duration, FileSize: 1}); e != nil {
		t.Fatal(e)
	}
	engine, e := New(s, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	if e = engine.Start(); e != nil {
		t.Fatal(e)
	}
	defer stopEngine(t, engine)
	id, e := engine.SubmitPaths(ctx, "test_external", json.RawMessage(`{}`), []string{root})
	if e != nil {
		t.Fatal(e)
	}
	waitJob(t, engine, id, "completed")
	tags, e := s.Tags(ctx, 0)
	if e != nil || len(tags) != 1 {
		t.Fatalf("unexpected scan selection: %+v %v", tags, e)
	}
	canonicalAudio, e := filepath.EvalSymlinks(audio)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ByPath(ctx, canonicalAudio); e != nil {
		t.Fatalf("file symlink was not included: %v", e)
	}
	canonicalBundle, e := filepath.EvalSymlinks(bundled)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ByPath(ctx, canonicalBundle); e == nil {
		t.Fatal("imported bundled module media")
	}
}
