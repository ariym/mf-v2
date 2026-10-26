package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
)

var testBinary string

func TestMain(m *testing.M) {
	dir, e := os.MkdirTemp("", "mf-bin-")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	testBinary = filepath.Join(dir, "mf")
	buildArgs := []string{"build", "-o", testBinary}
	if raceEnabled {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, ".")
	cmd := exec.Command("go", buildArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	dir string
	env []string
	url string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "mf-it-")
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{dir: dir, env: os.Environ()}
	t.Cleanup(func() { _, _ = f.run("daemon", "stop", "-f"); os.RemoveAll(dir) })
	if out, e := f.run("init", "-y"); e != nil {
		t.Fatalf("init: %v %s", e, out)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	f.configure(t, "127.0.0.1", port)
	return f
}
func (f *fixture) configure(t *testing.T, host string, port int) {
	t.Helper()
	path := filepath.Join(f.dir, "mf.yaml")
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	s := strings.ReplaceAll(string(b), "server_ip: localhost", "server_ip: "+host)
	s = strings.ReplaceAll(s, "server_port: 11747", "server_port: "+strconv.Itoa(port))
	if e = os.WriteFile(path, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
	f.url = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}
func (f *fixture) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBinary, args...)
	cmd.Dir = f.dir
	cmd.Env = f.env
	out, e := cmd.CombinedOutput()
	return string(out), e
}
func (f *fixture) must(t *testing.T, args ...string) string {
	t.Helper()
	out, e := f.run(args...)
	if e != nil {
		t.Fatalf("mf %v: %v\n%s", args, e, out)
	}
	return out
}
func (f *fixture) request(t *testing.T, method, path, body string, status int) []byte {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	req, e := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	if res.StatusCode != status {
		t.Fatalf("HTTP %s: %d %s", path, res.StatusCode, b)
	}
	return b
}
func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func TestDaemonLifecycle(t *testing.T) {
	f := newFixture(t)
	f.must(t, "daemon", "start")
	f.request(t, "GET", "/healthz", "", 200)
	if out, e := f.run("daemon", "start"); e == nil || !strings.Contains(out, "already running") {
		t.Fatalf("duplicate: %v %s", e, out)
	}
	logs := f.must(t, "daemon", "logs")
	if !strings.Contains(logs, "server ready") {
		t.Fatalf("missing logs: %s", logs)
	}
	// Follow emits newly appended lines and exits cleanly on interrupt.
	capture, e := os.Create(filepath.Join(f.dir, "follow.txt"))
	if e != nil {
		t.Fatal(e)
	}
	follow := exec.Command(testBinary, "daemon", "logs", "--follow")
	follow.Dir = f.dir
	follow.Stdout = capture
	follow.Stderr = capture
	if e = follow.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = follow.Process.Kill() })
	waitUntil(t, func() bool { b, _ := os.ReadFile(capture.Name()); return bytes.Contains(b, []byte("server ready")) })
	logFile := filepath.Join(f.dir, "mf.db.mf", "daemon.log")
	log, e := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	fmt.Fprintln(log, "follow-check-marker")
	log.Close()
	waitUntil(t, func() bool {
		b, _ := os.ReadFile(capture.Name())
		return bytes.Contains(b, []byte("follow-check-marker"))
	})
	follow.Process.Signal(os.Interrupt)
	if e = follow.Wait(); e != nil {
		t.Fatal(e)
	}
	capture.Close()
	f.must(t, "daemon", "stop")
	if out := f.must(t, "daemon", "logs"); !strings.Contains(out, "server stopped") {
		t.Fatalf("logs after shutdown: %s", out)
	}
	// PID state is informational: stale/reused PIDs are never signaled.
	os.WriteFile(filepath.Join(f.dir, "mf.db.mf", "pid.json"), []byte(fmt.Sprintf(`{"pid":%d,"instance":"stale"}`, os.Getpid())), 0600)
	f.must(t, "daemon", "start")
	f.must(t, "daemon", "stop")
}
func TestDaemonBusyStopResumeAndConcurrentAdd(t *testing.T) {
	f := newFixture(t)
	bin := filepath.Join(f.dir, "bin")
	os.Mkdir(bin, 0700)
	script := `#!/bin/sh
case "$*" in
  *slow.wav*) if [ ! -f "$MF_TEST_GATE" ]; then echo started > "$MF_TEST_STARTED"; exec sleep 30; fi ;;
esac
printf '%s' '{"streams":[{"index":0,"codec_type":"audio","codec_name":"pcm_s16le","sample_rate":"44100","channels":1}],"format":{"duration":"1.0"}}'
`
	if e := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	gate := filepath.Join(f.dir, "resume")
	started := filepath.Join(f.dir, "started")
	f.env = append(f.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "MF_TEST_GATE="+gate, "MF_TEST_STARTED="+started)
	for _, name := range []string{"slow.wav", "fast.wav"} {
		if e := os.WriteFile(filepath.Join(f.dir, name), []byte("fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	f.must(t, "daemon", "start")
	b := f.request(t, "POST", "/api/v1/jobs", `{"flow":"import_media","input":{"path":"slow.wav"}}`, 202)
	var submitted struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(b, &submitted); e != nil {
		t.Fatal(e)
	}
	waitUntil(t, func() bool { _, e := os.Stat(started); return e == nil })
	// The direct CLI can write to the same catalog while a queued job is running.
	out := f.must(t, "add", "fast.wav")
	if !strings.Contains(out, "Added: 1") {
		t.Fatal(out)
	}
	if out, e := f.run("daemon", "stop"); e == nil || !strings.Contains(out, "(1) jobs running") {
		t.Fatalf("busy stop: %v %s", e, out)
	}
	f.request(t, "GET", "/healthz", "", 200)
	f.must(t, "daemon", "stop", "-f")
	if e := os.WriteFile(gate, []byte("go"), 0600); e != nil {
		t.Fatal(e)
	}
	f.must(t, "daemon", "start")
	waitUntil(t, func() bool {
		var j jobs.Job
		b := f.request(t, "GET", "/api/v1/jobs/"+submitted.ID, "", 200)
		if e := json.Unmarshal(b, &j); e != nil {
			t.Fatal(e)
		}
		if j.State == "failed" {
			t.Fatalf("resume failed: %+v", j)
		}
		return j.State == "completed"
	})
	var catalog struct {
		Items []any `json:"items"`
	}
	if e := json.Unmarshal(f.request(t, "GET", "/api/v1/media", "", 200), &catalog); e != nil || len(catalog.Items) != 2 {
		t.Fatalf("catalog: %+v %v", catalog, e)
	}
	f.must(t, "daemon", "stop")
}
func TestStartupFailuresAndForegroundInterrupt(t *testing.T) {
	f := newFixture(t)
	c, e := config.Load(filepath.Join(f.dir, "mf.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", c.Address())
	if e != nil {
		t.Fatal(e)
	}
	if out, e := f.run("daemon", "start"); e == nil || !strings.Contains(out, "before readiness") || !strings.Contains(out, "cannot bind") {
		t.Fatalf("occupied port: %v %s", e, out)
	}
	listener.Close()
	if out := f.must(t, "daemon", "logs"); !strings.Contains(out, "cannot bind") {
		t.Fatal(out)
	}
	// A failed state write must never be reported as a ready daemon.
	pidPath := filepath.Join(f.dir, "mf.db.mf", "pid.json")
	if e := os.Mkdir(pidPath, 0700); e != nil {
		t.Fatal(e)
	}
	if out, e := f.run("daemon", "start"); e == nil || !strings.Contains(out, "before readiness") {
		t.Fatalf("failed readiness: %v %s", e, out)
	}
	if e := os.Remove(pidPath); e != nil {
		t.Fatal(e)
	}
	capture, e := os.Create(filepath.Join(f.dir, "foreground.log"))
	if e != nil {
		t.Fatal(e)
	}
	defer capture.Close()
	cmd := exec.Command(testBinary, "server", "start")
	cmd.Dir = f.dir
	cmd.Stdout = capture
	cmd.Stderr = capture
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer cmd.Process.Kill()
	waitUntil(t, func() bool { b, _ := os.ReadFile(capture.Name()); return bytes.Contains(b, []byte("server ready")) })
	f.request(t, "GET", "/healthz", "", 200)
	if e = cmd.Process.Signal(syscall.SIGINT); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("foreground did not exit")
	}
	path := filepath.Join(f.dir, "mf.yaml")
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	b = bytes.ReplaceAll(b, []byte("127.0.0.1"), []byte("192.0.2.1"))
	os.WriteFile(path, b, 0600)
	if out, e := f.run("server", "start"); e == nil || !strings.Contains(out, "cannot bind") {
		t.Fatalf("invalid bind: %v %s", e, out)
	}
}

func TestClientCLIAnnotationsRenderingAndPathJobs(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("FFmpeg unavailable")
	}
	f := newFixture(t)
	audio := filepath.Join(f.dir, "audio.wav")
	if out, e := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "pcm_s16le", audio).CombinedOutput(); e != nil {
		t.Fatalf("fixture: %v %s", e, out)
	}
	definition := filepath.Join(f.dir, "confidence.json")
	if e := os.WriteFile(definition, []byte(`{"key":"confidence","version":1,"data_type":"number","format":{"range":[0,1]}}`), 0600); e != nil {
		t.Fatal(e)
	}
	f.must(t, "tagtypes", "add", definition)
	segment := strings.TrimSpace(f.must(t, "segments", "add", audio, "00:00:00.1", "00:00:00.6"))
	tag := strings.TrimSpace(f.must(t, "tags", "add", "confidence@1", "--segment", segment, "--value", "0.9"))
	if !strings.Contains(f.must(t, "tags", "show", tag, "--json"), `"tag_type_key": "confidence"`) {
		t.Fatal("wrong tag terminology")
	}
	rendered := filepath.Join(f.dir, "render.mp3")
	out := f.must(t, "sql-to-ffmpeg", "SELECT * FROM segments ORDER BY id", "--output", rendered, "--print-only")
	if !strings.Contains(out, "libmp3lame") {
		t.Fatalf("wrong command %s", out)
	}
	if _, e := os.Stat(rendered); !os.IsNotExist(e) {
		t.Fatal("print-only rendered output")
	}
	f.must(t, "sql-to-ffmpeg", "SELECT * FROM segments ORDER BY id", "--output", rendered)
	f.must(t, "programs", "link", "echo", "/bin/echo")
	if out = f.must(t, "programs", "exec", "echo", "--", "passthrough"); strings.TrimSpace(out) != "passthrough" {
		t.Fatalf("program passthrough %s", out)
	}
	// This starts the daemon implicitly, and compiled imports remain path-filterable.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	jobCmd := exec.CommandContext(ctx, testBinary, "jobs", "run", "import_media", audio, "--wait", "--json")
	jobCmd.Dir = f.dir
	jobCmd.Env = f.env
	var diagnostics bytes.Buffer
	jobCmd.Stderr = &diagnostics
	jobOutput, err := jobCmd.Output()
	if err != nil {
		t.Fatalf("job submission: %v\n%s", err, diagnostics.String())
	}
	out = string(jobOutput)
	var completed jobs.Job
	if err := json.Unmarshal([]byte(out), &completed); err != nil || completed.State != "completed" {
		t.Fatalf("job %s", out)
	}
	out = f.must(t, "jobs", "list", audio)
	if !strings.Contains(out, "FLOW") || !strings.Contains(out, "import_media") || json.Valid([]byte(out)) {
		t.Fatalf("path jobs %s", out)
	}
	f.must(t, "daemon", "stop")
	f.must(t, "migrate")
}
