package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func TestV4MigrationFromV3PreservesDraftsAndWorkflowHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mf.db")
	s, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	draft, err := s.SaveDocument(ctx, "jobs", Document{Name: "keep", Document: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`DROP TABLE job_dependencies;DROP TABLE audio_artifacts;DROP TABLE job_execution;UPDATE mf_schema_migrations SET version=3`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actual, err := s.Document(ctx, "jobs", draft.ID)
	if err != nil || actual.Document != draft.Document {
		t.Fatalf("draft lost %+v %v", actual, err)
	}
	if err = Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func TestTargetDerivedResultUsesRequestedRangeAndOriginalDigest(t *testing.T) {
	s, m := catalogStore(t)
	ctx := context.Background()
	addDefinition(t, s, `{"key":"source","version":1,"data_type":"string"}`)
	addDefinition(t, s, `{"key":"derived","version":1,"data_type":"string"}`)
	full, err := s.AddSegment(ctx, m.ID, "00:00:00", "00:00:10")
	if err != nil {
		t.Fatal(err)
	}
	value := json.RawMessage(`"source text"`)
	source, err := s.AddTag(ctx, "source", 1, value, []TagLink{{SegmentID: full}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES('v2','test',2,'{}','{}','[]','now')`); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(value))
	target := Target{MediaFileID: m.ID, Start: "00:00:02.000000", End: "00:00:03.000000"}
	index := 0
	raw, _ := json.Marshal(map[string]any{"tags": []ResultTag{{Key: "derived", Version: 1, TargetIndex: &index, SourceTagID: &source, SourceValueSHA256: digest, Value: json.RawMessage(`"derived"`)}}})
	err = s.IngestTargetResult(ctx, "v2", raw, map[string]int{"source": 1, "derived": 1}, []Media{m}, []Target{target}, []Binding{{TargetIndex: 0, Kind: "tag", Key: "source", Version: 1, TagID: source, SHA256: digest}})
	if err != nil {
		t.Fatal(err)
	}
	var start, end string
	err = s.DB.QueryRow(`SELECT s.start,s.end FROM tag t JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id WHERE t.source_job_id='v2'`).Scan(&start, &end)
	if err != nil || start != target.Start || end != target.End {
		t.Fatalf("incorrect result range %s %s %v", start, end, err)
	}
}
