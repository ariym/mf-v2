package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateV2PreservesCatalogAndAddsDraftStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mf.db")
	s, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.Upsert(ctx, Media{FilePath: "/media/keep.mp4", MediaType: "video", FileSize: 23}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DROP TABLE production_coverage;DROP TABLE media_requests;DROP TABLE job_dependencies; DROP TABLE audio_artifacts; DROP TABLE job_execution; DROP TABLE manifest_drafts; DROP TABLE job_definitions; UPDATE mf_schema_migrations SET version=2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = Open(path); err == nil || !strings.Contains(err.Error(), "mf migrate") {
		t.Fatalf("v2 opened without explicit migration: %v", err)
	}
	if err = Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	media, err := s.Get(ctx, 1)
	if err != nil || media.FilePath != "/media/keep.mp4" {
		t.Fatalf("catalog changed: %+v %v", media, err)
	}
	d, err := s.SaveDocument(ctx, "jobs", Document{Name: "Keep this draft", Document: "{ incomplete"})
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	after, err := s.Document(ctx, "jobs", d.ID)
	if err != nil || after.Document != d.Document {
		t.Fatal("draft not durable")
	}
	if _, err = s.SaveDocument(ctx, "invalid", d); err == nil {
		t.Fatal("invalid table accepted")
	}
	if _, err = s.SaveDocument(ctx, "jobs", Document{Name: ""}); err == nil {
		t.Fatal("empty name accepted")
	}
}
