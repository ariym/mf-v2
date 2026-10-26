package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/ariym/mf-v2/internal/executable"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/google/uuid"
)

type RequestInput struct {
	TagTypes   []string          `json:"tagtypes"`
	Paths      []string          `json:"paths,omitempty"`
	SegmentIDs []int64           `json:"segment_ids,omitempty"`
	Flow       string            `json:"flow,omitempty"`
	Producers  map[string]string `json:"producers,omitempty"`
	Input      json.RawMessage   `json:"input,omitempty"`
}
type RequestInstall struct {
	Ref           hub.Ref `json:"ref"`
	DownloadBytes int64   `json:"download_bytes"`
}
type RequestPreparation struct {
	Program string          `json:"program"`
	Version int             `json:"version"`
	Profile string          `json:"profile"`
	Spec    hub.Preparation `json:"spec"`
}
type RequestEdge struct {
	TargetIndex int    `json:"target_index"`
	Name        string `json:"name"`
	JobID       string `json:"job_id"`
}
type RequestNode struct {
	ID       string          `json:"id"`
	Flow     string          `json:"flow"`
	Version  int             `json:"version"`
	Data     downloadedInput `json:"data"`
	Edges    []RequestEdge   `json:"dependencies,omitempty"`
	Reused   bool            `json:"reused,omitempty"`
	DedupKey string          `json:"dedup_key"`
}
type RequestOutput struct {
	Target        store.Target `json:"target"`
	Type          hub.DataSpec `json:"type"`
	SourceSHA256  string       `json:"source_sha256"`
	TagID         int64        `json:"tag_id,omitempty"`
	CoverageJobID string       `json:"coverage_job_id,omitempty"`
	JobID         string       `json:"job_id,omitempty"`
}
type RequestPlan struct {
	ID            string               `json:"id"`
	Fingerprint   string               `json:"fingerprint"`
	Request       RequestInput         `json:"request"`
	Installations []RequestInstall     `json:"installations"`
	Preparation   []RequestPreparation `json:"preparation"`
	Jobs          []RequestNode        `json:"jobs"`
	Outputs       []RequestOutput      `json:"outputs"`
	Resolutions   []hub.Resolution     `json:"resolutions"`
	Candidates    map[string][]string  `json:"candidates,omitempty"`
	Blockers      []string             `json:"blockers,omitempty"`
}

func requestDigest(v any) string     { return fmt.Sprintf("%x", sha256.Sum256(canonicalJSON(v))) }
func (p RequestPlan) digest() string { p.Fingerprint = ""; return requestDigest(p) }

func (e *Engine) tool(ctx context.Context, key string) (string, error) {
	if p, err := exec.LookPath(key); err == nil {
		return p, nil
	}
	m, err := e.store.Module(ctx, hub.Programs, key)
	if err != nil {
		return "", fmt.Errorf("%s must be installed before media inspection", key)
	}
	return m.Executable, modules.Executable(m.Executable)
}

func (e *Engine) PlanRequest(ctx context.Context, client hub.Client, in RequestInput, id string) (RequestPlan, error) {
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := uuid.Parse(id); err != nil {
		return RequestPlan{}, fmt.Errorf("invalid request ID")
	}
	p := RequestPlan{ID: id, Request: in, Installations: []RequestInstall{}, Preparation: []RequestPreparation{}, Jobs: []RequestNode{}, Outputs: []RequestOutput{}, Resolutions: []hub.Resolution{}, Candidates: map[string][]string{}}
	if len(in.TagTypes) == 0 || len(in.TagTypes) > 32 {
		return p, fmt.Errorf("request between 1 and 32 tag types")
	}
	if (len(in.Paths) == 0) == (len(in.SegmentIDs) == 0) {
		return p, fmt.Errorf("supply paths or segment_ids, not both")
	}
	if len(in.Input) == 0 {
		in.Input = json.RawMessage(`{}`)
	}
	var options map[string]json.RawMessage
	if err := hub.DecodeStrict(in.Input, &options); err != nil || options == nil {
		return p, fmt.Errorf("input must be an object")
	}
	var stream *int
	if raw, ok := options["audio_stream_index"]; ok {
		var n int
		if json.Unmarshal(raw, &n) != nil || n < 0 {
			return p, fmt.Errorf("audio_stream_index must be nonnegative")
		}
		stream = &n
	}
	p.Request.Input = in.Input
	var targets []store.Target
	mediaByPath := map[string]store.Media{}
	if len(in.SegmentIDs) > 1000 {
		return p, fmt.Errorf("too many segments")
	}
	for _, sid := range in.SegmentIDs {
		seg, err := e.store.Segment(ctx, sid)
		if err != nil {
			return p, err
		}
		m, err := e.store.Get(ctx, seg.MediaFileID)
		if err != nil {
			return p, err
		}
		mediaByPath[m.FilePath] = m
		target := store.Target{MediaFileID: m.ID, MediaPath: m.FilePath, SegmentID: sid, Start: seg.Start, End: seg.End, StreamIndex: stream}
		if full, x := fullTarget(m); x == nil {
			target.WholeFile = target.Start == full.Start && target.End == full.End
		}
		targets = append(targets, target)
	}
	probePath, probeErr := e.tool(ctx, "ffprobe")
	seenPaths := map[string]bool{}
	for _, path := range in.Paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.base, path)
		}
		path, err := filepath.EvalSymlinks(path)
		if err != nil {
			return p, err
		}
		rootPath := path
		err = filepath.WalkDir(path, func(path string, d os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if path == e.dbPath+".mf" {
					return filepath.SkipDir
				}
				for _, excluded := range e.excludeDirs {
					if path == excluded {
						return filepath.SkipDir
					}
				}
				return nil
			}
			explicit := path == rootPath
			canonical, x := filepath.EvalSymlinks(path)
			if x != nil {
				return x
			}
			path = canonical
			info, x := os.Stat(path)
			if x != nil {
				return x
			}
			if !info.Mode().IsRegular() {
				if explicit {
					return fmt.Errorf("not a regular file: %s", path)
				}
				return nil
			}
			if path == e.dbPath || path == e.dbPath+"-wal" || path == e.dbPath+"-shm" {
				if explicit {
					return fmt.Errorf("cannot request the catalog database")
				}
				return nil
			}
			if seenPaths[path] {
				return nil
			}
			seenPaths[path] = true
			var m store.Media
			var mid int64
			lookupErr := e.db.QueryRowContext(ctx, "SELECT id FROM media_files WHERE file_path=?", path).Scan(&mid)
			if lookupErr == nil {
				m, lookupErr = e.store.Get(ctx, mid)
			}
			if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				return lookupErr
			}
			if probeErr != nil {
				if lookupErr != nil {
					p.Blockers = append(p.Blockers, probeErr.Error())
					return nil
				}
			} else {
				probed, err := (media.Probe{Binary: probePath}).File(ctx, path)
				if err != nil && !explicit && (errors.Is(err, media.ErrNotMedia) || !media.ExpectedMediaExtension(path)) && !errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				if err != nil {
					return err
				}
				probed.ID = m.ID
				m = probed
			}
			t, err := fullTarget(m)
			if err != nil {
				return err
			}
			t.StreamIndex = stream
			if len(targets) >= 1000 {
				return fmt.Errorf("request exceeds 1000 targets")
			}
			targets = append(targets, t)
			mediaByPath[path] = m
			return nil
		})
		if err != nil {
			return p, err
		}
	}
	if len(p.Blockers) > 0 {
		p.Blockers = []string{p.Blockers[0]}
		p.Fingerprint = p.digest()
		return p, nil
	}
	if len(targets) == 0 {
		return p, fmt.Errorf("no media targets found")
	}
	hashes := map[string]string{}
	for path := range mediaByPath {
		sum, err := hashFile(ctx, path)
		if err != nil {
			return p, err
		}
		hashes[path] = sum
	}
	defs := []hub.DataSpec{}
	for _, name := range in.TagTypes {
		ref, err := hub.ParseRef(hub.TagTypes, name)
		if err != nil {
			return p, err
		}
		def, err := e.store.Definition(ctx, ref.Identifier, ref.Version)
		if errors.Is(err, sql.ErrNoRows) {
			r, x := client.Get(ctx, ref)
			err = x
			if x == nil {
				def = r.Value.(hub.TagType)
			}
		}
		if err != nil {
			return p, err
		}
		defs = append(defs, hub.DataSpec{Kind: "tag", Key: def.Key, Version: def.Version})
	}
	// Resolve cached outputs first: an offline request needs no catalog access.
	missing := map[string]bool{}
	for _, d := range defs {
		for _, t := range targets {
			ok, err := e.requestCovered(ctx, t, d, hashes[t.MediaPath])
			if err != nil {
				return p, err
			}
			if !ok {
				missing[d.Key] = true
			}
		}
	}
	flows := map[string]hub.Flow{}
	locals, err := e.store.Modules(ctx, hub.Flows)
	if err != nil {
		return p, err
	}
	for _, m := range locals {
		var f hub.Flow
		if json.Unmarshal(m.Manifest, &f) == nil {
			flows[fmt.Sprintf("%s@%d", f.Key, f.Version)] = f
		}
	}
	if len(missing) > 0 {
		for offset := 0; ; {
			page, x := client.List(ctx, hub.Flows, "", 1000, offset)
			if x != nil {
				if len(flows) == 0 {
					return p, x
				}
				break
			}
			for _, raw := range page.Items {
				r, x := hub.Decode(hub.Flows, raw)
				if x != nil {
					return p, x
				}
				f := r.Value.(hub.Flow)
				flows[fmt.Sprintf("%s@%d", f.Key, f.Version)] = f
			}
			if len(page.Items) < 1000 {
				break
			}
			offset += len(page.Items)
		}
	}
	selected := map[string]hub.Flow{}
	for _, d := range defs {
		if !missing[d.Key] {
			continue
		}
		choice := in.Producers[d.Key]
		if choice == "" {
			choice = in.Flow
		}
		candidates := []string{}
		for name, f := range flows {
			if f.Execution == nil {
				continue
			}
			supported := false
			for _, a := range f.Artifacts {
				supported = supported || (a.OS == runtime.GOOS && a.Arch == runtime.GOARCH)
			}
			if !supported {
				continue
			}
			for _, out := range f.Execution.Produces {
				if out == d {
					candidates = append(candidates, name)
					break
				}
			}
		}
		sort.Strings(candidates)
		if choice != "" {
			r, x := hub.ParseRef(hub.Flows, choice)
			if x != nil {
				return p, x
			}
			if r.Version == 0 {
				for _, name := range candidates {
					f := flows[name]
					if f.Key == r.Identifier && f.Version > r.Version {
						r.Version = f.Version
					}
				}
			}
			name := fmt.Sprintf("%s@%d", r.Identifier, r.Version)
			if _, ok := flows[name]; !ok {
				record, err := client.Get(ctx, r)
				if err != nil {
					return p, err
				}
				f := record.Value.(hub.Flow)
				r.Version = f.Version
				flows[fmt.Sprintf("%s@%d", f.Key, f.Version)] = f
			}
			choice = fmt.Sprintf("%s@%d", r.Identifier, r.Version)
		}
		if choice == "" && len(candidates) == 1 {
			choice = candidates[0]
		}
		if choice == "" {
			if len(candidates) == 0 {
				return p, fmt.Errorf("no producer declares %s@%d", d.Key, d.Version)
			}
			p.Candidates[d.Key] = candidates
			continue
		}
		f, ok := flows[choice]
		valid := false
		if ok && f.Execution != nil {
			for _, out := range f.Execution.Produces {
				valid = valid || out == d
			}
		}
		if !valid {
			return p, fmt.Errorf("%s does not declare %s@%d", choice, d.Key, d.Version)
		}
		selected[d.Key] = f
	}
	if len(p.Candidates) > 0 {
		p.Fingerprint = p.digest()
		return p, nil
	}
	resolutions := map[string]hub.Resolution{}
	for _, d := range defs {
		f, ok := selected[d.Key]
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s@%d", f.Key, f.Version)
		if _, ok = resolutions[key]; ok {
			continue
		}
		if snap, x := modules.Pin(ctx, e.store, hub.Ref{Kind: hub.Flows, Identifier: f.Key, Version: f.Version}); x == nil {
			res := e.installedRequestResolution(ctx, snap)
			resolutions[key] = res
			p.Resolutions = append(p.Resolutions, res)
			continue
		}
		raw := canonicalJSON(f)
		root, x := hub.Decode(hub.Flows, raw)
		if x != nil {
			return p, x
		}
		lookup := func(ctx context.Context, r hub.Ref) (hub.Record, error) {
			if r.Kind == hub.TagTypes {
				if def, x := e.store.Definition(ctx, r.Identifier, r.Version); x == nil {
					return hub.Decode(r.Kind, canonicalJSON(def))
				}
			}
			if m, x := e.store.Module(ctx, r.Kind, r.Identifier); x == nil && m.Version == r.Version {
				return hub.Decode(r.Kind, m.Manifest)
			}
			return client.Get(ctx, r)
		}
		res, x := hub.Resolve(ctx, root, lookup, runtime.GOOS, runtime.GOARCH)
		if x != nil {
			return p, x
		}
		resolutions[key] = res
		p.Resolutions = append(p.Resolutions, res)
	}
	installed := map[string]bool{}
	pins := map[string]int{}
	for _, res := range p.Resolutions {
		for _, r := range res.Order {
			key := r.Kind + "/" + r.Identifier
			if v, ok := pins[key]; ok && v != r.Version {
				return p, fmt.Errorf("conflicting versions of %s", key)
			}
			pins[key] = r.Version
			var raw []byte
			var artifact hub.Artifact
			if r.Kind == hub.TagTypes {
				for _, d := range res.TagTypes {
					if d.Key == r.Identifier {
						raw = canonicalJSON(d)
					}
				}
				if old, x := e.store.Definition(ctx, r.Identifier, 0); x == nil {
					if !store.SameJSON(canonicalJSON(old), raw) {
						return p, fmt.Errorf("%s conflicts with installed definition", key)
					}
				}
				continue
			}
			if r.Kind == hub.Programs {
				for _, v := range res.Programs {
					if v.ProgramName == r.Identifier {
						raw = canonicalJSON(v.Program)
						artifact = v.Artifact
					}
				}
			} else {
				for _, v := range res.Flows {
					if v.Key == r.Identifier {
						raw = canonicalJSON(v.Flow)
						artifact = v.Artifact
					}
				}
			}
			if old, x := e.store.Module(ctx, r.Kind, r.Identifier); x == nil {
				if old.Version != r.Version || !store.SameJSON(old.Manifest, raw) {
					return p, fmt.Errorf("%s conflicts with installed version", key)
				}
				if x = modules.Executable(old.Executable); x != nil {
					return p, x
				}
				continue
			} else if !errors.Is(x, sql.ErrNoRows) {
				return p, x
			}
			if installed[key] {
				continue
			}
			installed[key] = true
			if artifact.DownloadBytes == nil {
				return p, fmt.Errorf("%s@%d needs download_bytes metadata before automatic installation", key, r.Version)
			}
			p.Installations = append(p.Installations, RequestInstall{Ref: r, DownloadBytes: *artifact.DownloadBytes})
		}
	}
	nodeKeys := map[string]string{}
	var add func(hub.Resolution, string, store.Target, json.RawMessage, map[string]bool) (string, error)
	add = func(res hub.Resolution, flow string, t store.Target, input json.RawMessage, stack map[string]bool) (string, error) {
		if len(p.Jobs) >= 10000 {
			return "", fmt.Errorf("request graph exceeds 10000 jobs")
		}
		if stack[flow] {
			return "", fmt.Errorf("cycle at %s", flow)
		}
		next := map[string]bool{}
		for k, v := range stack {
			next[k] = v
		}
		next[flow] = true
		var f hub.Flow
		for _, v := range res.Flows {
			if v.Key == flow {
				f = v.Flow
			}
		}
		builtin := flow == "builtin:extract_audio"
		if !builtin && f.Execution == nil {
			return "", fmt.Errorf("%s requires protocol v2", flow)
		}
		m := mediaByPath[t.MediaPath]
		var opts map[string]json.RawMessage
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		if err := hub.DecodeStrict(input, &opts); err != nil || opts == nil {
			return "", fmt.Errorf("producer input must be an object")
		}
		output := filepath.Join(e.dbPath+".mf", "results")
		if raw, ok := opts["output_dir"]; ok {
			if json.Unmarshal(raw, &output) != nil || output == "" {
				return "", fmt.Errorf("output_dir must be a path")
			}
			if !filepath.IsAbs(output) {
				output = filepath.Join(e.base, output)
			}
		}
		opts["output_dir"] = canonicalJSON(output)
		if raw, ok := opts["audio_stream_index"]; ok {
			var selected int
			if json.Unmarshal(raw, &selected) != nil || selected < 0 {
				return "", fmt.Errorf("audio_stream_index must be nonnegative")
			}
			if t.StreamIndex != nil && *t.StreamIndex != selected {
				return "", fmt.Errorf("conflicting audio stream selections")
			}
			t.StreamIndex = &selected
		}
		if t.StreamIndex != nil {
			opts["audio_stream_index"] = canonicalJSON(*t.StreamIndex)
		}
		if builtin {
			if len(m.Audio) == 0 {
				return "", fmt.Errorf("source has no audio stream: %s", m.FilePath)
			}
			if len(m.Audio) > 1 && t.StreamIndex == nil {
				return "", fmt.Errorf("multiple audio streams: select input.audio_stream_index")
			}
			if t.StreamIndex != nil {
				found := false
				for _, audio := range m.Audio {
					found = found || audio.StreamIndex == *t.StreamIndex
				}
				if !found {
					return "", fmt.Errorf("audio stream %d unavailable", *t.StreamIndex)
				}
			}
		}
		input = canonicalJSON(opts)
		identity := flow
		if builtin {
			identity = "extract_audio"
		}
		key := graphIdentity(identity, f, []store.Target{t}, input, []string{hashes[t.MediaPath]})
		if id, ok := nodeKeys[key]; ok {
			return id, nil
		}
		node := RequestNode{ID: uuid.NewSHA1(uuid.MustParse(p.ID), []byte(key)).String(), Flow: identity, Version: f.Version, DedupKey: key}
		var activeID string
		x := e.db.QueryRowContext(ctx, `SELECT x.job_id FROM job_execution x JOIN job_metadata j ON j.id=x.job_id WHERE x.dedup_key=? AND x.state IN ('waiting','ready') AND j.cancel_requested=0 AND j.terminal_state IS NULL`, key).Scan(&activeID)
		if x == nil {
			j, err := e.Get(ctx, activeID)
			if err != nil {
				return "", err
			}
			if j.State != "completed" && j.State != "failed" && j.State != "cancelled" {
				node.ID = activeID
				node.Reused = true
				p.Jobs = append(p.Jobs, node)
				nodeKeys[key] = activeID
				return activeID, nil
			}
		} else if !errors.Is(x, sql.ErrNoRows) {
			return "", x
		}
		nodeKeys[key] = node.ID
		snap := e.requestSnapshot(ctx, res, flow)
		node.Data = downloadedInput{JobID: node.ID, Input: input, Snapshot: snap, Targets: []store.Target{t}, Media: []store.Media{m}}
		if builtin {
			node.Version = 1
			node.Data.Builtin = "extract_audio"
			node.Data.Snapshot = modules.Snapshot{}
		}
		if !builtin {
			for _, req := range f.Execution.Requires {
				var b *store.Binding
				var x error
				if t.MediaFileID != 0 {
					b, x = findBinding(ctx, e.db, t, req)
					if x != nil {
						return "", x
					}
				}
				if b != nil {
					b.TargetIndex = 0
					node.Data.Bindings = append(node.Data.Bindings, *b)
					continue
				}
				full, x := fullTarget(m)
				if x != nil {
					return "", x
				}
				full.StreamIndex = t.StreamIndex
				producer := req.Producer.Flow
				if req.Producer.Builtin != "" {
					producer = "builtin:" + req.Producer.Builtin
				}
				dep, x := add(res, producer, full, req.Producer.Input, next)
				if x != nil {
					return "", x
				}
				node.Edges = append(node.Edges, RequestEdge{TargetIndex: 0, Name: req.Name, JobID: dep})
			}
		}
		p.Jobs = append(p.Jobs, node)
		return node.ID, nil
	}
	for _, d := range defs {
		for _, t := range targets {
			out := RequestOutput{Target: t, Type: d, SourceSHA256: hashes[t.MediaPath]}
			covered, tagID, coverageJobID, x := e.requestCoverage(ctx, t, d, out.SourceSHA256)
			out.TagID = tagID
			out.CoverageJobID = coverageJobID
			if x != nil {
				return p, x
			}
			if !covered {
				f := selected[d.Key]
				res := resolutions[fmt.Sprintf("%s@%d", f.Key, f.Version)]
				out.JobID, x = add(res, f.Key, t, in.Input, map[string]bool{})
				if x != nil {
					return p, x
				}
			}
			p.Outputs = append(p.Outputs, out)
		}
	}
	neededFlows := map[string]bool{}
	var need func(modules.Snapshot, string)
	need = func(snap modules.Snapshot, key string) {
		if neededFlows[key] {
			return
		}
		neededFlows[key] = true
		for child := range snap.FlowManifests[key].Flows {
			need(snap, child)
		}
	}
	for _, node := range p.Jobs {
		if !node.Reused && node.Data.Builtin == "" {
			need(node.Data.Snapshot, node.Flow)
		}
	}
	preparation := map[string]bool{}
	for _, res := range p.Resolutions {
		for _, f := range res.Flows {
			if f.Execution == nil || !neededFlows[f.Key] {
				continue
			}
			keys := []string{}
			for k := range f.Execution.Preparation {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				profile := f.Execution.Preparation[key]
				var prog hub.Program
				for _, pr := range res.Programs {
					if pr.ProgramName == key {
						prog = pr.Program
					}
				}
				spec, ok := prog.Preparation[profile]
				if !ok {
					return p, fmt.Errorf("%s requires undeclared preparation %s/%s", f.Key, key, profile)
				}
				k := fmt.Sprintf("%s@%d/%s", key, prog.Version, profile)
				if preparation[k] {
					continue
				}
				preparation[k] = true
				ready := false
				if m, x := e.store.Module(ctx, hub.Programs, key); x == nil {
					ready = preparationReady(ctx, m.Executable, spec.ReadyArgs, nil) == nil
				}
				if ready {
					continue
				}
				for _, env := range spec.RequiredEnv {
					if os.Getenv(env) == "" {
						p.Blockers = append(p.Blockers, fmt.Sprintf("%s requires environment variable %s", k, env))
					}
				}
				p.Preparation = append(p.Preparation, RequestPreparation{Program: key, Version: prog.Version, Profile: profile, Spec: spec})
			}
		}
	}
	for _, n := range p.Jobs {
		if n.Reused {
			continue
		}
		needsProbe := n.Data.Builtin == "extract_audio"
		for _, binding := range n.Data.Bindings {
			needsProbe = needsProbe || binding.Kind == "audio"
		}
		if needsProbe {
			if _, err := e.tool(ctx, "ffprobe"); err != nil {
				if err = e.addRequestTool(ctx, client, &p, "ffprobe"); err != nil {
					return p, err
				}
			}
		}
		if n.Data.Builtin == "extract_audio" {
			if _, err := e.tool(ctx, "ffmpeg"); err != nil {
				if err = e.addRequestTool(ctx, client, &p, "ffmpeg"); err != nil {
					return p, err
				}
			}
		}
	}
	p.Fingerprint = p.digest()
	return p, nil
}

func runPreparation(ctx context.Context, path string, args []string, log *os.File) error {
	cmd := exec.CommandContext(ctx, path, args...)
	if log != nil {
		cmd.Stdout = log
		cmd.Stderr = log
	}
	return executable.Run(ctx, cmd)
}

func (e *Engine) requestSnapshot(ctx context.Context, res hub.Resolution, root string) modules.Snapshot {
	s := modules.Snapshot{FlowModules: map[string]store.Module{}, FlowManifests: map[string]hub.Flow{}, Programs: map[string]modules.Dependency{}, Flows: map[string]modules.Dependency{}, TagTypes: map[string]int{}, ProgramManifests: map[string]hub.Program{}}
	for _, r := range res.Order {
		s.Entities = append(s.Entities, hub.Ref{Kind: hub.LocalKind(r.Kind), Identifier: r.Identifier, Version: r.Version})
	}
	for _, d := range res.TagTypes {
		s.TagTypes[d.Key] = d.Version
	}
	module := func(kind, key string, v int, raw []byte, a hub.Artifact) store.Module {
		if m, x := e.store.Module(ctx, kind, key); x == nil {
			return m
		}
		dir := filepath.Join(e.excludeDirs[0], kind, key, fmt.Sprint(v))
		return store.Module{Kind: kind, Key: key, Version: v, Manifest: raw, Source: "request", InstallDir: dir, Executable: filepath.Join(dir, filepath.FromSlash(a.Entrypoint)), OS: a.OS, Arch: a.Arch, ArtifactSHA256: a.SHA256}
	}
	for _, v := range res.Programs {
		m := module(hub.Programs, v.ProgramName, v.Version, canonicalJSON(v.Program), v.Artifact)
		s.Programs[m.Key] = modules.Dependency{Version: m.Version, Path: m.Executable}
		s.ProgramManifests[m.Key] = v.Program
	}
	for _, v := range res.Flows {
		m := module(hub.Flows, v.Key, v.Version, canonicalJSON(v.Flow), v.Artifact)
		s.FlowModules[m.Key] = m
		s.FlowManifests[m.Key] = v.Flow
		if m.Key == root {
			s.Root = m
		} else {
			s.Flows[m.Key] = modules.Dependency{Version: m.Version, Path: m.Executable}
		}
	}
	return s
}

func (e *Engine) requestCovered(ctx context.Context, t store.Target, d hub.DataSpec, sum string) (bool, error) {
	ok, _, _, err := e.requestCoverage(ctx, t, d, sum)
	return ok, err
}
func (e *Engine) requestCoverage(ctx context.Context, t store.Target, d hub.DataSpec, sum string) (bool, int64, string, error) {
	if t.MediaFileID == 0 {
		return false, 0, "", nil
	}
	b, err := findBinding(ctx, e.db, t, hub.Requirement{Name: "requested", AnyOf: []hub.DataSpec{d}})
	if err != nil {
		return false, 0, "", err
	}
	if b != nil {
		var source sql.NullString
		var oldHash sql.NullString
		var requestSource sql.NullString
		err = e.db.QueryRowContext(ctx, `SELECT t.source_job_id,c.source_sha256,json_type(j.snapshot,'$.request_sources') FROM tag t LEFT JOIN job_metadata j ON j.id=t.source_job_id LEFT JOIN production_coverage c ON c.job_id=t.source_job_id AND c.key=? AND c.version=? AND c.media_file_id=? WHERE t.id=?`, d.Key, d.Version, t.MediaFileID, b.TagID).Scan(&source, &oldHash, &requestSource)
		if err != nil {
			return false, 0, "", err
		}
		valid := !oldHash.Valid || oldHash.String == sum
		if source.Valid && requestSource.Valid {
			j, x := e.Get(ctx, source.String)
			if x != nil {
				return false, 0, "", x
			}
			valid = valid && oldHash.Valid && j.State == "completed"
		}
		if valid {
			return true, b.TagID, "", nil
		}
	}
	rows, err := e.db.QueryContext(ctx, `SELECT c.job_id,c.start,c.end,c.output_digest FROM production_coverage c JOIN job_metadata j ON j.id=c.job_id WHERE c.media_file_id=? AND c.key=? AND c.version=? AND c.start<=? AND c.end>=? AND c.source_sha256=? AND (? IS NULL OR c.stream_index=?) AND j.ingested=1 AND j.cancel_requested=0 ORDER BY j.created_at DESC,c.job_id`, t.MediaFileID, d.Key, d.Version, t.Start, t.End, sum, t.StreamIndex, t.StreamIndex)
	if err != nil {
		return false, 0, "", err
	}
	type candidate struct{ id, start, end, digest string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.start, &c.end, &c.digest); err != nil {
			rows.Close()
			return false, 0, "", err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, 0, "", err
	}
	for _, c := range candidates {
		j, x := e.Get(ctx, c.id)
		if x != nil {
			return false, 0, "", x
		}
		if j.State != "completed" {
			continue
		}
		full := t
		full.Start = c.start
		full.End = c.end
		actual, err := e.requestOutputDigest(ctx, c.id, full, d)
		if err != nil {
			return false, 0, "", err
		}
		if actual == c.digest {
			return true, 0, c.id, nil
		}
	}
	return false, 0, "", nil
}
func (e *Engine) requestOutputDigest(ctx context.Context, job string, t store.Target, d hub.DataSpec) (string, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT t.id,t.value,s.start,s.end,st.value_pointer FROM tag t JOIN tagtype d ON d.id=t.tag_type_id JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id WHERE t.source_job_id=? AND d.key=? AND d.version=? AND s.media_file_id=? AND s.start<? AND s.end>? ORDER BY t.id,s.start,s.end,st.value_pointer`, job, d.Key, d.Version, t.MediaFileID, t.End, t.Start)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var records []any
	for rows.Next() {
		var id int64
		var value, start, end, pointer string
		if err = rows.Scan(&id, &value, &start, &end, &pointer); err != nil {
			return "", err
		}
		records = append(records, []any{id, value, start, end, pointer})
	}
	return requestDigest(records), rows.Err()
}
func (e *Engine) addRequestTool(ctx context.Context, c hub.Client, p *RequestPlan, key string) error {
	for _, i := range p.Installations {
		if i.Ref.Kind == hub.Programs && i.Ref.Identifier == key {
			return nil
		}
	}
	res, err := c.Resolve(ctx, hub.Ref{Kind: hub.Programs, Identifier: key}, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	for _, v := range res.Programs {
		if v.Artifact.DownloadBytes == nil {
			return fmt.Errorf("%s needs download_bytes metadata", key)
		}
		p.Installations = append(p.Installations, RequestInstall{Ref: hub.Ref{Kind: hub.Programs, Identifier: v.ProgramName, Version: v.Version}, DownloadBytes: *v.Artifact.DownloadBytes})
	}
	p.Resolutions = append(p.Resolutions, res)
	return nil
}

// BootstrapProbePlan exposes the sole allowed pre-review installation.
func (e *Engine) BootstrapProbePlan(ctx context.Context, c hub.Client) (RequestPlan, error) {
	p := RequestPlan{ID: uuid.NewString()}
	err := e.addRequestTool(ctx, c, &p, "ffprobe")
	p.Fingerprint = p.digest()
	return p, err
}

// graphIdentity is shared by explicit jobs and output requests. Exact manifests
// include their dependency pins; catalog IDs do not affect source identity.
func graphIdentity(flow string, f hub.Flow, targets []store.Target, input json.RawMessage, hashes []string) string {
	normalized := append([]store.Target{}, targets...)
	for i := range normalized {
		normalized[i].MediaFileID = 0
	}
	return requestDigest([]any{flow, f, normalized, json.RawMessage(input), hashes})
}

func preparationReady(ctx context.Context, path string, args []string, log *os.File) error {
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return runPreparation(check, path, args, log)
}

func (e *Engine) installedRequestResolution(ctx context.Context, s modules.Snapshot) hub.Resolution {
	res := hub.Resolution{Root: hub.Ref{Kind: hub.Flows, Identifier: s.Root.Key, Version: s.Root.Version}, OS: runtime.GOOS, Arch: runtime.GOARCH, Order: []hub.Ref{}, TagTypes: []hub.TagType{}, Programs: []hub.ResolvedProgram{}, Flows: []hub.ResolvedFlow{}}
	for _, ref := range s.Entities {
		ref.Kind = hub.WireKind(ref.Kind)
		res.Order = append(res.Order, ref)
		switch ref.Kind {
		case hub.TagTypes:
			d, _ := e.store.Definition(ctx, ref.Identifier, ref.Version)
			res.TagTypes = append(res.TagTypes, d)
		case hub.Programs:
			p := s.ProgramManifests[ref.Identifier]
			a := hub.Artifact{}
			for _, v := range p.Artifacts {
				if v.OS == runtime.GOOS && v.Arch == runtime.GOARCH {
					a = v
				}
			}
			res.Programs = append(res.Programs, hub.ResolvedProgram{Program: p, Artifact: a})
		case hub.Flows:
			f := s.FlowManifests[ref.Identifier]
			a := hub.Artifact{}
			for _, v := range f.Artifacts {
				if v.OS == runtime.GOOS && v.Arch == runtime.GOARCH {
					a = v
				}
			}
			res.Flows = append(res.Flows, hub.ResolvedFlow{Flow: f, Artifact: a})
		}
	}
	return res
}
