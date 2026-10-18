package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariym/mf-v2/flows"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/cschleiden/go-workflows/workflow"
)

func newTestEngine(t *testing.T, s *store.Store) *Engine {
	t.Helper()
	e, err := New(s, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func stopEngine(t *testing.T, e *Engine) {
	t.Helper()
	e.Abort()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func waitJob(t *testing.T, e *Engine, id, state string) Job {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		j, err := e.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == state {
			return j
		}
		if j.State == "failed" && state != "failed" {
			t.Fatalf("job failed: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not become %s", state)
	return Job{}
}
func TestRetriesAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mf.db")
	s, err := store.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	var attempts atomic.Int32
	if err = e.Registry.RegisterActivity("flaky", func(context.Context) (string, error) {
		if attempts.Add(1) < 3 {
			return "", errors.New("transient")
		}
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = e.Registry.RegisterFlow("retry", func(ctx workflow.Context, _ json.RawMessage) (any, error) {
		opts := flows.ActivityOptions()
		opts.RetryOptions.FirstRetryInterval = 10 * time.Millisecond
		return workflow.ExecuteActivity[string](ctx, opts, "flaky").Get(ctx)
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := e.Submit(context.Background(), "retry", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, e, id, "completed")
	if j.Result != "ok" || attempts.Load() != 3 {
		t.Fatalf("retry: %+v attempts=%d", j, attempts.Load())
	}
	stopEngine(t, e)
	s.Close()
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e2 := newTestEngine(t, s)
	defer e2.Abort()
	j, err = e2.Get(context.Background(), id)
	if err != nil || j.State != "completed" || j.Result != "ok" {
		t.Fatalf("persisted result: %+v %v", j, err)
	}
}
func TestBusyStopAndInterruptedActivityResumes(t *testing.T) {
	s, err := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var allowed atomic.Bool
	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	register := func(e *Engine) {
		if err := e.Registry.RegisterActivity("interruptible", func(ctx context.Context) (string, error) {
			calls.Add(1)
			entered <- struct{}{}
			if allowed.Load() {
				return "resumed", nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Registry.RegisterFlow("resume", func(ctx workflow.Context, _ json.RawMessage) (any, error) {
			return workflow.ExecuteActivity[string](ctx, flows.ActivityOptions(), "interruptible").Get(ctx)
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestEngine(t, s)
	register(e)
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := e.Submit(context.Background(), "resume", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("activity did not start")
	}
	if n, err := e.RequestStop(context.Background(), false); err != nil || n != 1 {
		t.Fatalf("busy stop: %d %v", n, err)
	}
	if n, err := e.RequestStop(context.Background(), true); err != nil || n != 0 {
		t.Fatalf("forced stop: %d %v", n, err)
	}
	stopEngine(t, e)
	if _, err = e.Submit(context.Background(), "resume", json.RawMessage(`{}`)); !errors.Is(err, ErrStopping) {
		t.Fatalf("accepted after stopping: %v", err)
	}
	allowed.Store(true)
	e2 := newTestEngine(t, s)
	register(e2)
	if err = e2.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e2)
	j := waitJob(t, e2, id, "completed")
	if j.Result != "resumed" || calls.Load() != 2 {
		t.Fatalf("resume: %+v calls=%d", j, calls.Load())
	}
}
func TestQueuedJobsSurviveStop(t *testing.T) {
	s, err := store.Init(filepath.Join(t.TempDir(), "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := newTestEngine(t, s)
	if err = e.Registry.RegisterFlow("simple", func(workflow.Context, json.RawMessage) (any, error) { return "done", nil }, nil); err != nil {
		t.Fatal(err)
	}
	id, err := e.Submit(context.Background(), "simple", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Get(context.Background(), id)
	if err != nil || j.State != "queued" {
		t.Fatalf("queued state: %+v %v", j, err)
	}
	if n, err := e.RequestStop(context.Background(), false); err != nil || n != 0 {
		t.Fatalf("queued job prevented stop: %d %v", n, err)
	}
	e2 := newTestEngine(t, s)
	if err = e2.Registry.RegisterFlow("simple", func(workflow.Context, json.RawMessage) (any, error) { return "done", nil }, nil); err != nil {
		t.Fatal(err)
	}
	if err = e2.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e2)
	waitJob(t, e2, id, "completed")
}
