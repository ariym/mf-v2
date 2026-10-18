package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/cschleiden/go-workflows/workflow"
)

func TestAPI(t *testing.T) {
	s, e := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	engine, e := jobs.New(s, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	if e = engine.Registry.RegisterFlow("echo", func(_ workflow.Context, input json.RawMessage) (any, error) { return string(input), nil }, nil); e != nil {
		t.Fatal(e)
	}
	if e = engine.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		engine.Abort()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if e := engine.Wait(ctx); e != nil {
			t.Error(e)
		}
	}()
	api := API(config.Defaults(), s, engine)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/healthz", "", 200}, {"GET", "/api/v1/media", "", 200}, {"GET", "/api/v1/media?limit=0", "", 400}, {"GET", "/api/v1/media/999", "", 404}, {"GET", "/api/v1/media/nope", "", 400}, {"GET", "/api/v1/jobs/missing", "", 404}, {"POST", "/api/v1/jobs", `{"flow":"unknown","input":{}}`, 400}, {"POST", "/api/v1/jobs", `{"flow":"import_media","input":{}}`, 400}, {"POST", "/api/v1/jobs", `{} {}`, 400}, {"POST", "/api/v1/jobs", `{`, 400}, {"GET", "/nope", "", 404}} {
		w := request(tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Fatal("non JSON response")
		}
	}
	w := request("POST", "/api/v1/jobs", `{"flow":"echo","input":{"hello":"world"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &created); e != nil {
		t.Fatal(e)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		w = request("GET", "/api/v1/jobs/"+created.ID, "")
		var j jobs.Job
		if e = json.Unmarshal(w.Body.Bytes(), &j); e != nil {
			t.Fatal(e)
		}
		if j.State == "completed" {
			return
		}
		if j.State == "failed" {
			t.Fatalf("job failed: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("API job did not finish")
}
