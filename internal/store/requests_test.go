package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestV5MigrationFromV4PreservesMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mf.db")
	s, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(context.Background(), Media{FilePath: "/keep.wav", MediaType: "audio", FileSize: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DROP TABLE production_coverage;DROP TABLE media_requests;UPDATE mf_schema_migrations SET version=4"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if old, err := Open(path); err == nil {
		old.Close()
		t.Fatal("v4 opened without migration")
	}
	if err = Migrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.Get(context.Background(), 1)
	if err != nil || m.FilePath != "/keep.wav" {
		t.Fatalf("media lost: %+v %v", m, err)
	}
	if err = Migrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}
