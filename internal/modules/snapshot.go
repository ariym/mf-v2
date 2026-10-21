package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/google/uuid"
)

type Dependency struct {
	Version int    `json:"version"`
	Path    string `json:"path"`
}
type Snapshot struct {
	Root             store.Module           `json:"root"`
	Programs         map[string]Dependency  `json:"programs"`
	Flows            map[string]Dependency  `json:"flows"`
	Entities         []hub.Ref              `json:"entities"`
	TagTypes         map[string]int         `json:"tag_types"`
	ProgramManifests map[string]hub.Program `json:"program_manifests"`
}

func Pin(ctx context.Context, s *store.Store, ref hub.Ref) (Snapshot, error) {
	out := Snapshot{Programs: map[string]Dependency{}, Flows: map[string]Dependency{}, TagTypes: map[string]int{}, ProgramManifests: map[string]hub.Program{}}
	lookup := func(ctx context.Context, r hub.Ref) (hub.Record, error) {
		if r.Kind == hub.TagTypes {
			def, e := s.Definition(ctx, r.Identifier, r.Version)
			if e != nil {
				return hub.Record{}, e
			}
			raw, _ := json.Marshal(def)
			return hub.Decode(hub.TagTypes, raw)
		}
		m, e := s.Module(ctx, r.Kind, r.Identifier)
		if e != nil {
			return hub.Record{}, e
		}
		if r.Version != 0 && m.Version != r.Version {
			return hub.Record{}, fmt.Errorf("%s requires version %d; installed version is %d", r.Identifier, r.Version, m.Version)
		}
		if m.OS != "" && (m.OS != runtime.GOOS || m.Arch != runtime.GOARCH) {
			return hub.Record{}, fmt.Errorf("%s was installed for %s/%s", m.Key, m.OS, m.Arch)
		}
		if e = Executable(m.Executable); e != nil {
			return hub.Record{}, e
		}
		if r.Kind == hub.Programs {
			var p hub.Program
			if e = json.Unmarshal(m.Manifest, &p); e != nil {
				return hub.Record{}, e
			}
			return hub.Record{Ref: hub.Ref{Kind: r.Kind, Identifier: m.Key, Version: m.Version}, Value: p}, nil
		}
		return hub.Decode(r.Kind, m.Manifest)
	}
	root, e := lookup(ctx, ref)
	if e != nil {
		return out, e
	}
	records, e := hub.Walk(ctx, root, lookup)
	if e != nil {
		return out, e
	}
	out.Root, e = s.Module(ctx, hub.Flows, ref.Identifier)
	if e != nil {
		return out, e
	}
	for _, r := range records {
		local := r.Ref
		local.Kind = hub.LocalKind(local.Kind)
		out.Entities = append(out.Entities, local)
		if r.Ref.Kind == hub.TagTypes {
			out.TagTypes[r.Ref.Identifier] = r.Ref.Version
			continue
		}
		m, e := s.Module(ctx, r.Ref.Kind, r.Ref.Identifier)
		if e != nil {
			return out, e
		}
		d := Dependency{m.Version, m.Executable}
		if r.Ref.Kind == hub.Programs {
			out.Programs[m.Key] = d
			out.ProgramManifests[m.Key] = r.Value.(hub.Program)
		} else if m.Key != out.Root.Key {
			out.Flows[m.Key] = d
		}
	}
	return out, nil
}
func Compatible(p hub.Program, m store.Media) error {
	if len(p.InputFormats) == 0 {
		return fmt.Errorf("program %s@%d has unknown input compatibility", p.ProgramName, p.Version)
	}
	for _, rule := range p.InputFormats {
		if !strings.EqualFold(filepath.Ext(m.FilePath), rule.Container) {
			continue
		}
		if rule.VideoCodec != "" {
			ok := false
			for _, v := range m.Video {
				if strings.EqualFold(v.Codec, rule.VideoCodec) {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if rule.AudioCodec != "" || rule.SampleRateHz != nil {
			ok := false
			for _, a := range m.Audio {
				if rule.AudioCodec != "" && !strings.EqualFold(a.Codec, rule.AudioCodec) {
					continue
				}
				if rule.SampleRateHz != nil && (a.SampleRate == nil || *a.SampleRate != *rule.SampleRateHz) {
					continue
				}
				ok = true
			}
			if !ok {
				continue
			}
		}
		return nil
	}
	return fmt.Errorf("program %s@%d does not accept %s", p.ProgramName, p.Version, m.FilePath)
}
func (i Installer) Remove(ctx context.Context, kind, key string) error {
	unlock, e := i.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if e = i.recover(ctx); e != nil {
		return e
	}
	if kind == "tagtypes" {
		return i.Store.RemoveModule(ctx, kind, key)
	}
	m, e := i.Store.Module(ctx, kind, key)
	if e != nil {
		return e
	}
	if m.InstallDir == "" {
		return i.Store.RemoveModule(ctx, kind, key)
	}
	if !within(i.Root, m.InstallDir) {
		return fmt.Errorf("installation is outside configured modules_path")
	}
	id := uuid.NewString()
	paths, _ := json.Marshal([]installPath{{Stage: filepath.Join(i.Root, ".remove-"+id), Final: m.InstallDir}})
	if _, e = i.Store.DB.ExecContext(ctx, "INSERT INTO install_journal(id,paths,created_at) VALUES(?,?,?)", id, string(paths), time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
		return e
	}
	// Validate dependency protection before touching the executable. Database
	// deletion serializes with submission's pin transaction; files are removed last.
	if e = i.Store.RemoveModule(ctx, kind, key); e != nil {
		_, _ = i.Store.DB.ExecContext(context.Background(), "DELETE FROM install_journal WHERE id=?", id)
		return e
	}
	if e = os.RemoveAll(m.InstallDir); e != nil {
		return e
	}
	_, e = i.Store.DB.ExecContext(ctx, "DELETE FROM install_journal WHERE id=?", id)
	return e
}
func (i Installer) Link(ctx context.Context, key string, version int, executable string, formats []hub.MediaInputFormat) error {
	if err := hub.ValidateInputFormats(formats); err != nil {
		return err
	}
	if !hub.ValidIdentifier(key) || version < 1 {
		return fmt.Errorf("valid program key and positive version required")
	}
	abs, e := filepath.Abs(executable)
	if e != nil {
		return e
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return e
	}
	if e = Executable(abs); e != nil {
		return e
	}
	unlock, e := i.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if e = i.recover(ctx); e != nil {
		return e
	}
	if old, err := i.Store.Module(ctx, hub.Programs, key); err == nil && old.Executable != abs {
		return fmt.Errorf("program %s is already registered at %s; remove it before relinking", key, old.Executable)
	}
	raw, _ := json.Marshal(hub.Program{ProgramName: key, Version: version, InputFormats: formats})
	return i.Store.RegisterInstall(ctx, nil, []store.Module{{Kind: hub.Programs, Key: key, Version: version, Manifest: raw, Source: "local", Executable: abs, OS: runtime.GOOS, Arch: runtime.GOARCH}}, "", "local")
}
