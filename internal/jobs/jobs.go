// Package jobs adapts the embedded engine to API jobs and coordinated shutdown.
package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ariym/mf-v2/flows"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/history"
	wsqlite "github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/core"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"
)

var ErrStopping = errors.New("server is stopping")

type Engine struct {
	RequestHubURL string
	gate          *gate
	Worker        *worker.Worker
	Registry      *flows.Registry
	client        *client.Client
	db            *sql.DB
	cancel        context.CancelFunc
	done          chan error
	store         *store.Store
	base          string
	dbPath        string
	excludeDirs   []string
	activeMu      sync.Mutex
	active        map[string]context.CancelFunc
}
type gate struct {
	backend.Backend
	mu       sync.Mutex
	stopping bool
	db       *sql.DB
}

// Holding this mutex across fetch/checkpoint and stop prevents admission races.
func (g *gate) GetWorkflowTask(ctx context.Context, q []workflow.Queue) (*backend.WorkflowTask, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return nil, nil
	}
	return g.Backend.GetWorkflowTask(ctx, q)
}
func (g *gate) GetActivityTask(ctx context.Context, q []workflow.Queue) (*backend.ActivityTask, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return nil, nil
	}
	return g.Backend.GetActivityTask(ctx, q)
}
func (g *gate) CompleteActivityTask(ctx context.Context, t *backend.ActivityTask, e *history.Event) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return ErrStopping
	}
	return g.Backend.CompleteActivityTask(ctx, t, e)
}
func (g *gate) CompleteWorkflowTask(ctx context.Context, t *backend.WorkflowTask, s core.WorkflowInstanceState, e, a, tm []*history.Event, we []*history.WorkflowEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return ErrStopping
	}
	return g.Backend.CompleteWorkflowTask(ctx, t, s, e, a, tm, we)
}

// New requires the caller to hold the exclusive database server lock.
func New(s *store.Store, base string, logger *slog.Logger, moduleRoots ...string) (*Engine, error) {
	// No previous server can still own these leases while the process lock is held.
	if _, e := s.DB.Exec("UPDATE instances SET locked_until=NULL,sticky_until=NULL; UPDATE activities SET locked_until=NULL;"); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := wsqlite.NewSqliteBackendWithDB(s.DB, wsqlite.WithBackendOptions(backend.WithLogger(logger), backend.WithStickyTimeout(0)))
	g := &gate{Backend: b, db: s.DB}
	opts := worker.DefaultOptions
	opts.MaxParallelActivityTasks = 1
	opts.MaxParallelWorkflowTasks = 1
	opts.ActivityPollers = 1
	opts.WorkflowPollers = 1
	w := worker.New(g, &opts)
	r := flows.New(w, ctx)
	engine := &Engine{gate: g, Worker: w, Registry: r, client: client.New(g), db: s.DB, cancel: cancel, done: make(chan error, 1)}
	engine.store = s
	engine.base = base
	engine.excludeDirs = moduleRoots
	if len(engine.excludeDirs) == 0 {
		engine.excludeDirs = []string{filepath.Join(base, "mf_modules")}
	}
	engine.active = map[string]context.CancelFunc{}
	var seq int
	var dbName string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &dbName, &engine.dbPath); err != nil {
		cancel()
		return nil, err
	}
	if err := engine.registerDownloaded(); err != nil {
		cancel()
		return nil, err
	}
	importer := &media.Importer{Store: s, ExcludeDirs: engine.excludeDirs}
	if e := r.RegisterActivity("import_media_files", func(ctx context.Context, input ImportInput) (media.Summary, error) {
		summary, e := importer.Import(ctx, input.Path)
		if e != nil {
			var partial *media.PartialError
			if errors.As(e, &partial) || errors.Is(e, os.ErrNotExist) || errors.Is(e, os.ErrPermission) {
				if len(summary.Errors) > 0 {
					e = fmt.Errorf("%w: %s", e, strings.Join(summary.Errors[:min(5, len(summary.Errors))], "; "))
				}
				return summary, workflow.NewPermanentError(e)
			}
		}
		return summary, e
	}); e != nil {
		cancel()
		return nil, e
	}
	if e := r.RegisterFlow("import_media", ImportMedia, func(raw json.RawMessage) (json.RawMessage, error) {
		var input ImportInput
		if e := json.Unmarshal(raw, &input); e != nil {
			return nil, e
		}
		if input.Path == "" {
			return nil, fmt.Errorf("input.path is required")
		}
		if !filepath.IsAbs(input.Path) {
			input.Path = filepath.Join(base, input.Path)
		}
		return json.Marshal(input)
	}); e != nil {
		cancel()
		return nil, e
	}
	return engine, nil
}

type ImportInput struct {
	Path string `json:"path"`
}

func ImportMedia(ctx workflow.Context, raw json.RawMessage) (any, error) {
	var input ImportInput
	if e := json.Unmarshal(raw, &input); e != nil {
		return nil, e
	}
	return workflow.ExecuteActivity[media.Summary](ctx, flows.ActivityOptions(), "import_media_files", input).Get(ctx)
}
func (e *Engine) Start() error {
	// Use the registry lifetime context through a separate poller context; Stop cancels both.
	ctx, cancel := context.WithCancel(context.Background())
	old := e.cancel
	e.cancel = func() { cancel(); old() }
	if err := e.reconcile(context.Background()); err != nil {
		return err
	}
	if err := e.Worker.Start(ctx); err != nil {
		e.cancel()
		return err
	}
	if err := e.dispatchGraph(ctx); err != nil {
		e.cancel()
		return err
	}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		if e.RequestHubURL == "" {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.reconcileRequests(ctx, hub.Client{URL: e.RequestHubURL}); err != nil && ctx.Err() == nil {
					slog.Error("request reconciliation", "error", err)
				}
			}
		}
	}()
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.dispatchGraph(ctx); err != nil && ctx.Err() == nil {
					slog.Error("prerequisite reconciliation", "error", err)
				}
			}
		}
	}()
	go func() { err := e.Worker.WaitForCompletion(); cancel(); <-schedulerDone; <-requestDone; e.done <- err }()
	return nil
}
func (e *Engine) Submit(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if name == downloadedFlowName || name == downloadedFlowV2 || name == extractAudioFlow {
		return "", &InputError{fmt.Errorf("reserved flow name")}
	}
	if !e.Registry.Has(name) {
		return e.submitDownloaded(ctx, name, input, nil, nil)
	}
	normalized, err := e.Registry.Prepare(name, input)
	if err != nil {
		return "", &InputError{err}
	}
	return e.persistCompiled(ctx, uuid.NewString(), name, normalized)
}

type InputError struct{ error }

const startedPredicate = "(i.worker IS NOT NULL OR EXISTS(SELECT 1 FROM history h WHERE h.instance_id=i.id AND h.execution_id=i.execution_id))"

func (e *Engine) RequestStop(ctx context.Context, force bool) (int, error) {
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return 0, nil
	}
	var count int
	if err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM instances i WHERE i.state=0 AND i.parent_instance_id IS NULL AND "+startedPredicate).Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 && !force {
		return count, nil
	}
	e.gate.stopping = true
	e.cancel()
	return 0, nil
}
func (e *Engine) Wait(ctx context.Context) error {
	select {
	case err := <-e.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *Engine) Abort() { e.gate.mu.Lock(); e.gate.stopping = true; e.cancel(); e.gate.mu.Unlock() }

type Job struct {
	Targets         []store.Target `json:"targets,omitempty"`
	PrerequisiteIDs []string       `json:"prerequisite_ids,omitempty"`
	WaitingReason   string         `json:"waiting_reason,omitempty"`
	ID              string         `json:"id"`
	State           string         `json:"state"`
	Result          any            `json:"result,omitempty"`
	Error           string         `json:"error,omitempty"`
	CreatedAt       string         `json:"created_at"`
	CompletedAt     *string        `json:"completed_at"`
	Flow            string         `json:"flow,omitempty"`
	FlowVersion     int            `json:"flow_version,omitempty"`
	Paths           []string       `json:"paths,omitempty"`
	CancelRequested bool           `json:"cancel_requested,omitempty"`
}

func (e *Engine) Get(ctx context.Context, id string) (Job, error) {
	j := Job{ID: id}
	var execution string
	var state int
	var started bool
	err := e.db.QueryRowContext(ctx, "SELECT execution_id,state,created_at,completed_at,"+startedPredicate+" FROM instances i WHERE id=? AND parent_instance_id IS NULL ORDER BY created_at DESC LIMIT 1", id).Scan(&execution, &state, &j.CreatedAt, &j.CompletedAt, &started)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.pending(ctx, id)
		}
		return j, err
	}
	j.State = "queued"
	if started {
		j.State = "running"
	}
	if state == int(core.WorkflowInstanceStateFinished) || state == int(core.WorkflowInstanceStateContinuedAsNew) {
		j.State = "completed"
		var raw json.RawMessage
		raw, err = client.GetWorkflowResult[json.RawMessage](ctx, e.client, core.NewWorkflowInstance(id, execution), time.Second)
		if err == nil {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			err = decoder.Decode(&j.Result)
		}
		if err != nil {
			j.State = "failed"
			if errors.Is(err, client.ErrWorkflowCanceled) {
				j.State = "cancelled"
			}
			j.Error = err.Error()
		}
	}
	var paths, terminal string
	metaErr := e.db.QueryRowContext(ctx, "SELECT flow_key,flow_version,paths,cancel_requested,coalesce(terminal_state,'') FROM job_metadata WHERE id=?", id).Scan(&j.Flow, &j.FlowVersion, &paths, &j.CancelRequested, &terminal)
	if metaErr == nil {
		if err = json.Unmarshal([]byte(paths), &j.Paths); err != nil {
			return j, err
		}
		if j.CancelRequested && (terminal == "cancelled" || j.State == "failed") {
			j.State = "cancelled"
		}
		if j.State == "completed" || j.State == "failed" || j.State == "cancelled" {
			_, err = e.db.ExecContext(ctx, "UPDATE job_metadata SET terminal_state=? WHERE id=?", j.State, id)
			if err != nil {
				return j, err
			}
		}
	} else if !errors.Is(metaErr, sql.ErrNoRows) {
		return j, metaErr
	}
	if err = e.graphJobFields(ctx, &j); err != nil {
		return j, err
	}
	return j, nil
}
