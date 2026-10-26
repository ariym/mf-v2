package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/store"
)

func TestRequestAPIApprovalBoundary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Init(filepath.Join(root, "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(root, "media.wav")
	if err = os.WriteFile(path, []byte("cached source"), 0600); err != nil {
		t.Fatal(err)
	}
	duration := 1.0
	if _, err = s.Upsert(ctx, store.Media{FilePath: path, MediaType: "audio", FileSize: 13, Duration: &duration}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO tagtype(key,version,manifest,source,created_at,modified_at) VALUES('cached',1,'{"key":"cached","version":1,"data_type":"string"}','test','now','now')`); err != nil {
		t.Fatal(err)
	}
	seg, err := s.AddSegment(ctx, 1, "00:00:00", "00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddTag(ctx, "cached", 1, json.RawMessage(`"already available"`), []store.TagLink{{SegmentID: seg}}); err != nil {
		t.Fatal(err)
	}
	e, err := jobs.New(s, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Abort()
	handler := API(config.Config{}, s, e)
	call := func(method, path string, v any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(v)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(raw)))
		return w
	}
	w := call(http.MethodPost, "/api/v1/requests/plan", jobs.RequestInput{TagTypes: []string{"cached"}, SegmentIDs: []int64{seg}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var p jobs.RequestPlan
	if err = json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM media_requests").Scan(&count); err != nil || count != 0 {
		t.Fatal("preview created durable intent")
	}
	w = call(http.MethodPost, "/api/v1/requests", p)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	w = call(http.MethodPost, "/api/v1/requests", p)
	if w.Code != 202 {
		t.Fatal("idempotent acceptance failed", w.Body.String())
	}
	if err = s.DB.QueryRow("SELECT count(*) FROM media_requests").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate request")
	}
	w = call(http.MethodGet, "/api/v1/requests/"+p.ID, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call(http.MethodGet, "/api/v1/requests/missing", nil)
	if w.Code != 404 {
		t.Fatal("missing request status", w.Code)
	}
}
