package jobs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/kballard/go-shellquote"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
)

func TestRequestPlanAdmissionResultsAndCache(t *testing.T) {
	s, _, ids := graphFixture(t)
	defer s.Close()
	e := newTestEngine(t, s)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	ctx := context.Background()
	c := hub.Client{}
	in := RequestInput{TagTypes: []string{"answer"}, SegmentIDs: ids[:1], Input: json.RawMessage(`{"step":"consumer"}`)}
	plan, err := e.PlanRequest(ctx, c, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs) != 3 || plan.Jobs[0].Flow != "extract_audio" || plan.Jobs[1].Flow != "producer" || plan.Jobs[2].Flow != "consumer" {
		t.Fatalf("wrong ordered graph: %d nodes", len(plan.Jobs))
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM job_metadata").Scan(&count); err != nil || count != 0 {
		t.Fatalf("planning submitted jobs: %d %v", count, err)
	}
	id, err := e.AcceptRequest(ctx, c, plan)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.AcceptRequest(ctx, c, plan)
	if err != nil || again != id {
		t.Fatalf("idempotence: %s %v", again, err)
	}
	if err = e.reconcileRequests(ctx, c); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(12 * time.Second)
	var r MediaRequest
	for time.Now().Before(until) {
		r, err = e.GetRequest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "completed" || r.State == "failed" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if r.State != "completed" || len(r.Results) != 1 || len(r.Results[0].Tags) != 1 {
		t.Fatalf("request: %+v", r)
	}
	cached, err := e.PlanRequest(ctx, c, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cached.Jobs) != 0 || len(cached.Resolutions) != 0 {
		t.Fatalf("cached request scheduled work: %+v", cached)
	}
	cachedID, err := e.AcceptRequest(ctx, c, cached)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileRequests(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, err = e.GetRequest(ctx, cachedID)
	if err != nil || r.State != "completed" {
		t.Fatalf("cached result: %+v %v", r, err)
	}
}
func TestRequestRejectsStaleAndTamperedPlan(t *testing.T) {
	s, _, ids := graphFixture(t)
	defer s.Close()
	e := newTestEngine(t, s)
	defer e.Abort()
	ctx := context.Background()
	p, err := e.PlanRequest(ctx, hub.Client{}, RequestInput{TagTypes: []string{"answer"}, SegmentIDs: ids[:1]}, "")
	if err != nil {
		t.Fatal(err)
	}
	bad := p
	bad.Jobs = append([]RequestNode{}, p.Jobs...)
	bad.Jobs[0].Flow = "malicious"
	if _, err = e.AcceptRequest(ctx, hub.Client{}, bad); err == nil {
		t.Fatal("accepted tampered work")
	}
	seg, err := s.Segment(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddTag(ctx, "answer", 1, json.RawMessage(`0.5`), []store.TagLink{{SegmentID: seg.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptRequest(ctx, hub.Client{}, p); err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatalf("stale approval: %v", err)
	}
}

func TestRequestEmptyEventCoverage(t *testing.T) {
	s, _, ids := graphFixture(t)
	defer s.Close()
	ctx := context.Background()
	m, err := s.Module(ctx, hub.Flows, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	var f hub.Flow
	if err = json.Unmarshal(m.Manifest, &f); err != nil {
		t.Fatal(err)
	}
	f.Execution.AllowEmpty = []string{"answer"}
	m.Manifest = canonicalJSON(f)
	// Only the fixture declaration changes; production manifests remain immutable.
	if _, err = s.DB.Exec("UPDATE flow SET manifest=? WHERE key='consumer'", string(m.Manifest)); err != nil {
		t.Fatal(err)
	}
	seg, err := s.Segment(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddTag(ctx, "confidence", 1, json.RawMessage(`0.6`), []store.TagLink{{SegmentID: seg.ID}}); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	in := RequestInput{TagTypes: []string{"answer"}, SegmentIDs: ids[:1], Input: json.RawMessage(`{"omit":true}`)}
	p, err := e.PlanRequest(ctx, hub.Client{}, in, "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.AcceptRequest(ctx, hub.Client{}, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileRequests(ctx, hub.Client{}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		r, err := e.GetRequest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "failed" {
			t.Fatal(r.Error)
		}
		if r.State == "completed" {
			if len(r.Results) != 1 || len(r.Results[0].Tags) != 0 {
				t.Fatal("empty events not retained")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cached, err := e.PlanRequest(ctx, hub.Client{}, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cached.Jobs) != 0 || cached.Outputs[0].CoverageJobID == "" {
		t.Fatal("zero-event coverage not reused")
	}
}

func TestRequestAdmissionSharesConcurrentWork(t *testing.T) {
	s, _, ids := graphFixture(t)
	defer s.Close()
	e := newTestEngine(t, s)
	defer e.Abort()
	ctx := context.Background()
	in := RequestInput{TagTypes: []string{"answer"}, SegmentIDs: ids[:1]}
	a, err := e.PlanRequest(ctx, hub.Client{}, in, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.PlanRequest(ctx, hub.Client{}, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptRequest(ctx, hub.Client{}, a); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptRequest(ctx, hub.Client{}, b); err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileRequests(ctx, hub.Client{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM job_metadata").Scan(&count); err != nil || count != 3 {
		t.Fatalf("duplicated graph: %d %v", count, err)
	}
	r, err := e.GetRequest(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Plan.Jobs {
		if !n.Reused {
			t.Fatal("second request did not share work")
		}
	}
}

func TestRequestPreparationResumesFromReadiness(t *testing.T) {
	s, _, _ := graphFixture(t)
	defer s.Close()
	ctx := context.Background()
	e := newTestEngine(t, s)
	defer e.Abort()
	root := t.TempDir()
	marker := filepath.Join(root, "ready")
	tool := filepath.Join(root, "prepare-tool")
	code := "#!/bin/sh\ncase \"$1\" in\nready) test -f \"$2\";;\nprepare) printf x >> \"$2\";;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(tool, []byte(code), 0755); err != nil {
		t.Fatal(err)
	}
	prog := hub.Program{ProgramName: "prepared", Version: 1, Desc: "fixture", GitRepo: "https://example.com/prepared"}
	if err := s.RegisterInstall(ctx, nil, []store.Module{{Kind: hub.Programs, Key: "prepared", Version: 1, Executable: tool, Manifest: canonicalJSON(prog), Source: "test"}}, "", "test"); err != nil {
		t.Fatal(err)
	}
	p := RequestPlan{ID: uuid.NewString(), Preparation: []RequestPreparation{{Program: "prepared", Version: 1, Profile: "default", Spec: hub.Preparation{ReadyArgs: []string{"ready", marker}, PrepareArgs: []string{"prepare", marker}}}}}
	if _, err := s.DB.Exec("INSERT INTO media_requests VALUES(?,?,?,'setup','','now','now')", p.ID, p.ID, string(canonicalJSON(p))); err != nil {
		t.Fatal(err)
	}
	// Simulate a stop after provisioning but before the request's next checkpoint.
	if err := runPreparation(ctx, tool, []string{"prepare", marker}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.reconcileRequests(ctx, hub.Client{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("repeated preparation: %q %v", data, err)
	}
	r, err := e.GetRequest(ctx, p.ID)
	if err != nil || r.State != "completed" {
		t.Fatalf("request: %s %s %v", r.State, r.Error, err)
	}
}

func TestRequestInstallsAndPreparesFromEmptyCatalog(t *testing.T) {
	s, path := externalFixture(t, "request")
	ctx := context.Background()
	if _, err := s.DB.Exec("DELETE FROM flow;DELETE FROM tagtype"); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	archive := func(script string) []byte {
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		tw := tar.NewWriter(z)
		data := []byte(script)
		if err := tw.WriteHeader(&tar.Header{Name: "bin/tool", Mode: 0755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		z.Close()
		return b.Bytes()
	}
	payloads := map[string][]byte{"/flow.tar.gz": archive("#!/bin/sh\nexec " + shellquote.Join(exe, "-test.run=^TestDownloadedChild$") + "\n"), "/tool.tar.gz": archive("#!/bin/sh\ncase \"$1\" in\nready) test -f \"$0.ready\";;\nprepare) touch \"$0.ready\";;\n*) exit 1;;\nesac\n")}
	var f hub.Flow
	var prog hub.Program
	def := hub.TagType{Key: "confidence", Version: 1, DataType: "number"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := payloads[r.URL.Path]; ok {
			w.Write(data)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/flows":
			json.NewEncoder(w).Encode(hub.Page{Items: []json.RawMessage{canonicalJSON(f)}, Limit: 1000})
		case strings.HasPrefix(r.URL.Path, "/api/v1/tags/"):
			json.NewEncoder(w).Encode(def)
		case strings.HasPrefix(r.URL.Path, "/api/v1/programs/"):
			json.NewEncoder(w).Encode(prog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	artifact := func(name string) hub.Artifact {
		data := payloads[name]
		size := int64(len(data))
		return hub.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: server.URL + name, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Entrypoint: "bin/tool", DownloadBytes: &size}
	}
	prog = hub.Program{ProgramName: "tool", Version: 1, Desc: "fixture", GitRepo: "https://example.com/tool", Artifacts: []hub.Artifact{artifact("/tool.tar.gz")}, InputFormats: []hub.MediaInputFormat{{Container: ".wav"}}, Preparation: map[string]hub.Preparation{"default": {ReadyArgs: []string{"ready"}, PrepareArgs: []string{"prepare"}, DownloadBytes: 1}}}
	f = hub.Flow{Key: "produce", Version: 1, Title: "Produce", Desc: "fixture", GHRepo: "https://example.com/flow", Artifacts: []hub.Artifact{artifact("/flow.tar.gz")}, Programs: hub.Versions{"tool": 1}, TagTypes: hub.Versions{"confidence": 1}, Execution: &hub.Execution{ProtocolVersion: 2, Preparation: map[string]string{"tool": "default"}, Produces: []hub.DataSpec{{Kind: "tag", Key: "confidence", Version: 1}}}}
	c := hub.Client{URL: server.URL, HTTP: server.Client()}
	p, err := e.PlanRequest(ctx, c, RequestInput{TagTypes: []string{"confidence"}, Paths: []string{path}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Installations) != 2 || len(p.Preparation) != 1 || len(p.Jobs) != 1 {
		t.Fatalf("unexpected setup plan: %d installs %d preparations %d jobs", len(p.Installations), len(p.Preparation), len(p.Jobs))
	}
	id, err := e.AcceptRequest(ctx, c, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileRequests(ctx, c); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(10 * time.Second)
	var r MediaRequest
	for time.Now().Before(until) {
		r, err = e.GetRequest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "failed" || r.State == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.State != "completed" || len(r.Results) != 1 || len(r.Results[0].Tags) != 1 {
		t.Fatalf("clean request: %s %s", r.State, r.Error)
	}
}

func TestRequestRequiredOutputValidation(t *testing.T) {
	in := downloadedInput{RequestSources: map[string]string{"source": "digest"}, Targets: []store.Target{{}}, Snapshot: modules.Snapshot{Root: store.Module{Key: "flow"}, FlowManifests: map[string]hub.Flow{"flow": {Execution: &hub.Execution{ProtocolVersion: 2, Produces: []hub.DataSpec{{Kind: "tag", Key: "transcript", Version: 1}}}}}}}
	if err := validateRequestedOutputs(in, json.RawMessage(`{"tags":[]}`)); err == nil {
		t.Fatal("accepted missing transcript")
	}
	if err := validateRequestedOutputs(in, json.RawMessage(`{"tags":[{"key":"transcript","version":1,"target_index":0,"value":{}}]}`)); err != nil {
		t.Fatal(err)
	}
	spec := in.Snapshot.FlowManifests["flow"].Execution
	spec.AllowEmpty = []string{"transcript"}
	if err := validateRequestedOutputs(in, json.RawMessage(`{"tags":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestRequestDirectorySkipsNonMedia(t *testing.T) {
	s, path, _ := graphFixture(t)
	defer s.Close()
	e := newTestEngine(t, s)
	defer e.Abort()
	folder := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(folder, "notes.txt"), []byte("not media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(folder, "alias.wav")); err != nil {
		t.Fatal(err)
	}
	p, err := e.PlanRequest(context.Background(), hub.Client{}, RequestInput{TagTypes: []string{"answer"}, Paths: []string{folder}}, "")
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(path)
	if len(p.Outputs) != 1 || p.Outputs[0].Target.MediaPath != canonical {
		t.Fatalf("directory duplicated or included nonmedia: %d", len(p.Outputs))
	}
}

func TestRequestFlowNameDoesNotShadowBuiltin(t *testing.T) {
	s, _, ids := graphFixture(t)
	defer s.Close()
	ctx := context.Background()
	m, err := s.Module(ctx, hub.Flows, "consumer")
	if err != nil {
		t.Fatal(err)
	}
	var f hub.Flow
	if err = json.Unmarshal(m.Manifest, &f); err != nil {
		t.Fatal(err)
	}
	f.Key = "extract_audio"
	m.Key = f.Key
	m.Manifest = canonicalJSON(f)
	if err = s.RegisterInstall(ctx, nil, []store.Module{m}, "", "test"); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	defer e.Abort()
	p, err := e.PlanRequest(ctx, hub.Client{}, RequestInput{TagTypes: []string{"answer"}, SegmentIDs: ids[:1], Flow: "extract_audio@1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	root := p.Jobs[len(p.Jobs)-1]
	if root.Data.Builtin != "" || root.Data.Snapshot.Root.Key != "extract_audio" {
		t.Fatal("ordinary flow was replaced with builtin extraction")
	}
}
