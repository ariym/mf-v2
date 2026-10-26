package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func faceDefinition(t *testing.T, s *Store, key string) {
	t.Helper()
	b, err := os.ReadFile("../hub/testdata/" + key + ".v1.json")
	if err != nil {
		t.Fatal(err)
	}
	addDefinition(t, s, string(b))
}

func faceJob(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.DB.Exec("INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES(?,'face_test',1,'{}','{}','[]','now')", id)
	if err != nil {
		t.Fatal(err)
	}
}

func faceVector() []float64 {
	v := make([]float64, 512)
	v[0] = 1
	return v
}

func TestFaceProfileSharesOneInstanceAcrossEnrollmentVideos(t *testing.T) {
	ctx := context.Background()
	s, first := catalogStore(t)
	second := first
	second.ID = 0
	second.FilePath = "/media/second.mp4"
	second.MediaType = "video"
	_, err := s.Upsert(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	second, err = s.ByPath(ctx, second.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	faceDefinition(t, s, "face_profile")
	faceJob(t, s, "face-profile")
	examples := []any{}
	for _, m := range []Media{first, second} {
		examples = append(examples, map[string]any{"media": map[string]any{"path": m.FilePath, "sha256": strings.Repeat("a", 64)}, "embedding": faceVector(), "track_ids": []string{"t1"}})
	}
	value := map[string]any{"schema_version": 1, "person_id": "alice", "label": "Alice", "embedding_space": strings.Repeat("b", 64), "embedding": faceVector(), "examples": examples, "reference": map[string]any{"path": "/reference/alice.jpg", "sha256": strings.Repeat("c", 64)}, "provenance": map[string]any{"custom": "preserved"}}
	body, _ := json.Marshal(map[string]any{"tags": []any{map[string]any{"key": "face_profile", "version": 1, "value": value}}})
	for range 2 {
		if err = s.IngestResult(ctx, "face-profile", body, map[string]int{"face_profile": 1}, []Media{first, second}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.Tags(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Tags(ctx, second.ID)
	if err != nil || len(a) != 1 || len(b) != 1 || a[0].ID != b[0].ID || len(a[0].Links) != 2 || !strings.Contains(string(a[0].Value), "preserved") {
		t.Fatalf("profile was not shared: %+v %+v %v", a, b, err)
	}
}

func TestFaceTrackIngestionPreservesTimedPointersAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, m := catalogStore(t)
	faceDefinition(t, s, "face_tracks")
	faceJob(t, s, "face-tracks")
	track := map[string]any{"track_id": "v0-t1", "start": 1.123456, "end": 2.5, "person_id": nil, "label": "unknown", "score": 0.2, "score_type": "cosine_similarity", "embedding": faceVector(), "candidates": []any{map[string]any{"person_id": "alice", "label": "Alice", "score": 0.2}}}
	value := map[string]any{"schema_version": 1, "media": map[string]any{"path": m.FilePath, "sha256": strings.Repeat("a", 64)}, "embedding_space": strings.Repeat("b", 64), "tracks": []any{track}, "observations": []any{}, "embeddings": map[string]any{"artifact": "video-0-embeddings.npy", "sha256": strings.Repeat("c", 64), "shape": []int{1, 512}, "dtype": "float64"}, "provenance": map[string]any{}}
	bundle := func() []byte {
		b, _ := json.Marshal(map[string]any{"tags": []any{map[string]any{"key": "face_tracks", "version": 1, "value": value}}})
		return b
	}
	track["end"] = 11
	if err := s.IngestResult(ctx, "face-tracks", bundle(), map[string]int{"face_tracks": 1}, []Media{m}); err == nil {
		t.Fatal("accepted out-of-media track")
	}
	segments, _ := s.Segments(ctx, m.ID)
	if len(segments) != 0 {
		t.Fatal("partial face result committed")
	}
	track["end"] = 2.5
	for range 2 {
		if err := s.IngestResult(ctx, "face-tracks", bundle(), map[string]int{"face_tracks": 1}, []Media{m}); err != nil {
			t.Fatal(err)
		}
	}
	tags, err := s.Tags(ctx, m.ID)
	if err != nil || len(tags) != 1 || len(tags[0].Links) != 2 || tags[0].Links[1].ValuePointer != "/tracks/0" {
		t.Fatalf("missing track links: %+v %v", tags, err)
	}
	segments, _ = s.Segments(ctx, m.ID)
	if len(segments) != 2 || segments[1].Start != "00:00:01.123456" {
		t.Fatalf("track timing lost: %+v", segments)
	}
}
