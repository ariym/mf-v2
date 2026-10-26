package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/cschleiden/go-workflows/workflow"
)

func TestWorkspaceQueriesAnnotationsAndDrafts(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.SQLitePath = filepath.Join(t.TempDir(), "mf.db")
	s, err := store.Init(cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	duration := 500000.0
	for _, m := range []store.Media{{FilePath: "/not-mounted-here/a.mp4", MediaType: "video", Duration: &duration, FileSize: 1}, {FilePath: "/not-mounted-here/a.wav", MediaType: "audio", Duration: &duration, FileSize: 2}} {
		if _, err = s.Upsert(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.AddTagType(ctx, hub.TagType{Key: "label", Version: 1, DataType: "string"}, "local"); err != nil {
		t.Fatal(err)
	}
	segment, err := s.AddSegment(ctx, 1, "123:00:00.100001", "123:00:02.100002")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := s.AddTag(ctx, "label", 1, json.RawMessage(`"hello"`), []store.TagLink{{SegmentID: segment}})
	if err != nil {
		t.Fatal(err)
	}
	handler := API(cfg, s, nil)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"mode":"segments","filters":{"tag_type":"label","value":"hello"}}`, 200},
		{`{"mode":"media","filters":{"media_ids":[2]}}`, 200},
		{`{"mode":"segments","sql":"SELECT media_file_id,id AS segment_id,start,end FROM segments ORDER BY id"}`, 200},
		{`{"mode":"media","sql":"SELECT 1 AS media_file_id UNION ALL SELECT 1 AS media_file_id"}`, 200},
		{`{"mode":"media","sql":"SELECT 2 AS media_file_id UNION ALL SELECT 1 AS media_file_id"}`, 200},
		{`{"mode":"media","sql":"SELECT 1 AS media_file_id WHERE 0"}`, 200},
		{`{"mode":"media","sql":"DELETE FROM media_files"}`, 400},
		{`{"mode":"media","sql":"SELECT 1 AS media_file_id; DELETE FROM media_files"}`, 400},
		{`{"mode":"media","sql":"SELECT 999 AS media_file_id"}`, 404},
		{`{"mode":"segments","sql":"SELECT 1 AS media_file_id, 0 AS start, 1 AS end"}`, 400},
		{`{"mode":"media","sql":"WITH RECURSIVE x(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM x WHERE n<1001) SELECT 1 AS media_file_id FROM x"}`, 400},
		{`{"mode":"media","mode":"segments","filters":{}}`, 400},
		{`{"mode":"media","sql":""}`, 400},
	} {
		w := call("POST", "/api/v1/query", tc.body)
		if w.Code != tc.status {
			t.Fatalf("query %s: HTTP %d: %s", tc.body, w.Code, w.Body)
		}
	}
	w := call("POST", "/api/v1/query", `{"mode":"segments","filters":{}}`)
	var result struct{ Items []playbackClip }
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Items) != 1 || result.Items[0].Start != "123:00:00.100001" {
		t.Fatalf("timecode round trip: %s %v", w.Body, err)
	}
	w = call("POST", "/api/v1/query", `{"mode":"media","sql":"SELECT 2 AS media_file_id UNION ALL SELECT 1 AS media_file_id UNION ALL SELECT 2 AS media_file_id"}`)
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Items) != 3 || result.Items[0].Media.ID != 2 || result.Items[1].Media.ID != 1 || result.Items[2].Media.ID != 2 {
		t.Fatalf("ordering lost: %s", w.Body)
	}
	for _, path := range []string{"/api/v1/schema", "/api/v1/capabilities", "/api/v1/media/1/annotations", "/api/v1/segments?media_file_id=1", "/api/v1/tags?media_file_id=1", "/api/v1/modules/tagtypes"} {
		if w = call("GET", path, ""); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	w = call("PATCH", "/api/v1/tags/"+jsonNumber(tag), `{"value":"updated"}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, kind := range []string{"manifests", "jobs"} {
		w = call("POST", "/api/v1/drafts/"+kind, `{"name":"Unfinished draft","document":"{ broken json"}`)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		var d store.Document
		if err = json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if d.Document != "{ broken json" {
			t.Fatal("lost draft text")
		}
		w = call("PUT", "/api/v1/drafts/"+kind+"/"+d.ID, `{"name":"Revised","document":"{}"}`)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		w = call("DELETE", "/api/v1/drafts/"+kind+"/"+d.ID, "")
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		if w = call("GET", "/api/v1/drafts/"+kind+"/"+d.ID, ""); w.Code != 404 {
			t.Fatal("deleted draft survived")
		}
	}
	if w = call("POST", "/api/v1/manifests/validate", `{"kind":"tagtypes","manifest":{"key":"x","version":1,"data_type":"number","format":{"range":[0,1]}}}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call("POST", "/api/v1/manifests/validate", `{"kind":"tagtypes","manifest":{"key":"x","version":1,"data_type":"json","format":{"schema":{"$ref":"https://example.com/schema"}}}}`); w.Code != 400 {
		t.Fatalf("external schema accepted: %s", w.Body)
	}
	// Context cancellation also interrupts expensive recursive SQL without changing the catalog.
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = selectClips(queryCtx, s, cfg.SQLitePath, selectionRequest{Mode: "media", SQL: "WITH RECURSIVE x(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM x WHERE n<1000000000) SELECT sum(n) AS media_file_id FROM x"})
	if err == nil {
		t.Fatal("query ignored deadline")
	}
}
func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
func TestSavedJobDefinitionCreatesIndependentRuns(t *testing.T) {
	cfg := config.Defaults()
	cfg.SQLitePath = filepath.Join(t.TempDir(), "mf.db")
	s, err := store.Init(cfg.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	engine, err := jobs.New(s, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Registry.RegisterFlow("echo", func(_ workflow.Context, raw json.RawMessage) (any, error) { return json.RawMessage(raw), nil }, nil); err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		engine.Abort()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = engine.Wait(ctx)
	}()
	d, err := s.SaveDocument(context.Background(), "jobs", store.Document{Name: "Echo", Document: `{"flow":"echo","data":{"kind":"raw"},"input":{"value":1}}`})
	if err != nil {
		t.Fatal(err)
	}
	handler := API(cfg, s, engine)
	ids := []string{}
	for range 2 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/drafts/jobs/"+d.ID+"/run", bytes.NewReader(nil)))
		if w.Code != 202 {
			t.Fatalf("run: %d %s", w.Code, w.Body)
		}
		var created struct{ ID string }
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		ids = append(ids, created.ID)
	}
	if ids[0] == ids[1] {
		t.Fatal("reused execution ID")
	}
	d.Document = `{"flow":"echo","data":{"kind":"raw"},"input":{"value":2}}`
	if _, err = s.SaveDocument(context.Background(), "jobs", d); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/jobs/"+ids[0]+"/submission", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":1`) {
		t.Fatalf("run snapshot changed: %s", w.Body)
	}
}
