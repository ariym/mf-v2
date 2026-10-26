package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
)

type MediaRequest struct {
	ID      string          `json:"id"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"`
	Plan    RequestPlan     `json:"plan"`
	Results []RequestResult `json:"results"`
}
type RequestResult struct {
	Target store.Target `json:"target"`
	Type   hub.DataSpec `json:"type"`
	Tags   []store.Tag  `json:"tags"`
}

// AcceptRequest verifies a freshly computed plan before recording durable intent.
// Repeated submissions of an approved fingerprint return the same request.
func (e *Engine) AcceptRequest(ctx context.Context, c hub.Client, approved RequestPlan) (string, error) {
	var existing string
	err := e.db.QueryRowContext(ctx, "SELECT id FROM media_requests WHERE fingerprint=?", approved.Fingerprint).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if approved.Fingerprint == "" || approved.Fingerprint != approved.digest() {
		return "", fmt.Errorf("invalid plan fingerprint")
	}
	frozen := approved.Request
	frozen.TagTypes = nil
	frozen.Producers = map[string]string{}
	for _, out := range approved.Outputs {
		ref := fmt.Sprintf("%s@%d", out.Type.Key, out.Type.Version)
		found := false
		for _, s := range frozen.TagTypes {
			found = found || s == ref
		}
		if !found {
			frozen.TagTypes = append(frozen.TagTypes, ref)
		}
	}
	for _, out := range approved.Outputs {
		for _, node := range approved.Jobs {
			if node.ID == out.JobID {
				frozen.Producers[out.Type.Key] = fmt.Sprintf("%s@%d", node.Flow, node.Version)
			}
		}
	}
	fresh, err := e.PlanRequest(ctx, c, frozen, approved.ID)
	if err != nil {
		return "", err
	}
	// Compare the actual work, keeping the user's original request spelling.
	fresh.Request = approved.Request
	fresh.Fingerprint = fresh.digest()
	if fresh.Fingerprint != approved.Fingerprint {
		return "", fmt.Errorf("plan changed; review a new plan before continuing")
	}
	if len(fresh.Blockers) > 0 || len(fresh.Candidates) > 0 {
		return "", fmt.Errorf("request has unresolved blockers or producer choices")
	}
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return "", ErrStopping
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = e.db.ExecContext(ctx, `INSERT INTO media_requests(id,fingerprint,plan,state,created_at,modified_at) VALUES(?,?,?,'setup',?,?) ON CONFLICT(fingerprint) DO NOTHING`, approved.ID, approved.Fingerprint, string(canonicalJSON(approved)), stamp, stamp)
	if err != nil {
		return "", err
	}
	return approved.ID, nil
}

func (e *Engine) GetRequest(ctx context.Context, id string) (MediaRequest, error) {
	r := MediaRequest{ID: id, Results: []RequestResult{}}
	var raw string
	err := e.db.QueryRowContext(ctx, "SELECT plan,state,error FROM media_requests WHERE id=?", id).Scan(&raw, &r.State, &r.Error)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(raw), &r.Plan); err != nil {
		return r, err
	}
	if r.State != "running" && r.State != "completed" {
		return r, nil
	}
	complete := true
	for _, n := range r.Plan.Jobs {
		j, x := e.Get(ctx, n.ID)
		if x != nil {
			return r, x
		}
		if j.State == "failed" || j.State == "cancelled" {
			r.State = "failed"
			r.Error = fmt.Sprintf("job %s %s: %s", j.ID, j.State, j.Error)
			break
		}
		if j.State != "completed" {
			complete = false
		}
	}
	if r.State == "running" && complete {
		r.State = "completed"
	}
	if r.State == "completed" || r.State == "failed" {
		_, err = e.db.ExecContext(ctx, "UPDATE media_requests SET state=?,error=?,modified_at=? WHERE id=?", r.State, r.Error, time.Now().UTC().Format(time.RFC3339Nano), id)
		if err != nil {
			return r, err
		}
	}
	if r.State == "completed" {
		for _, out := range r.Plan.Outputs {
			tags, x := e.store.Tags(ctx, out.Target.MediaFileID)
			if x != nil {
				return r, x
			}
			result := RequestResult{Target: out.Target, Type: out.Type, Tags: []store.Tag{}}
			for _, tag := range tags {
				if out.TagID != 0 && tag.ID != out.TagID {
					continue
				}
				sourceJob := out.JobID
				if sourceJob == "" {
					sourceJob = out.CoverageJobID
				}
				if sourceJob != "" && (tag.SourceJobID == nil || *tag.SourceJobID != sourceJob) {
					continue
				}
				if tag.Key != out.Type.Key || tag.Version != out.Type.Version {
					continue
				}
				include := false
				for _, link := range tag.Links {
					seg, x := e.store.Segment(ctx, link.SegmentID)
					if x != nil {
						return r, x
					}
					if seg.MediaFileID == out.Target.MediaFileID && seg.Start < out.Target.End && seg.End > out.Target.Start {
						include = true
					}
				}
				if include {
					result.Tags = append(result.Tags, tag)
				}
			}
			r.Results = append(r.Results, result)
		}
	}
	return r, nil
}

func (e *Engine) reconcileRequests(ctx context.Context, c hub.Client) error {
	rows, err := e.db.QueryContext(ctx, "SELECT id FROM media_requests WHERE state IN ('setup','running') ORDER BY created_at,id")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		r, err := e.GetRequest(ctx, id)
		if err != nil {
			return err
		}
		if r.State != "setup" {
			continue
		}
		err = e.setupRequest(ctx, c, &r.Plan)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if _, x := e.db.ExecContext(ctx, "UPDATE media_requests SET state='failed',error=?,modified_at=? WHERE id=?", err.Error(), time.Now().UTC().Format(time.RFC3339Nano), id); x != nil {
				return x
			}
		}
	}
	return nil
}
func (e *Engine) setupRequest(ctx context.Context, c hub.Client, p *RequestPlan) error {
	installer := modules.Installer{Store: e.store, Root: e.excludeDirs[0], Hub: c}
	for _, res := range p.Resolutions {
		needsInstall := false
		for _, ref := range res.Order {
			if ref.Kind == hub.TagTypes {
				_, err := e.store.Definition(ctx, ref.Identifier, ref.Version)
				needsInstall = needsInstall || err != nil
			} else {
				_, err := e.store.Module(ctx, ref.Kind, ref.Identifier)
				needsInstall = needsInstall || err != nil
			}
		}
		if !needsInstall {
			continue
		}
		if _, err := installer.InstallResolved(ctx, res); err != nil {
			return err
		}
	}
	logDir := filepath.Join(e.dbPath+".mf", "requests")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(logDir, p.ID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	for _, step := range p.Preparation {
		m, err := e.store.Module(ctx, hub.Programs, step.Program)
		if err != nil {
			return err
		}
		if m.Version != step.Version {
			return fmt.Errorf("preparation program version changed")
		}
		if preparationReady(ctx, m.Executable, step.Spec.ReadyArgs, log) == nil {
			continue
		}
		for _, name := range step.Spec.RequiredEnv {
			if os.Getenv(name) == "" {
				return fmt.Errorf("%s preparation requires %s", step.Program, name)
			}
		}
		if err = runPreparation(ctx, m.Executable, step.Spec.PrepareArgs, log); err != nil {
			return fmt.Errorf("prepare %s/%s: %w (see %s)", step.Program, step.Profile, err, log.Name())
		}
		if err = preparationReady(ctx, m.Executable, step.Spec.ReadyArgs, log); err != nil {
			return fmt.Errorf("%s preparation readiness check failed: %w", step.Program, err)
		}
	}
	return e.admitRequest(ctx, p)
}
func (e *Engine) admitRequest(ctx context.Context, p *RequestPlan) error {
	mediaIDs := map[string]int64{}
	for _, out := range p.Outputs {
		if _, ok := mediaIDs[out.Target.MediaPath]; ok {
			continue
		}
		sum, err := hashFile(ctx, out.Target.MediaPath)
		if err != nil {
			return err
		}
		if sum != out.SourceSHA256 {
			return fmt.Errorf("media changed after approval: %s", out.Target.MediaPath)
		}
		if out.Target.MediaFileID != 0 {
			mediaIDs[out.Target.MediaPath] = out.Target.MediaFileID
			continue
		}
		probe, err := e.tool(ctx, "ffprobe")
		if err != nil {
			return err
		}
		// Catalog exactly the reviewed file; never rescan directories during admission.
		m, err := (media.Probe{Binary: probe}).File(ctx, out.Target.MediaPath)
		if err != nil {
			return err
		}
		if _, err = e.store.Upsert(ctx, m); err != nil {
			return err
		}
		var id int64
		if err = e.db.QueryRowContext(ctx, "SELECT id FROM media_files WHERE file_path=?", m.FilePath).Scan(&id); err != nil {
			return err
		}
		mediaIDs[m.FilePath] = id
	}
	tools := map[string]modules.Dependency{}
	for _, key := range []string{"ffprobe", "ffmpeg"} {
		if path, err := e.tool(ctx, key); err == nil {
			tools[key] = modules.Dependency{Path: path}
		}
	}
	for i := range p.Jobs {
		n := &p.Jobs[i]
		if n.Reused {
			continue
		}
		for j := range n.Data.Media {
			n.Data.Media[j].ID = mediaIDs[n.Data.Media[j].FilePath]
		}
		for j := range n.Data.Targets {
			n.Data.Targets[j].MediaFileID = mediaIDs[n.Data.Targets[j].MediaPath]
		}
		if err := e.validateBindings(ctx, n.Data); err != nil {
			return err
		}
		n.Data.Tools = tools
		n.Data.RequestSources = map[string]string{}
		for _, out := range p.Outputs {
			for _, target := range n.Data.Targets {
				if out.Target.MediaPath == target.MediaPath {
					n.Data.RequestSources[out.Target.MediaPath] = out.SourceSHA256
				}
			}
		}
		if n.Data.Builtin == "" {
			snap, err := modules.Pin(ctx, e.store, hub.Ref{Kind: hub.Flows, Identifier: n.Flow, Version: n.Version})
			if err != nil {
				return err
			}
			n.Data.Snapshot = snap
		}
	}
	for i := range p.Outputs {
		p.Outputs[i].Target.MediaFileID = mediaIDs[p.Outputs[i].Target.MediaPath]
	}
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return ErrStopping
	}
	if err := e.refreshGraphTerminals(ctx); err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM media_requests WHERE id=?", p.ID).Scan(&state); err != nil {
		return err
	}
	if state != "setup" {
		return nil
	}
	// Another request may have admitted identical work since approval. Sharing
	// it only removes work; it never adds an unapproved producer.
	remap := map[string]string{}
	for i := range p.Jobs {
		n := &p.Jobs[i]
		if n.Reused {
			continue
		}
		var active string
		x := tx.QueryRowContext(ctx, `SELECT x.job_id FROM job_execution x JOIN job_metadata j ON j.id=x.job_id WHERE x.dedup_key=? AND x.state IN ('waiting','ready') AND j.cancel_requested=0 AND j.terminal_state IS NULL`, n.DedupKey).Scan(&active)
		if x == nil {
			remap[n.ID] = active
			n.ID = active
			n.Reused = true
		} else if !errors.Is(x, sql.ErrNoRows) {
			return x
		}
	}
	for i := range p.Jobs {
		for j := range p.Jobs[i].Edges {
			edge := &p.Jobs[i].Edges[j]
			if id, ok := remap[edge.JobID]; ok {
				edge.JobID = id
			}
		}
	}
	for i := range p.Outputs {
		if id, ok := remap[p.Outputs[i].JobID]; ok {
			p.Outputs[i].JobID = id
		}
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, n := range p.Jobs {
		if n.Reused {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES(?,?,?,?,?,'[]',?)`, n.ID, n.Flow, n.Version, string(n.Data.Input), string(canonicalJSON(n.Data)), stamp); err != nil {
			return err
		}
		for _, m := range n.Data.Media {
			if _, err = tx.ExecContext(ctx, "INSERT INTO job_media VALUES(?,?)", n.ID, m.ID); err != nil {
				return err
			}
		}
		for _, pin := range n.Data.Snapshot.Entities {
			if _, err = tx.ExecContext(ctx, "INSERT INTO job_modules VALUES(?,?,?,?)", n.ID, pin.Kind, pin.Identifier, pin.Version); err != nil {
				return err
			}
		}
		state := "ready"
		if len(n.Edges) > 0 {
			state = "waiting"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO job_execution(job_id,targets,bindings,state,dedup_key) VALUES(?,?,?,?,?)`, n.ID, string(canonicalJSON(n.Data.Targets)), string(canonicalJSON(n.Data.Bindings)), state, n.DedupKey); err != nil {
			return err
		}
	}
	for _, n := range p.Jobs {
		if n.Reused {
			continue
		}
		for _, edge := range n.Edges {
			if _, err = tx.ExecContext(ctx, "INSERT INTO job_dependencies VALUES(?,?,?,?)", n.ID, edge.TargetIndex, edge.Name, edge.JobID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE media_requests SET plan=?,state='running',modified_at=? WHERE id=?", string(canonicalJSON(p)), stamp, p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) recordCoverage(ctx context.Context, in downloadedInput) error {
	spec := executionSpec(in)
	if spec == nil || in.Builtin != "" || len(in.RequestSources) == 0 {
		return nil
	}
	for _, target := range in.Targets {
		sum, err := hashFile(ctx, target.MediaPath)
		if err != nil {
			return err
		}
		if sum != in.RequestSources[target.MediaPath] {
			return fmt.Errorf("media changed during job execution: %s", target.MediaPath)
		}
		for _, out := range spec.Produces {
			if out.Kind != "tag" {
				continue
			}
			allowEmpty := false
			for _, key := range spec.AllowEmpty {
				allowEmpty = allowEmpty || key == out.Key
			}
			if !allowEmpty {
				var count int
				if err = e.db.QueryRowContext(ctx, `SELECT count(*) FROM tag t JOIN tagtype d ON d.id=t.tag_type_id JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id WHERE t.source_job_id=? AND d.key=? AND d.version=? AND s.media_file_id=? AND s.start<? AND s.end>?`, in.JobID, out.Key, out.Version, target.MediaFileID, target.End, target.Start).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return fmt.Errorf("producer %s completed without required output %s@%d", in.Snapshot.Root.Key, out.Key, out.Version)
				}
			}
			digest, x := e.requestOutputDigest(ctx, in.JobID, target, out)
			if x != nil {
				return x
			}
			_, err = e.db.ExecContext(ctx, `INSERT OR IGNORE INTO production_coverage(job_id,media_file_id,start,end,stream_index,key,version,source_sha256,output_digest) VALUES(?,?,?,?,?,?,?,?,?)`, in.JobID, target.MediaFileID, target.Start, target.End, target.StreamIndex, out.Key, out.Version, sum, digest)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Check declared nonempty outputs before ingestion so incomplete producer
// responses cannot leave partial tags available to unrelated consumers.
func validateRequestedOutputs(in downloadedInput, raw json.RawMessage) error {
	if len(in.RequestSources) == 0 {
		return nil
	}
	spec := executionSpec(in)
	if spec == nil {
		return nil
	}
	var result struct {
		Tags []store.ResultTag `json:"tags"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	for ti := range in.Targets {
		for _, out := range spec.Produces {
			if out.Kind != "tag" {
				continue
			}
			empty := false
			for _, key := range spec.AllowEmpty {
				empty = empty || key == out.Key
			}
			if empty {
				continue
			}
			found := false
			for _, tag := range result.Tags {
				if tag.TargetIndex != nil && *tag.TargetIndex == ti && tag.Key == out.Key && tag.Version == out.Version {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("producer %s omitted required output %s@%d for target %d", in.Snapshot.Root.Key, out.Key, out.Version, ti)
			}
		}
	}
	return nil
}
