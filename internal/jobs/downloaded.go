package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ariym/mf-v2/flows"
	"github.com/ariym/mf-v2/internal/executable"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/core"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"
)

const downloadedFlowName = "mf_downloaded_flow_v1"

type downloadedInput struct {
	JobID    string           `json:"job_id"`
	Input    json.RawMessage  `json:"input"`
	Snapshot modules.Snapshot `json:"snapshot"`
	Media    []store.Media    `json:"media"`
}
type protocolRequest struct {
	ProtocolVersion int                           `json:"protocol_version"`
	JobID           string                        `json:"job_id"`
	DBPath          string                        `json:"db_path"`
	Input           json.RawMessage               `json:"input"`
	Programs        map[string]modules.Dependency `json:"programs"`
	Flows           map[string]modules.Dependency `json:"flows"`
}
type protocolResponse struct {
	ProtocolVersion int             `json:"protocol_version"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
}

func downloadedWorkflow(ctx workflow.Context, raw json.RawMessage) (any, error) {
	var input downloadedInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	result, err := workflow.ExecuteActivity[json.RawMessage](ctx, flows.ActivityOptions(), "mf_execute_downloaded_v1", input).Get(ctx)
	if err != nil {
		return nil, err
	}
	return workflow.ExecuteActivity[json.RawMessage](ctx, flows.ActivityOptions(), "mf_ingest_downloaded_v1", input, result).Get(ctx)
}
func (e *Engine) registerDownloaded() error {
	if err := e.Registry.RegisterActivity("mf_execute_downloaded_v1", e.executeDownloaded); err != nil {
		return err
	}
	if err := e.Registry.RegisterActivity("mf_ingest_downloaded_v1", func(ctx context.Context, in downloadedInput, result json.RawMessage) (json.RawMessage, error) {
		if err := e.checkCancelled(ctx, in.JobID); err != nil {
			return nil, workflow.NewPermanentError(err)
		}
		if err := e.store.IngestResult(ctx, in.JobID, result, in.Snapshot.TagTypes, in.Media); err != nil {
			return nil, workflow.NewPermanentError(err)
		}
		return result, nil
	}); err != nil {
		return err
	}
	return e.Registry.RegisterFlow(downloadedFlowName, downloadedWorkflow, nil)
}
func (e *Engine) checkCancelled(ctx context.Context, id string) error {
	var cancelled bool
	if err := e.db.QueryRowContext(ctx, "SELECT cancel_requested FROM job_metadata WHERE id=?", id).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return fmt.Errorf("job cancelled")
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit   int
	onLimit context.CancelFunc
	failed  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.failed = true
		if b.onLimit != nil {
			b.onLimit()
		}
		return 0, fmt.Errorf("flow response exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}
func (e *Engine) executeDownloaded(ctx context.Context, in downloadedInput) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.activeMu.Lock()
	e.active[in.JobID] = cancel
	e.activeMu.Unlock()
	defer func() { e.activeMu.Lock(); delete(e.active, in.JobID); e.activeMu.Unlock() }()
	if err := e.checkCancelled(ctx, in.JobID); err != nil {
		return nil, workflow.NewPermanentError(err)
	}
	if err := modules.Executable(in.Snapshot.Root.Executable); err != nil {
		return nil, workflow.NewPermanentError(err)
	}
	request := protocolRequest{1, in.JobID, e.dbPath, in.Input, in.Snapshot.Programs, in.Snapshot.Flows}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, workflow.NewPermanentError(fmt.Errorf("flow request exceeds 1 MiB"))
	}
	dir := filepath.Join(e.dbPath+".mf", "jobs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, in.JobID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	fmt.Fprintf(log, "\n[%s] %s@%d\n", time.Now().UTC().Format(time.RFC3339Nano), in.Snapshot.Root.Key, in.Snapshot.Root.Version)
	cmd := exec.CommandContext(ctx, in.Snapshot.Root.Executable)
	cmd.Dir = e.base
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = log
	out := limitedBuffer{limit: 64 << 20, onLimit: cancel}
	cmd.Stdout = &out
	err = executable.Run(ctx, cmd)
	if out.failed {
		return nil, workflow.NewPermanentError(fmt.Errorf("flow response exceeds 64 MiB"))
	}
	if ctx.Err() != nil {
		if e.checkCancelled(context.Background(), in.JobID) != nil {
			_, _ = e.db.Exec("UPDATE job_metadata SET terminal_state='cancelled' WHERE id=? AND cancel_requested=1", in.JobID)
		}
		return nil, workflow.NewPermanentError(ctx.Err())
	}
	var response protocolResponse
	decodeErr := hub.DecodeStrict(out.Bytes(), &response)
	if err != nil {
		message := ""
		if decodeErr == nil {
			message = response.Error
		}
		return nil, workflow.NewPermanentError(fmt.Errorf("flow exited unsuccessfully: %w: %s (see mf jobs logs %s)", err, message, in.JobID))
	}
	if decodeErr != nil {
		return nil, workflow.NewPermanentError(fmt.Errorf("malformed flow response: %w", decodeErr))
	}
	if response.ProtocolVersion != 1 || response.Error != "" || len(response.Result) == 0 || !json.Valid(response.Result) {
		return nil, workflow.NewPermanentError(fmt.Errorf("invalid flow protocol response: %s", response.Error))
	}
	return response.Result, nil
}

func (e *Engine) SubmitPaths(ctx context.Context, name string, input json.RawMessage, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", &InputError{fmt.Errorf("paths must not be empty")}
	}
	if name == "import_media" {
		if len(paths) != 1 {
			return "", &InputError{fmt.Errorf("import_media accepts one file or directory")}
		}
		raw, _ := json.Marshal(ImportInput{paths[0]})
		return e.Submit(ctx, name, raw)
	}
	// Reuse the synchronous canonical-path importer, including its scan errors.
	seen := map[int64]bool{}
	var selected []store.Media
	var canonical []string
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.base, path)
		}
		abs, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", &InputError{err}
		}
		summary, importErr := (&media.Importer{Store: e.store, ExcludeDirs: e.excludeDirs}).Import(ctx, abs)
		if importErr != nil {
			if len(summary.Errors) > 0 {
				importErr = fmt.Errorf("%w: %s", importErr, strings.Join(summary.Errors[:min(5, len(summary.Errors))], "; "))
			}
			return "", &InputError{fmt.Errorf("%s: %w", abs, importErr)}
		}
		canonical = append(canonical, abs)
		for _, id := range summary.MediaIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			m, err := e.store.Get(ctx, id)
			if err != nil {
				return "", err
			}
			selected = append(selected, m)
		}
	}
	if len(selected) == 0 {
		return "", &InputError{fmt.Errorf("no media files found")}
	}
	var fields map[string]json.RawMessage
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if err := hub.DecodeStrict(input, &fields); err != nil || fields == nil {
		return "", &InputError{fmt.Errorf("input must be a JSON object")}
	}
	if _, ok := fields["media"]; ok {
		return "", &InputError{fmt.Errorf("input.media is supplied by paths; use jobs submit for raw input")}
	}
	mediaPaths := []string{}
	for _, m := range selected {
		mediaPaths = append(mediaPaths, m.FilePath)
	}
	fields["media"], _ = json.Marshal(mediaPaths)
	if _, ok := fields["output_dir"]; !ok {
		fields["output_dir"], _ = json.Marshal(filepath.Join(e.dbPath+".mf", "results"))
	} else {
		var dir string
		if err := json.Unmarshal(fields["output_dir"], &dir); err != nil || dir == "" {
			return "", &InputError{fmt.Errorf("input.output_dir must be a path")}
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(e.base, dir)
		}
		fields["output_dir"], _ = json.Marshal(dir)
	}
	var dir string
	_ = json.Unmarshal(fields["output_dir"], &dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(fields)
	if e.Registry.Has(name) {
		return e.Submit(ctx, name, raw)
	}
	return e.submitDownloaded(ctx, name, raw, selected, canonical)
}
func (e *Engine) submitDownloaded(ctx context.Context, name string, input json.RawMessage, media []store.Media, paths []string) (string, error) {
	if name == downloadedFlowName {
		return "", &InputError{fmt.Errorf("reserved flow name")}
	}
	ref, err := hub.ParseRef(hub.Flows, name)
	if err != nil {
		return "", &InputError{err}
	}
	snapshot, err := modules.Pin(ctx, e.store, ref)
	if err != nil {
		return "", &InputError{err}
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if err = hub.DecodeStrict(input, &object); err != nil || object == nil {
		return "", &InputError{fmt.Errorf("flow input must be a JSON object")}
	}
	// Raw input can still carry paths; use them to associate returned instances.
	if len(media) == 0 {
		var filePaths []string
		if raw, ok := object["media"]; ok {
			if err = json.Unmarshal(raw, &filePaths); err != nil {
				return "", &InputError{err}
			}
			for _, path := range filePaths {
				m, e := e.store.ByPath(ctx, path)
				if e != nil {
					return "", &InputError{fmt.Errorf("raw input media must already be imported: %w", e)}
				}
				media = append(media, m)
			}
		}
	}
	for _, p := range snapshot.ProgramManifests {
		for _, m := range media {
			if err = modules.Compatible(p, m); err != nil {
				return "", &InputError{err}
			}
		}
	}
	return e.persistDownloaded(ctx, downloadedInput{JobID: uuid.NewString(), Input: input, Snapshot: snapshot, Media: media}, paths)
}
func (e *Engine) persistDownloaded(ctx context.Context, in downloadedInput, paths []string) (string, error) {
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return "", ErrStopping
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	pathJSON, _ := json.Marshal(paths)
	if paths == nil {
		pathJSON = []byte(`[]`)
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Pin validation and metadata insertion share the write lock with uninstall.
	for _, r := range in.Snapshot.Entities {
		table := "tagtype"
		if r.Kind == "flows" {
			table = "flow"
		} else if r.Kind == "programs" {
			table = "program"
		}
		var version int
		if err = tx.QueryRowContext(ctx, "SELECT version FROM "+table+" WHERE key=?", r.Identifier).Scan(&version); err != nil {
			return "", err
		}
		if version != r.Version {
			return "", fmt.Errorf("installed version changed during submission")
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES(?,?,?,?,?,?,?)", in.JobID, in.Snapshot.Root.Key, in.Snapshot.Root.Version, string(in.Input), string(raw), string(pathJSON), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	for _, m := range in.Media {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO job_media(job_id,media_file_id) VALUES(?,?)", in.JobID, m.ID); err != nil {
			return "", err
		}
	}
	for _, r := range in.Snapshot.Entities {
		if _, err = tx.ExecContext(ctx, "INSERT INTO job_modules(job_id,kind,key,version) VALUES(?,?,?,?)", in.JobID, r.Kind, r.Identifier, r.Version); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if err = e.enqueue(ctx, in.JobID, raw); err != nil {
		return in.JobID, fmt.Errorf("job %s saved for recovery: %w", in.JobID, err)
	}
	return in.JobID, nil
}
func (e *Engine) enqueue(ctx context.Context, id string, raw []byte) error {
	var compiled compiledInput
	_ = json.Unmarshal(raw, &compiled)
	name := downloadedFlowName
	input := json.RawMessage(raw)
	if compiled.WorkflowName != "" {
		name = compiled.WorkflowName
		input = compiled.Input
	}
	var count int
	if err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM instances WHERE id=?", id).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := e.client.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: id}, name, input); err != nil {
			return err
		}
	}
	if compiled.WorkflowName != "" {
		var requested bool
		if err := e.db.QueryRowContext(ctx, "SELECT cancel_requested FROM job_metadata WHERE id=?", id).Scan(&requested); err != nil {
			return err
		}
		if requested {
			var execution string
			var state int
			if err := e.db.QueryRowContext(ctx, "SELECT execution_id,state FROM instances WHERE id=?", id).Scan(&execution, &state); err != nil {
				return err
			}
			if state == 0 {
				if err := e.client.CancelWorkflowInstance(ctx, core.NewWorkflowInstance(id, execution)); err != nil {
					return err
				}
			}
		}
	}
	_, err := e.db.ExecContext(ctx, "UPDATE job_metadata SET submitted=1 WHERE id=?", id)
	return err
}
func (e *Engine) reconcile(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, "SELECT id,snapshot FROM job_metadata WHERE submitted=0 OR (cancel_requested=1 AND terminal_state IS NULL)")
	if err != nil {
		return err
	}
	type entry struct {
		id  string
		raw []byte
	}
	var pending []entry
	for rows.Next() {
		var p entry
		if err = rows.Scan(&p.id, &p.raw); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err = e.enqueue(ctx, p.id, p.raw); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) pending(ctx context.Context, id string) (Job, error) {
	var j Job
	var paths string
	j.ID = id
	j.State = "queued"
	err := e.db.QueryRowContext(ctx, "SELECT flow_key,flow_version,paths,created_at,cancel_requested FROM job_metadata WHERE id=?", id).Scan(&j.Flow, &j.FlowVersion, &paths, &j.CreatedAt, &j.CancelRequested)
	if err == nil {
		err = json.Unmarshal([]byte(paths), &j.Paths)
	}
	return j, err
}
func (e *Engine) List(ctx context.Context, path, state string, limit, offset int) ([]Job, error) {
	if path != "" {
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			path = canonical
		}
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return nil, fmt.Errorf("invalid pagination")
	}
	rows, err := e.db.QueryContext(ctx, "SELECT id FROM instances WHERE parent_instance_id IS NULL UNION SELECT id FROM job_metadata")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Job{}
	for i := len(ids) - 1; i >= 0; i-- {
		j, err := e.Get(ctx, ids[i])
		if err != nil {
			return nil, err
		}
		if state != "" && j.State != state {
			continue
		}
		if path != "" {
			var count int
			for _, p := range j.Paths {
				rel, err := filepath.Rel(path, p)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					count++
				}
			}
			rows, err := e.db.QueryContext(ctx, "SELECT m.file_path FROM job_media jm JOIN media_files m ON m.id=jm.media_file_id WHERE jm.job_id=?", j.ID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var p string
				if err = rows.Scan(&p); err != nil {
					rows.Close()
					return nil, err
				}
				rel, err := filepath.Rel(path, p)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					count++
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if count == 0 {
				continue
			}
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	if offset >= len(out) {
		return []Job{}, nil
	}
	return out[offset:min(len(out), offset+limit)], nil
}
func (e *Engine) CancelJob(ctx context.Context, id string) error {
	job, err := e.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.State != "queued" && job.State != "running" {
		return fmt.Errorf("job is already %s", job.State)
	}
	var ingested bool
	err = e.db.QueryRowContext(ctx, "SELECT ingested FROM job_metadata WHERE id=?", id).Scan(&ingested)
	if err == nil {
		if ingested {
			return fmt.Errorf("job results have already committed")
		}
		result, updateErr := e.db.ExecContext(ctx, "UPDATE job_metadata SET cancel_requested=1 WHERE id=? AND ingested=0", id)
		err = updateErr
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return fmt.Errorf("job results have already committed")
		}
		e.activeMu.Lock()
		cancel := e.active[id]
		if cancel != nil {
			cancel()
		}
		e.activeMu.Unlock()
		var raw []byte
		if err = e.db.QueryRowContext(ctx, "SELECT snapshot FROM job_metadata WHERE id=?", id).Scan(&raw); err != nil {
			return err
		}
		var compiled compiledInput
		_ = json.Unmarshal(raw, &compiled)
		if compiled.WorkflowName != "" {
			var execution string
			if err = e.db.QueryRowContext(ctx, "SELECT execution_id FROM instances WHERE id=?", id).Scan(&execution); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			return e.client.CancelWorkflowInstance(ctx, core.NewWorkflowInstance(id, execution))
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var execution string
	if err = e.db.QueryRowContext(ctx, "SELECT execution_id FROM instances WHERE id=?", id).Scan(&execution); err != nil {
		return err
	}
	return e.client.CancelWorkflowInstance(ctx, core.NewWorkflowInstance(id, execution))
}
func (e *Engine) Retry(ctx context.Context, id string) (string, error) {
	j, err := e.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if j.State != "failed" && j.State != "cancelled" {
		return "", fmt.Errorf("only failed or cancelled jobs can be retried")
	}
	var raw, paths []byte
	if err = e.db.QueryRowContext(ctx, "SELECT snapshot,paths FROM job_metadata WHERE id=?", id).Scan(&raw, &paths); err != nil {
		return "", fmt.Errorf("retry requires a downloaded flow job: %w", err)
	}
	var in downloadedInput
	var compiled compiledInput
	if err = json.Unmarshal(raw, &compiled); err != nil {
		return "", err
	}
	if compiled.WorkflowName != "" {
		return e.Submit(ctx, compiled.WorkflowName, compiled.Input)
	}
	var p []string
	if err = json.Unmarshal(raw, &in); err != nil {
		return "", err
	}
	if err = json.Unmarshal(paths, &p); err != nil {
		return "", err
	}
	in.JobID = uuid.NewString()
	return e.persistDownloaded(ctx, in, p)
}

type compiledInput struct {
	WorkflowName string          `json:"workflow_name"`
	Input        json.RawMessage `json:"input"`
}

func (e *Engine) persistCompiled(ctx context.Context, id, name string, input json.RawMessage) (string, error) {
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return "", ErrStopping
	}
	var fields struct {
		Path  string   `json:"path"`
		Media []string `json:"media"`
	}
	_ = json.Unmarshal(input, &fields)
	paths := fields.Media
	if fields.Path != "" {
		paths = append(paths, fields.Path)
	}
	if paths == nil {
		paths = []string{}
	}
	for n, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.base, path)
		}
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			path = canonical
		}
		paths[n] = filepath.Clean(path)
	}
	raw, err := json.Marshal(compiledInput{name, input})
	if err != nil {
		return "", err
	}
	p, _ := json.Marshal(paths)
	_, err = e.db.ExecContext(ctx, "INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES(?,?,0,?,?,?,?)", id, name, string(input), string(raw), string(p), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	if err = e.enqueue(ctx, id, raw); err != nil {
		return id, fmt.Errorf("job %s saved for recovery: %w", id, err)
	}
	return id, nil
}
func (e *Engine) Logs(ctx context.Context, id string, offset int64) ([]byte, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, fmt.Errorf("invalid job ID")
	}
	if _, err := e.Get(ctx, id); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset must be nonnegative")
	}
	f, err := os.Open(filepath.Join(e.dbPath+".mf", "jobs", id+".log"))
	if os.IsNotExist(err) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
