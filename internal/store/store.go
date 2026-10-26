// Package store owns the media schema and validates both application and engine tables.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/cschleiden/go-workflows/backend"
	wsqlite "github.com/cschleiden/go-workflows/backend/sqlite"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE mf_schema_migrations (version INTEGER PRIMARY KEY);
INSERT INTO mf_schema_migrations VALUES (1);
CREATE TABLE media_files (
 id INTEGER PRIMARY KEY, file_path TEXT NOT NULL UNIQUE,
 media_type TEXT NOT NULL CHECK(media_type IN ('audio','video')),
 duration REAL, file_size INTEGER NOT NULL CHECK(file_size >= 0),
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE file_audio_metadata (
 id INTEGER PRIMARY KEY, media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
 stream_index INTEGER NOT NULL, sample_rate INTEGER, channels INTEGER, codec TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL, UNIQUE(media_file_id,stream_index)
);
CREATE TABLE file_video_metadata (
 id INTEGER PRIMARY KEY, media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
 stream_index INTEGER NOT NULL, width INTEGER, height INTEGER, fps REAL, codec TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL, UNIQUE(media_file_id,stream_index)
);`

type Store struct{ DB *sql.DB }

func connect(path, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", mode)
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if e = db.Ping(); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func initialize(db *sql.DB) error {
	if _, e := db.Exec(schema); e != nil {
		return e
	}
	if _, e := db.Exec(schemaV2); e != nil {
		return e
	}
	if _, e := db.Exec(schemaV3); e != nil {
		return e
	}
	if _, e := db.Exec(schemaV4); e != nil {
		return e
	}
	if _, e := db.Exec(schemaV5); e != nil {
		return e
	}
	b := wsqlite.NewSqliteBackendWithDB(db, wsqlite.WithBackendOptions(backend.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))))
	return b.Migrate()
}

// Init exclusively creates a new database, or validates an existing one without migration.
func Init(path string) (*Store, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if os.IsExist(e) {
		return Open(path)
	}
	if e != nil {
		return nil, fmt.Errorf("create sqlite_path %s: %w", path, e)
	}
	if e = f.Close(); e != nil {
		return nil, e
	}
	db, e := connect(path, "rw")
	if e != nil {
		return nil, e
	}
	if e = initialize(db); e != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", e)
	}
	if _, e = db.Exec("PRAGMA journal_mode=WAL"); e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}
func Open(path string) (*Store, error) {
	db, e := connect(path, "rw")
	if e != nil {
		return nil, fmt.Errorf("open sqlite_path %s: %w (run mf init to initialize a new database)", path, e)
	}
	if e = Validate(db); e != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite_path %s: %w", path, e)
	}
	if _, e = db.Exec("PRAGMA journal_mode=WAL"); e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.DB.Close() }

var baseline struct {
	sync.Once
	objects map[string]string
	err     error
}

func schemaObjects(db *sql.DB) (map[string]string, error) {
	rows, e := db.Query("SELECT type || ' ' || name, sql FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY name")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, s string
		if e = rows.Scan(&n, &s); e != nil {
			return nil, e
		}
		out[n] = strings.Join(strings.Fields(s), " ")
	}
	return out, rows.Err()
}
func Validate(db *sql.DB) error { return validateVersion(db, 5) }
func validateVersion(db *sql.DB, expected int) error {
	var v int
	if e := db.QueryRow("SELECT version FROM mf_schema_migrations").Scan(&v); e != nil {
		return fmt.Errorf("missing or invalid mf_schema_migrations: %w", e)
	}
	if v != expected {
		if v > 0 && v < expected {
			return fmt.Errorf("database schema %d requires migration; stop the server and run mf migrate", v)
		}
		return fmt.Errorf("unsupported schema version %d; executable expects %d", v, expected)
	}
	var ev int
	var dirty bool
	if e := db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&ev, &dirty); e != nil {
		return fmt.Errorf("workflow schema: %w", e)
	}
	if ev != 3 || dirty {
		return fmt.Errorf("unsupported workflow schema version %d (dirty=%v); expected clean version 3", ev, dirty)
	}
	baseline.Do(func() {
		var ref *sql.DB
		ref, baseline.err = sql.Open("sqlite", ":memory:")
		if baseline.err != nil {
			return
		}
		ref.SetMaxOpenConns(1)
		defer ref.Close()
		if baseline.err = initialize(ref); baseline.err != nil {
			return
		}
		baseline.objects, baseline.err = schemaObjects(ref)
	})
	if baseline.err != nil {
		return baseline.err
	}
	actual, e := schemaObjects(db)
	if e != nil {
		return e
	}
	for name, definition := range baseline.objects {
		if expected < 5 && v5Objects[name] {
			continue
		}
		if expected < 4 && v4Objects[name] {
			continue
		}
		if expected < 3 && v3Objects[name] {
			continue
		}
		if expected == 1 && v2Objects[name] {
			continue
		}
		got, ok := actual[name]
		if !ok {
			return fmt.Errorf("schema missing %s", name)
		}
		if got != definition {
			return fmt.Errorf("incompatible %s: columns, types, keys, or constraints differ from expected schema", name)
		}
	}
	return nil
}
func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }
