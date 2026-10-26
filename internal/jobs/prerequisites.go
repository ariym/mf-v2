package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/google/uuid"
)

const downloadedFlowV2 = "mf_downloaded_flow_v2"
const extractAudioFlow = "mf_extract_audio_v1"

type executionRecord struct {
	Targets      []store.Target
	Bindings     []store.Binding
	State, Error string
	CompletedAt  *string
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		n, e := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", e
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
func canonicalJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func fullTarget(m store.Media) (store.Target, error) {
	if m.Duration == nil || *m.Duration <= 0 {
		return store.Target{}, fmt.Errorf("known positive duration required for %s", m.FilePath)
	}
	end, err := store.TimecodeSeconds(*m.Duration)
	return store.Target{WholeFile: true, MediaPath: m.FilePath, MediaFileID: m.ID, Start: "00:00:00.000000", End: end}, err
}
func executionSpec(in downloadedInput) *hub.Execution {
	if in.Builtin != "" {
		return &hub.Execution{ProtocolVersion: 2, Produces: []hub.DataSpec{{Kind: "audio", Key: "audio", Version: 1}}}
	}
	f := in.Snapshot.FlowManifests[in.Snapshot.Root.Key]
	return f.Execution
}

func (e *Engine) SubmitSegments(ctx context.Context, name string, input json.RawMessage, ids []int64) (string, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return "", &InputError{fmt.Errorf("select between 1 and 1000 segments")}
	}
	ref, err := hub.ParseRef(hub.Flows, name)
	if err != nil {
		return "", &InputError{err}
	}
	snap, err := modules.Pin(ctx, e.store, ref)
	if err != nil {
		return "", &InputError{err}
	}
	if snap.FlowManifests[snap.Root.Key].Execution == nil {
		return "", &InputError{fmt.Errorf("segment targets require a protocol-v2 flow")}
	}
	var targets []store.Target
	var media []store.Media
	seen := map[int64]bool{}
	for _, id := range ids {
		s, err := e.store.Segment(ctx, id)
		if err != nil {
			return "", &InputError{err}
		}
		m, err := e.store.Get(ctx, s.MediaFileID)
		if err != nil {
			return "", err
		}
		target := store.Target{MediaPath: m.FilePath, MediaFileID: m.ID, SegmentID: s.ID, Start: s.Start, End: s.End}
		if full, err := fullTarget(m); err == nil {
			target.WholeFile = target.Start == full.Start && target.End == full.End
		}
		targets = append(targets, target)
		if !seen[m.ID] {
			media = append(media, m)
			seen[m.ID] = true
		}
	}
	return e.submitGraph(ctx, downloadedInput{JobID: uuid.NewString(), Input: input, Snapshot: snap, Media: media, Targets: targets}, nil)
}

// submitGraph serializes resolution with admission, cancellation, and module removal.
// The write transaction includes every new node, edge, binding, and exact module pin.
func (e *Engine) submitGraph(ctx context.Context, in downloadedInput, paths []string) (string, error) {
	if len(in.Input) == 0 {
		in.Input = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := hub.DecodeStrict(in.Input, &fields); err != nil || fields == nil {
		return "", &InputError{fmt.Errorf("flow input must be an object")}
	}
	var stream *int
	if raw, ok := fields["audio_stream_index"]; ok {
		var index int
		if err := json.Unmarshal(raw, &index); err != nil || index < 0 {
			return "", &InputError{fmt.Errorf("audio_stream_index must be nonnegative")}
		}
		stream = &index
	}
	if len(in.Targets) == 0 {
		for _, m := range in.Media {
			t, err := fullTarget(m)
			if err != nil {
				return "", &InputError{err}
			}
			in.Targets = append(in.Targets, t)
		}
	}
	if len(in.Targets) == 0 {
		return "", &InputError{fmt.Errorf("protocol-v2 flows require media or segment targets")}
	}
	for i := range in.Targets {
		if stream != nil {
			in.Targets[i].StreamIndex = stream
		}
	}
	// Build provider snapshots from the already-pinned closure before taking SQLite's single connection.
	providers := map[string]modules.Snapshot{}
	for key, m := range in.Snapshot.FlowModules {
		snap, err := modules.Pin(ctx, e.store, hub.Ref{Kind: hub.Flows, Identifier: key, Version: m.Version})
		if err != nil {
			return "", &InputError{err}
		}
		providers[key] = snap
	}
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return "", ErrStopping
	}
	if err := e.refreshGraphTerminals(ctx); err != nil {
		return "", err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for _, pin := range in.Snapshot.Entities {
		table := "tagtype"
		if pin.Kind == hub.Flows {
			table = "flow"
		} else if pin.Kind == hub.Programs {
			table = "program"
		}
		var version int
		if err = tx.QueryRowContext(ctx, "SELECT version FROM "+table+" WHERE key=?", pin.Identifier).Scan(&version); err != nil {
			return "", err
		}
		if version != pin.Version {
			return "", fmt.Errorf("installed version changed during submission")
		}
	}
	count := 0
	var persist func(downloadedInput, []string, bool, map[string]bool) (string, error)
	persist = func(node downloadedInput, nodePaths []string, producer bool, stack map[string]bool) (string, error) {
		var nodeInput map[string]json.RawMessage
		if len(node.Input) == 0 {
			node.Input = json.RawMessage(`{}`)
		}
		if err := hub.DecodeStrict(node.Input, &nodeInput); err != nil || nodeInput == nil {
			return "", fmt.Errorf("producer input must be an object")
		}
		output := filepath.Join(e.dbPath+".mf", "results")
		if raw, ok := nodeInput["output_dir"]; ok {
			if err := json.Unmarshal(raw, &output); err != nil || output == "" {
				return "", fmt.Errorf("output_dir must be a path")
			}
			if !filepath.IsAbs(output) {
				output = filepath.Join(e.base, output)
			}
		}
		nodeInput["output_dir"] = canonicalJSON(output)
		if raw, ok := nodeInput["audio_stream_index"]; ok {
			var stream int
			if err := json.Unmarshal(raw, &stream); err != nil || stream < 0 {
				return "", fmt.Errorf("audio_stream_index must be nonnegative")
			}
			for i := range node.Targets {
				if node.Targets[i].StreamIndex != nil && *node.Targets[i].StreamIndex != stream {
					return "", fmt.Errorf("conflicting audio stream selections")
				}
				node.Targets[i].StreamIndex = &stream
			}
		}
		var normalized any
		if err := hub.DecodeStrict(canonicalJSON(nodeInput), &normalized); err != nil {
			return "", err
		}
		node.Input = canonicalJSON(normalized)
		count++
		if count > 10000 {
			return "", fmt.Errorf("prerequisite graph exceeds 10000 nodes")
		}
		identity := node.Snapshot.Root.Key
		if node.Builtin != "" {
			identity = node.Builtin
		}
		if stack[identity] {
			return "", fmt.Errorf("prerequisite cycle at %s", identity)
		}
		next := map[string]bool{}
		for k, v := range stack {
			next[k] = v
		}
		next[identity] = true
		var dedup any
		if producer {
			// Content, stream, target, exact executable pins, and options identify shared work.
			fingerprints := []string{}
			for _, m := range node.Media {
				sum, err := hashFile(ctx, m.FilePath)
				if err != nil {
					return "", fmt.Errorf("prerequisite source unavailable: %w", err)
				}
				fingerprints = append(fingerprints, sum)
			}
			key := fmt.Sprintf("%x", sha256.Sum256(canonicalJSON([]any{identity, node.Snapshot.Root.Version, node.Snapshot.Entities, node.Targets, json.RawMessage(node.Input), fingerprints})))
			var existing string
			err := tx.QueryRowContext(ctx, `SELECT x.job_id FROM job_execution x JOIN job_metadata j ON j.id=x.job_id WHERE x.dedup_key=? AND x.state IN ('waiting','ready') AND j.cancel_requested=0 AND j.terminal_state IS NULL`, key).Scan(&existing)
			if err == nil {
				return existing, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			dedup = key
		}
		raw := canonicalJSON(node)
		if nodePaths == nil {
			nodePaths = []string{}
		}
		key, version := node.Snapshot.Root.Key, node.Snapshot.Root.Version
		if node.Builtin != "" {
			key = "extract_audio"
			version = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES(?,?,?,?,?,?,?)`, node.JobID, key, version, string(node.Input), string(raw), string(canonicalJSON(nodePaths)), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return "", err
		}
		for _, m := range node.Media {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO job_media VALUES(?,?)`, node.JobID, m.ID); err != nil {
				return "", err
			}
		}
		for _, pin := range node.Snapshot.Entities {
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_modules VALUES(?,?,?,?)`, node.JobID, pin.Kind, pin.Identifier, pin.Version); err != nil {
				return "", err
			}
		}
		spec := executionSpec(node)
		if spec == nil {
			return "", fmt.Errorf("producer %s does not support protocol v2", identity)
		}
		var bindings []store.Binding
		waiting := false
		for ti, target := range node.Targets {
			for _, req := range spec.Requires {
				binding, err := findBinding(ctx, tx, target, req)
				if err != nil {
					return "", err
				}
				if binding != nil {
					binding.TargetIndex = ti
					bindings = append(bindings, *binding)
					continue
				}
				var m store.Media
				found := false
				for _, v := range node.Media {
					if v.ID == target.MediaFileID {
						m = v
						found = true
					}
				}
				if !found {
					return "", fmt.Errorf("target media missing from snapshot")
				}
				full, err := fullTarget(m)
				if err != nil {
					return "", err
				}
				full.StreamIndex = target.StreamIndex
				p := req.Producer
				pin := providers[p.Flow]
				child := downloadedInput{JobID: uuid.NewString(), Input: p.Input, Snapshot: pin, Media: []store.Media{m}, Targets: []store.Target{full}}
				if len(child.Input) == 0 {
					child.Input = json.RawMessage(`{}`)
				}
				if p.Builtin != "" {
					child.Builtin = p.Builtin
					child.Snapshot = modules.Snapshot{}
				}
				id, err := persist(child, nil, true, next)
				if err != nil {
					return "", err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO job_dependencies VALUES(?,?,?,?)`, node.JobID, ti, req.Name, id); err != nil {
					return "", err
				}
				waiting = true
			}
		}
		state := "ready"
		if waiting {
			state = "waiting"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution(job_id,targets,bindings,state,dedup_key) VALUES(?,?,?,?,?)`, node.JobID, string(canonicalJSON(node.Targets)), string(canonicalJSON(bindings)), state, dedup); err != nil {
			return "", err
		}
		return node.JobID, nil
	}
	id, err := persist(in, paths, false, map[string]bool{})
	if err != nil {
		return "", &InputError{err}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

type sqlReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func findBinding(ctx context.Context, q sqlReader, target store.Target, req hub.Requirement) (*store.Binding, error) {
	for _, wanted := range req.AnyOf {
		if wanted.Kind == "tag" {
			rows, err := q.QueryContext(ctx, `SELECT DISTINCT t.id,t.value,s.start,s.end,coalesce(x.targets,'[]') FROM tag t JOIN tagtype d ON d.id=t.tag_type_id JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id LEFT JOIN job_execution x ON x.job_id=t.source_job_id WHERE d.key=? AND d.version=? AND s.media_file_id=? AND st.value_pointer='' ORDER BY t.id DESC`, wanted.Key, wanted.Version, target.MediaFileID)
			if err != nil {
				return nil, err
			}
			a, _ := store.ParseTimecode(target.Start)
			z, _ := store.ParseTimecode(target.End)
			for rows.Next() {
				var id int64
				var value, start, end, jobTargets string
				if err = rows.Scan(&id, &value, &start, &end, &jobTargets); err != nil {
					rows.Close()
					return nil, err
				}
				x, _ := store.ParseTimecode(start)
				y, _ := store.ParseTimecode(end)
				if target.StreamIndex != nil {
					var provenance struct {
						Media struct {
							StreamIndex *int `json:"stream_index"`
							Source      struct {
								StreamIndex *int `json:"stream_index"`
							} `json:"source"`
						} `json:"media"`
						Speech struct {
							Media struct {
								StreamIndex *int `json:"stream_index"`
								Source      struct {
									StreamIndex *int `json:"stream_index"`
								} `json:"source"`
							} `json:"media"`
						} `json:"speech"`
					}
					_ = json.Unmarshal([]byte(value), &provenance)
					stream := provenance.Media.Source.StreamIndex
					if stream == nil {
						stream = provenance.Media.StreamIndex
					}
					if stream == nil {
						stream = provenance.Speech.Media.Source.StreamIndex
					}
					if stream == nil {
						stream = provenance.Speech.Media.StreamIndex
					}
					var producerTargets []store.Target
					if err := json.Unmarshal([]byte(jobTargets), &producerTargets); err != nil {
						rows.Close()
						return nil, err
					}
					for _, produced := range producerTargets {
						if produced.MediaFileID == target.MediaFileID && produced.StreamIndex != nil {
							stream = produced.StreamIndex
							break
						}
					}
					if stream == nil || *stream != *target.StreamIndex {
						continue
					}
				}
				if x <= a && y >= z {
					rows.Close()
					return &store.Binding{Name: req.Name, Kind: "tag", Key: wanted.Key, Version: wanted.Version, TagID: id, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(value)))}, nil
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		} else {
			rows, err := q.QueryContext(ctx, `SELECT stream_index,path,sha256,source_sha256 FROM audio_artifacts WHERE media_file_id=? ORDER BY id DESC`, target.MediaFileID)
			if err != nil {
				return nil, err
			}
			var candidates []store.Binding
			for rows.Next() {
				b := store.Binding{Name: req.Name, Kind: "audio", Key: "audio", Version: 1}
				if err = rows.Scan(&b.StreamIndex, &b.Path, &b.SHA256, &b.SourceSHA256); err != nil {
					rows.Close()
					return nil, err
				}
				candidates = append(candidates, b)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			var sourcePath string
			if err = q.QueryRowContext(ctx, "SELECT file_path FROM media_files WHERE id=?", target.MediaFileID).Scan(&sourcePath); err != nil {
				return nil, err
			}
			var stream, rate, channels int
			var codec string
			query := `SELECT stream_index,sample_rate,channels,codec FROM file_audio_metadata WHERE media_file_id=? ORDER BY stream_index`
			nativeRows, err := q.QueryContext(ctx, query, target.MediaFileID)
			if err != nil {
				return nil, err
			}
			streams := 0
			compatible := false
			compatibleStream := 0
			for nativeRows.Next() {
				var sr, ch sql.NullInt64
				if err = nativeRows.Scan(&stream, &sr, &ch, &codec); err != nil {
					nativeRows.Close()
					return nil, err
				}
				streams++
				rate = int(sr.Int64)
				channels = int(ch.Int64)
				if (target.StreamIndex == nil || stream == *target.StreamIndex) && rate == 16000 && channels == 1 && codec == "pcm_s16le" {
					compatible = true
					compatibleStream = stream
				}
			}
			err = nativeRows.Err()
			nativeRows.Close()
			if err != nil {
				return nil, err
			}
			if compatible && (streams == 1 || target.StreamIndex != nil) && strings.EqualFold(filepath.Ext(sourcePath), ".wav") {
				sum, err := hashFile(ctx, sourcePath)
				if err == nil {
					return &store.Binding{Name: req.Name, Kind: "audio", Key: "audio", Version: 1, Path: sourcePath, SHA256: sum, SourceSHA256: sum, StreamIndex: compatibleStream}, nil
				}
			}
			if streams > 1 && target.StreamIndex == nil {
				continue
			}
			for _, b := range candidates {
				if target.StreamIndex != nil && b.StreamIndex != *target.StreamIndex {
					continue
				}
				sum, err := hashFile(ctx, b.Path)
				if err != nil || sum != b.SHA256 {
					continue
				}
				sourceSum, err := hashFile(ctx, sourcePath)
				if err != nil || sourceSum != b.SourceSHA256 {
					continue
				}
				return &b, nil
			}
		}
	}
	return nil, nil
}

func (e *Engine) execution(ctx context.Context, id string) (executionRecord, error) {
	var r executionRecord
	var targets, bindings string
	err := e.db.QueryRowContext(ctx, `SELECT targets,bindings,state,error,completed_at FROM job_execution WHERE job_id=?`, id).Scan(&targets, &bindings, &r.State, &r.Error, &r.CompletedAt)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(targets), &r.Targets); err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(bindings), &r.Bindings)
	return r, err
}
func (e *Engine) validateBindings(ctx context.Context, in downloadedInput) error {
	for _, b := range in.Bindings {
		if b.TargetIndex < 0 || b.TargetIndex >= len(in.Targets) {
			return fmt.Errorf("invalid prerequisite target")
		}
		t := in.Targets[b.TargetIndex]
		if b.Kind == "tag" {
			if err := e.store.ValidateTagBinding(ctx, b, t); err != nil {
				return err
			}
		} else {
			sum, err := hashFile(ctx, b.Path)
			if err != nil || sum != b.SHA256 {
				return fmt.Errorf("prerequisite audio missing or changed: %s", b.Path)
			}
			for _, m := range in.Media {
				if m.ID == t.MediaFileID {
					sum, err = hashFile(ctx, m.FilePath)
					if err != nil || sum != b.SourceSHA256 {
						return fmt.Errorf("prerequisite audio source changed: %s", m.FilePath)
					}
				}
			}
		}
	}
	return nil
}

func (e *Engine) graphTerminal(ctx context.Context, id, state, message string) error {
	_, err := e.db.ExecContext(ctx, `UPDATE job_execution SET state=?,error=?,completed_at=? WHERE job_id=? AND state IN ('ready','waiting')`, state, message, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	_, err = e.db.ExecContext(ctx, `UPDATE job_metadata SET terminal_state=? WHERE id=?`, state, id)
	return err
}

// dispatchGraph never waits inside a workflow activity. SQLite is the durable outbox.
func (e *Engine) dispatchGraph(ctx context.Context) error {
	e.gate.mu.Lock()
	defer e.gate.mu.Unlock()
	if e.gate.stopping {
		return nil
	}
	rows, err := e.db.QueryContext(ctx, `SELECT j.id,j.snapshot,j.submitted,j.cancel_requested FROM job_metadata j JOIN job_execution x ON x.job_id=j.id WHERE x.state IN ('ready','waiting') ORDER BY j.created_at,j.id`)
	if err != nil {
		return err
	}
	type entry struct {
		id                   string
		raw                  []byte
		submitted, cancelled bool
	}
	var entries []entry
	for rows.Next() {
		var p entry
		if err = rows.Scan(&p.id, &p.raw, &p.submitted, &p.cancelled); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range entries {
		if p.submitted {
			j, err := e.Get(ctx, p.id)
			if err != nil {
				return err
			}
			if j.State == "completed" || j.State == "failed" || j.State == "cancelled" {
				if err = e.graphTerminal(ctx, p.id, j.State, j.Error); err != nil {
					return err
				}
			}
			continue
		}
		if p.cancelled {
			if err = e.graphTerminal(ctx, p.id, "cancelled", "job cancelled"); err != nil {
				return err
			}
			continue
		}
		var in downloadedInput
		if err = json.Unmarshal(p.raw, &in); err != nil {
			return err
		}
		record, err := e.execution(ctx, p.id)
		if err != nil {
			return err
		}
		in.Targets = record.Targets
		in.Bindings = record.Bindings
		// Dependency failure takes precedence over an output left by a cancelled producer.
		deps, err := e.db.QueryContext(ctx, `SELECT d.prerequisite_id,coalesce(j.terminal_state,'') FROM job_dependencies d JOIN job_metadata j ON j.id=d.prerequisite_id WHERE d.job_id=?`, p.id)
		if err != nil {
			return err
		}
		waiting := false
		failure := ""
		for deps.Next() {
			var id, state string
			if err = deps.Scan(&id, &state); err != nil {
				deps.Close()
				return err
			}
			if state == "failed" || state == "cancelled" {
				failure = fmt.Sprintf("prerequisite %s %s", id, state)
			} else if state != "completed" {
				waiting = true
			}
		}
		err = deps.Err()
		deps.Close()
		if err != nil {
			return err
		}
		if failure != "" {
			if err = e.graphTerminal(ctx, p.id, "failed", failure); err != nil {
				return err
			}
			continue
		}
		if waiting {
			continue
		}
		spec := executionSpec(in)
		if spec == nil {
			return fmt.Errorf("missing execution contract for %s", p.id)
		}
		bindings := []store.Binding{}
		for ti, t := range in.Targets {
			for _, req := range spec.Requires {
				b, err := findBinding(ctx, e.db, t, req)
				if err != nil {
					return err
				}
				if b == nil {
					failure = fmt.Sprintf("prerequisite output %s unavailable after producer completed", req.Name)
					break
				}
				b.TargetIndex = ti
				bindings = append(bindings, *b)
			}
		}
		if failure != "" {
			if err = e.graphTerminal(ctx, p.id, "failed", failure); err != nil {
				return err
			}
			continue
		}
		in.Bindings = bindings
		if _, err = e.db.ExecContext(ctx, `UPDATE job_execution SET bindings=?,state='ready' WHERE job_id=?`, string(canonicalJSON(bindings)), p.id); err != nil {
			return err
		}
		if err = e.enqueue(ctx, p.id, canonicalJSON(in)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) graphJobFields(ctx context.Context, j *Job) error {
	r, err := e.execution(ctx, j.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	j.Targets = r.Targets
	rows, err := e.db.QueryContext(ctx, `SELECT DISTINCT prerequisite_id FROM job_dependencies WHERE job_id=? ORDER BY prerequisite_id`, j.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		j.PrerequisiteIDs = append(j.PrerequisiteIDs, id)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if r.State == "waiting" && j.State == "queued" {
		j.State = "waiting"
		j.WaitingReason = "waiting for prerequisite outputs"
	}
	if r.State == "failed" || r.State == "cancelled" {
		j.State = r.State
		j.Error = r.Error
		j.CompletedAt = r.CompletedAt
	}
	return nil
}

// Resolve engine terminal outcomes before deduplicating a new graph. Outputs may
// have been removed after completion but before the next periodic dispatch.
func (e *Engine) refreshGraphTerminals(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `SELECT x.job_id FROM job_execution x JOIN instances i ON i.id=x.job_id WHERE x.state IN ('ready','waiting') AND i.state!=0`)
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
		j, err := e.Get(ctx, id)
		if err != nil {
			return err
		}
		if err = e.graphTerminal(ctx, id, j.State, j.Error); err != nil {
			return err
		}
	}
	return nil
}
