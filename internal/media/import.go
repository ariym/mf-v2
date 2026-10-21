package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ariym/mf-v2/internal/store"
)

type Summary struct {
	Added   int      `json:"added"`
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
	// MediaIDs describes this scan, not all historical catalog rows. It stays
	// internal so durable import results retain their existing wire shape.
	MediaIDs []int64 `json:"-"`
}
type PartialError struct{ Count int }

func (e *PartialError) Error() string { return fmt.Sprintf("%d file(s) failed to import", e.Count) }

type Importer struct {
	Store       *store.Store
	Probe       Probe
	ExcludeDirs []string
}

func (i *Importer) Import(ctx context.Context, path string) (Summary, error) {
	var result Summary
	if e := i.Probe.Check(); e != nil {
		return result, e
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return result, e
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return result, e
	}
	info, e := os.Stat(abs)
	if e != nil {
		return result, e
	}
	excluded := map[string]bool{}
	for _, dir := range i.ExcludeDirs {
		canonical, err := filepath.Abs(dir)
		if err != nil {
			return result, err
		}
		if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
			canonical = resolved
		}
		excluded[canonical] = true
	}
	seen := map[string]bool{}
	databasePath, e := i.storePath(ctx)
	if e != nil {
		return result, e
	}
	failed := func(path string, e error) {
		result.Failed++
		if len(result.Errors) < 100 {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, e))
		}
	}
	one := func(path string, explicit bool) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		canonical, e := filepath.EvalSymlinks(path)
		if e != nil {
			failed(path, e)
			return nil
		}
		if seen[canonical] {
			result.Skipped++
			return nil
		}
		seen[canonical] = true
		stat, e := os.Stat(canonical)
		if e != nil {
			failed(path, e)
			return nil
		}
		if !stat.Mode().IsRegular() {
			if explicit {
				failed(path, fmt.Errorf("not a regular file"))
			} else {
				result.Skipped++
			}
			return nil
		}
		if canonical == databasePath || canonical == databasePath+"-wal" || canonical == databasePath+"-shm" {
			if explicit {
				failed(path, fmt.Errorf("cannot import the catalog database"))
			} else {
				result.Skipped++
			}
			return nil
		}
		m, e := i.Probe.File(ctx, canonical)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !explicit && (errors.Is(e, ErrNotMedia) || !expectedMediaExtension(path)) && !errors.Is(e, context.DeadlineExceeded) {
				result.Skipped++
			} else {
				failed(path, e)
			}
			return nil
		}
		added, e := i.Store.Upsert(ctx, m)
		if e != nil {
			failed(path, e)
			return nil
		}
		var id int64
		if e = i.Store.DB.QueryRowContext(ctx, "SELECT id FROM media_files WHERE file_path=?", canonical).Scan(&id); e != nil {
			failed(path, e)
			return nil
		}
		result.MediaIDs = append(result.MediaIDs, id)
		if added {
			result.Added++
		} else {
			result.Updated++
		}
		return nil
	}
	if !info.IsDir() {
		e = one(abs, true)
	} else {
		// WalkDir is lexical and does not follow directory symlinks.
		e = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				failed(path, walkErr)
				return nil
			}
			if d.IsDir() {
				if path == databasePath+".mf" || (path != abs && excluded[path]) {
					return filepath.SkipDir
				}
				return nil
			}
			return one(path, false)
		})
	}
	if e != nil {
		return result, e
	}
	if result.Failed > 0 {
		return result, &PartialError{result.Failed}
	}
	return result, nil
}

// storePath uses SQLite's authoritative filename to exclude catalog artifacts.
func (i *Importer) storePath(ctx context.Context) (string, error) {
	var seq int
	var name, path string
	err := i.Store.DB.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path)
	return path, err
}
