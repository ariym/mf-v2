package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/store"
)

func TestSegmentSubmissionAndCopyableV2Definitions(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.SQLitePath = filepath.Join(t.TempDir(), "mf.db")
	s, err := store.Init(cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	duration := 10.0
	if _, err = s.Upsert(ctx, store.Media{FilePath: "/catalog/source.mp4", MediaType: "video", Duration: &duration, FileSize: 1}); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := hub.Flow{Key: "targets", Version: 1, Title: "Targets", Desc: "Test", GHRepo: "https://github.com/example/flow", Execution: &hub.Execution{ProtocolVersion: 2}, Artifacts: []hub.Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://example.com/flow.tar.gz", SHA256: strings.Repeat("a", 64), Entrypoint: "flow"}}}
	raw, _ := json.Marshal(f)
	if err = s.RegisterInstall(ctx, nil, []store.Module{{Kind: hub.Flows, Key: f.Key, Version: 1, Manifest: raw, Source: "test", Executable: executable}}, "", "test"); err != nil {
		t.Fatal(err)
	}
	engine, err := jobs.New(s, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Abort()
	handler := API(cfg, s, engine)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	segment, err := s.AddSegment(ctx, 1, "00:00:01", "00:00:02")
	if err != nil {
		t.Fatal(err)
	}
	w := request("POST", "/api/v1/jobs", fmt.Sprintf(`{"flow":"targets","segment_ids":[%d],"input":{}}`, segment))
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if err = s.UpdateSegment(ctx, segment, "00:00:02", "00:00:03"); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/v1/jobs/"+created.ID, "")
	var job jobs.Job
	if err = json.Unmarshal(w.Body.Bytes(), &job); err != nil || len(job.Targets) != 1 || job.Targets[0].Start != "00:00:01.000000" {
		t.Fatalf("target changed %s %v", w.Body, err)
	}
	id, err := engine.Submit(ctx, "targets", json.RawMessage(`{"media":["/catalog/source.mp4"]}`))
	if err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/v1/jobs/"+id+"/submission", "")
	var copied struct {
		Data struct {
			Kind     string
			MediaIDs []int64 `json:"media_ids"`
		}
		Input map[string]json.RawMessage
	}
	if err = json.Unmarshal(w.Body.Bytes(), &copied); err != nil || copied.Data.Kind != "media" || len(copied.Data.MediaIDs) != 1 {
		t.Fatalf("bad copy %s %v", w.Body, err)
	}
	if _, ok := copied.Input["media"]; ok {
		t.Fatal("copy conflicts with catalog selector")
	}
	w = request("POST", "/api/v1/jobs", fmt.Sprintf(`{"flow":"targets","segment_ids":[%d],"paths":[],"input":{}}`, segment))
	if w.Code != 400 {
		t.Fatalf("mixed selectors accepted %d", w.Code)
	}
}
