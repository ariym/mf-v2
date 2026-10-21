package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/hub"
	wsqlite "github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
)

func TestMigratePreservesLegacyMediaAndJobs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mf.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(schema); e != nil {
		t.Fatal(e)
	}
	backend := wsqlite.NewSqliteBackendWithDB(db)
	if e = backend.Migrate(); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO media_files VALUES(41,'/media/keep.wav','audio',10,123,'created','modified')"); e != nil {
		t.Fatal(e)
	}
	if _, e = client.New(backend).CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{InstanceID: "keep-job"}, "import_media", json.RawMessage(`{"path":"/media/keep.wav"}`)); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if _, e = Open(path); e == nil || !strings.Contains(e.Error(), "mf migrate") {
		t.Fatalf("expected migration instruction: %v", e)
	}
	if e = Migrate(ctx, path); e != nil {
		t.Fatal(e)
	}
	if e = Migrate(ctx, path); e != nil {
		t.Fatal(e)
	}
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	m, e := s.Get(ctx, 41)
	if e != nil || m.CreatedAt != "created" || m.FileSize != 123 {
		t.Fatalf("media lost: %+v %v", m, e)
	}
	var count int
	if e = s.DB.QueryRow("SELECT count(*) FROM instances WHERE id='keep-job'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("job lost: %d %v", count, e)
	}
}
func TestTimecodes(t *testing.T) {
	for _, text := range []string{"00:00:00", "00:01:23.45", "123:59:59.999999"} {
		us, e := ParseTimecode(text)
		if e != nil {
			t.Fatal(e)
		}
		again, e := ParseTimecode(Timecode(us))
		if e != nil || us != again {
			t.Fatalf("round trip %s", text)
		}
	}
	for _, text := range []string{"1.5", "2026-10-20T12:00:00Z", "-01:00:00", "00:60:00", "00:00:60", "00:00:00.1234567", "00:+1:00", "00:00:00."} {
		if _, e := ParseTimecode(text); e == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
func catalogStore(t *testing.T) (*Store, Media) {
	t.Helper()
	s, e := Init(filepath.Join(t.TempDir(), "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	duration := 10.0
	m := Media{FilePath: "/media/a.wav", MediaType: "audio", Duration: &duration, FileSize: 100}
	if _, e = s.Upsert(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	m, e = s.ByPath(context.Background(), m.FilePath)
	if e != nil {
		t.Fatal(e)
	}
	return s, m
}
func addDefinition(t *testing.T, s *Store, body string) {
	t.Helper()
	r, e := hub.Decode(hub.TagTypes, []byte(body))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AddTagType(context.Background(), r.Value.(hub.TagType), "test"); e != nil {
		t.Fatal(e)
	}
}
func TestSharedTagValidationAndDeletion(t *testing.T) {
	ctx := context.Background()
	s, m := catalogStore(t)
	addDefinition(t, s, `{"key":"confidence","version":1,"data_type":"number","format":{"range":[0,1]}}`)
	addDefinition(t, s, `{"key":"label","version":1,"data_type":"string","format":{"choices":["a","b"]}}`)
	addDefinition(t, s, `{"key":"set","version":1,"data_type":"tag_set","format":{"tags":{"confidence":1,"label":1}}}`)
	a, e := s.AddSegment(ctx, m.ID, "00:00:00", "00:00:01.123456")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.AddSegment(ctx, m.ID, "00:00:02", "00:00:03")
	if e != nil {
		t.Fatal(e)
	}
	links := []TagLink{{SegmentID: a}, {SegmentID: b}}
	for _, raw := range []string{`2`, `"0.5"`, `null`} {
		if _, e = s.AddTag(ctx, "confidence", 1, json.RawMessage(raw), links); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	id, e := s.AddTag(ctx, "confidence", 1, json.RawMessage(`0.5`), links)
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.Segment(ctx, a)
	if e != nil || len(v.Tags) != 1 || v.Tags[0] != id || v.End != "00:00:01.123456" {
		t.Fatalf("segment %+v %v", v, e)
	}
	if _, e = s.AddTag(ctx, "set", 1, json.RawMessage(`{"confidence":0.8,"label":"a"}`), []TagLink{{SegmentID: a}}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AddTag(ctx, "set", 1, json.RawMessage(`{"confidence":0.8}`), links); e == nil {
		t.Fatal("accepted incomplete set")
	}
	if e = s.RemoveModule(ctx, "tagtypes", "confidence"); e == nil {
		t.Fatal("removed required definition")
	}
	if e = s.RemoveSegment(ctx, a); e != nil {
		t.Fatal(e)
	}
	tags, e := s.Tags(ctx, m.ID)
	if e != nil || len(tags) != 1 || tags[0].ID != id {
		t.Fatalf("shared instance lost: %+v %v", tags, e)
	}
	if e = s.RemoveSegment(ctx, b); e != nil {
		t.Fatal(e)
	}
	tags, e = s.Tags(ctx, m.ID)
	if e != nil || len(tags) != 0 {
		t.Fatalf("orphan instance: %+v %v", tags, e)
	}
	if _, e = s.AddSegment(ctx, m.ID, "00:00:09", "00:00:11"); e == nil {
		t.Fatal("accepted out of duration range")
	}
}
func TestIngestStructuredResultIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, m := catalogStore(t)
	raw, e := os.ReadFile("../hub/testdata/speaker.v2.json")
	if e != nil {
		t.Fatal(e)
	}
	addDefinition(t, s, string(raw))
	_, e = s.DB.Exec("INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES('job','test',1,'{}','{}','[]','now')")
	if e != nil {
		t.Fatal(e)
	}
	value := map[string]any{"diarization": []any{map[string]any{"start": 1.123456, "end": 2.5, "speaker": "S1", "custom": "retained"}}, "exclusive_diarization": []any{}, "embeddings": map[string]any{}, "media": map[string]any{"path": m.FilePath, "sha256": strings.Repeat("a", 64)}, "raw": map[string]any{"upstream": "preserved"}}
	body, _ := json.Marshal(map[string]any{"tags": []any{map[string]any{"key": "speaker", "version": 2, "value": value}}})
	pins := map[string]int{"speaker": 2}
	if e = s.IngestResult(ctx, "job", body, map[string]int{"speaker": 1}, []Media{m}); e == nil {
		t.Fatal("accepted wrong schema pin")
	}
	segments, _ := s.Segments(ctx, m.ID)
	if len(segments) != 0 {
		t.Fatal("partial write")
	}
	for range 2 {
		if e = s.IngestResult(ctx, "job", body, pins, []Media{m}); e != nil {
			t.Fatal(e)
		}
	}
	segments, e = s.Segments(ctx, m.ID)
	if e != nil || len(segments) != 2 {
		t.Fatalf("segments %+v %v", segments, e)
	}
	tags, e := s.Tags(ctx, m.ID)
	if e != nil || len(tags) != 1 || len(tags[0].Links) != 2 || !strings.Contains(string(tags[0].Value), "retained") {
		t.Fatalf("tags %+v %v", tags, e)
	}
	if segments[1].Start != "00:00:01.123456" {
		t.Fatalf("precision: %+v", segments)
	}
	value["diarization"] = []any{}
	updated, _ := json.Marshal(value)
	if e = s.UpdateTag(ctx, tags[0].ID, updated); e == nil {
		t.Fatal("invalidated a linked JSON pointer")
	}
}

func TestCancelledIngestionDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	s, m := catalogStore(t)
	addDefinition(t, s, `{"key":"n","version":1,"data_type":"number"}`)
	if _, e := s.DB.Exec("INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,cancel_requested,created_at) VALUES('cancelled','test',1,'{}','{}','[]',1,'now')"); e != nil {
		t.Fatal(e)
	}
	body := json.RawMessage(`{"tags":[{"key":"n","version":1,"value":0.5}]}`)
	if e := s.IngestResult(ctx, "cancelled", body, map[string]int{"n": 1}, []Media{m}); e == nil {
		t.Fatal("ingested cancelled result")
	}
	segments, e := s.Segments(ctx, 0)
	if e != nil || len(segments) != 0 {
		t.Fatalf("partial cancelled result: %+v %v", segments, e)
	}
}

func TestImmutableDefinitionsDoNotLoseNumericPrecision(t *testing.T) {
	s, _ := catalogStore(t)
	addDefinition(t, s, `{"key":"precise","version":1,"data_type":"json","format":{"schema":{"type":"integer","minimum":9007199254740993}}}`)
	r, e := hub.Decode(hub.TagTypes, []byte(`{"key":"precise","version":1,"data_type":"json","format":{"schema":{"type":"integer","minimum":9007199254740992}}}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AddTagType(context.Background(), r.Value.(hub.TagType), "different"); e == nil {
		t.Fatal("accepted different exact schema as identical")
	}
}
