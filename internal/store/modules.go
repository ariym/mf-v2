package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ariym/mf-v2/internal/hub"
)

type Module struct {
	ID             int64           `json:"id"`
	Kind           string          `json:"kind"`
	Key            string          `json:"key"`
	Version        int             `json:"version"`
	Manifest       json.RawMessage `json:"manifest"`
	Source         string          `json:"source"`
	InstallDir     string          `json:"install_dir"`
	Executable     string          `json:"executable"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	ArtifactSHA256 string          `json:"artifact_sha256,omitempty"`
	CreatedAt      string          `json:"created_at"`
	ModifiedAt     string          `json:"modified_at"`
}

func moduleTable(kind string) (string, error) {
	switch kind {
	case "flows":
		return "flow", nil
	case "programs":
		return "program", nil
	}
	return "", fmt.Errorf("expected flows or programs")
}
func scanModule(row scanner, kind string) (Module, error) {
	var m Module
	var raw string
	m.Kind = kind
	e := row.Scan(&m.ID, &m.Key, &m.Version, &raw, &m.Source, &m.InstallDir, &m.Executable, &m.OS, &m.Arch, &m.ArtifactSHA256, &m.CreatedAt, &m.ModifiedAt)
	m.Manifest = json.RawMessage(raw)
	return m, e
}

const moduleColumns = "id,key,version,manifest,source,install_dir,executable,platform_os,platform_arch,artifact_sha256,created_at,modified_at"

func (s *Store) Module(ctx context.Context, kind, key string) (Module, error) {
	table, e := moduleTable(kind)
	if e != nil {
		return Module{}, e
	}
	return scanModule(s.DB.QueryRowContext(ctx, "SELECT "+moduleColumns+" FROM "+table+" WHERE key=?", key), kind)
}
func (s *Store) Modules(ctx context.Context, kind string) ([]Module, error) {
	table, e := moduleTable(kind)
	if e != nil {
		return nil, e
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT "+moduleColumns+" FROM "+table+" ORDER BY key")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Module{}
	for rows.Next() {
		m, e := scanModule(rows, kind)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func SameJSON(a, b []byte) bool {
	var x, y any
	dx := json.NewDecoder(bytes.NewReader(a))
	dx.UseNumber()
	dy := json.NewDecoder(bytes.NewReader(b))
	dy.UseNumber()
	if dx.Decode(&x) != nil || dy.Decode(&y) != nil {
		return false
	}
	aa, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return string(aa) == string(bb)
}
func (s *Store) RegisterInstall(ctx context.Context, defs []hub.TagType, modules []Module, journalID, source string) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = registerInstall(ctx, tx, defs, modules, journalID, source); e != nil {
		return e
	}
	return tx.Commit()
}

func registerInstall(ctx context.Context, q queryer, defs []hub.TagType, modules []Module, journalID, source string) error {
	var e error
	for _, def := range defs {
		if e = insertTagType(ctx, q, def, source); e != nil {
			return e
		}
	}
	for _, m := range modules {
		table, e := moduleTable(m.Kind)
		if e != nil {
			return e
		}
		var raw string
		var version int
		e = q.QueryRowContext(ctx, "SELECT version,manifest FROM "+table+" WHERE key=?", m.Key).Scan(&version, &raw)
		if e == nil {
			if version != m.Version || !SameJSON([]byte(raw), m.Manifest) {
				return fmt.Errorf("%s %s conflicts with installed version", m.Kind, m.Key)
			}
			continue
		}
		if e != sql.ErrNoRows {
			return e
		}
		_, e = q.ExecContext(ctx, "INSERT INTO "+table+"(key,version,manifest,source,install_dir,executable,platform_os,platform_arch,artifact_sha256,created_at,modified_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", m.Key, m.Version, string(m.Manifest), m.Source, m.InstallDir, m.Executable, m.OS, m.Arch, m.ArtifactSHA256, now(), now())
		if e != nil {
			return e
		}
	}
	if journalID != "" {
		if _, e = q.ExecContext(ctx, "DELETE FROM install_journal WHERE id=?", journalID); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) RemoveModule(ctx context.Context, kind, key string) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = removeModule(ctx, tx, kind, key); e != nil {
		return e
	}
	return tx.Commit()
}

// RegisterReplacement keeps old registrations intact if any dependency check or write fails.
func (s *Store) RegisterReplacement(ctx context.Context, defs []hub.TagType, modules []Module, journalID, source string, replacements []hub.Ref) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for n := len(replacements) - 1; n >= 0; n-- {
		r := replacements[n]
		if e = removeModule(ctx, tx, r.Kind, r.Identifier); e != nil {
			return e
		}
	}
	if e = registerInstall(ctx, tx, defs, modules, journalID, source); e != nil {
		return e
	}
	return tx.Commit()
}

// CheckReplacement preflights removability without changing the installed state.
// RegisterReplacement repeats these checks under its commit transaction.
func (s *Store) CheckReplacement(ctx context.Context, replacements []hub.Ref) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for n := len(replacements) - 1; n >= 0; n-- {
		r := replacements[n]
		if err = removeModule(ctx, tx, r.Kind, r.Identifier); err != nil {
			return err
		}
	}
	return nil
}

func removeModule(ctx context.Context, q queryer, kind, key string) error {
	kind = hub.LocalKind(hub.WireKind(kind))
	var table string
	var e error
	if kind == "tagtypes" {
		table = "tagtype"
	} else {
		table, e = moduleTable(kind)
		if e != nil {
			return e
		}
	}
	var version int
	if e = q.QueryRowContext(ctx, "SELECT version FROM "+table+" WHERE key=?", key).Scan(&version); e != nil {
		return e
	}
	var count int
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM job_modules jm JOIN job_metadata j ON j.id=jm.job_id LEFT JOIN instances i ON i.id=j.id WHERE jm.kind=? AND jm.key=? AND j.terminal_state IS NULL AND (j.submitted=0 OR i.id IS NULL OR i.state=0)`, kind, key).Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		return fmt.Errorf("%s %s is pinned by unfinished jobs", kind, key)
	}
	if kind == "tagtypes" {
		if e = q.QueryRowContext(ctx, "SELECT count(*) FROM tag WHERE tag_type_id=(SELECT id FROM tagtype WHERE key=?)", key).Scan(&count); e != nil {
			return e
		}
		if count > 0 {
			return fmt.Errorf("tag type %s is used by %d tag instances; keep its installed version", key, count)
		}
	}
	// Definitions and flows retain exact dependency pins in immutable manifests.
	for _, owner := range []string{"tagtype", "flow"} {
		rows, e := q.QueryContext(ctx, "SELECT key,manifest FROM "+owner)
		if e != nil {
			return e
		}
		for rows.Next() {
			var k, raw string
			if e = rows.Scan(&k, &raw); e != nil {
				rows.Close()
				return e
			}
			wireKind := hub.Flows
			if owner == "tagtype" {
				wireKind = hub.TagTypes
			}
			r, e := hub.Decode(wireKind, []byte(raw))
			if e != nil {
				rows.Close()
				return e
			}
			for _, dep := range r.Dependencies {
				if hub.LocalKind(dep.Kind) == kind && dep.Identifier == key {
					rows.Close()
					return fmt.Errorf("%s %s is required by %s %s", kind, key, owner, k)
				}
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if _, e = q.ExecContext(ctx, "DELETE FROM "+table+" WHERE key=?", key); e != nil {
		return fmt.Errorf("cannot remove referenced %s: %w", kind, e)
	}
	return nil
}
