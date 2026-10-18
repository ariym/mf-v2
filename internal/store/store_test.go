package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaAndUpsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	s, e := Init(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = Validate(s.DB); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	m := Media{FilePath: "/media/a.mp4", MediaType: "video", FileSize: 100, Audio: []Audio{{StreamIndex: 1, Codec: "aac"}, {StreamIndex: 2, Codec: "aac"}}, Video: []Video{{StreamIndex: 0, Codec: "h264"}}}
	if added, e := s.Upsert(ctx, m); e != nil || !added {
		t.Fatalf("first import: %v %v", added, e)
	}
	before, e := s.Get(ctx, 1)
	if e != nil {
		t.Fatal(e)
	}
	m.Audio = m.Audio[:1]
	if added, e := s.Upsert(ctx, m); e != nil || added {
		t.Fatalf("reimport: %v %v", added, e)
	}
	after, e := s.Get(ctx, 1)
	if e != nil {
		t.Fatal(e)
	}
	if len(after.Audio) != 1 || len(after.Video) != 1 || before.CreatedAt != after.CreatedAt || after.ID != before.ID {
		t.Fatalf("reimport mismatch: %+v", after)
	}
	if _, e = s.DB.Exec("DELETE FROM media_files WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = s.DB.QueryRow("SELECT count(*) FROM file_audio_metadata").Scan(&count); e != nil || count != 0 {
		t.Fatalf("cascade: %d %v", count, e)
	}
	if _, e = s.DB.Exec("ALTER TABLE media_files DROP COLUMN duration"); e != nil {
		t.Fatal(e)
	}
	if e = Validate(s.DB); e == nil || !strings.Contains(e.Error(), "media_files") {
		t.Fatalf("schema drift accepted: %v", e)
	}
}
func TestExistingDatabaseNotMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("CREATE TABLE original(value TEXT); INSERT INTO original VALUES('keep')"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Init(path); e == nil {
		t.Fatal("accepted foreign database")
	}
	after, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if string(before) != string(after) {
		t.Fatal("modified incompatible database")
	}
	if _, e = Open(filepath.Join(t.TempDir(), "missing.db")); e == nil {
		t.Fatal("created missing database")
	}
}
func TestUnknownVersionAndMissingIndex(t *testing.T) {
	s, e := Init(filepath.Join(t.TempDir(), "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.DB.Exec("UPDATE mf_schema_migrations SET version=2"); e != nil {
		t.Fatal(e)
	}
	if e = Validate(s.DB); e == nil {
		t.Fatal("accepted future version")
	}
	if _, e = s.DB.Exec("UPDATE mf_schema_migrations SET version=1; DROP INDEX idx_history_instance_sequence_id"); e != nil {
		t.Fatal(e)
	}
	if e = Validate(s.DB); e == nil {
		t.Fatal("accepted missing index")
	}
}
