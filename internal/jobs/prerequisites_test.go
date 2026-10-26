package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/store"
)

func graphChild(req protocolRequest) {
	var input struct {
		Step string `json:"step"`
		Fail bool   `json:"fail"`
		Omit bool   `json:"omit"`
	}
	_ = json.Unmarshal(req.Input, &input)
	if req.ProtocolVersion != 2 || len(req.Targets) == 0 {
		os.Exit(11)
	}
	if input.Fail {
		fmt.Println(`{"protocol_version":2,"error":"fixture failure"}`)
		os.Exit(12)
	}
	tags := []any{}
	for i := range req.Targets {
		key := "answer"
		if input.Step == "producer" {
			if os.Getenv("MF_TEST_GRAPH_PAUSE") != "" {
				marker := filepath.Join(os.Getenv("MF_TEST_DOWNLOADED_MARKERS"), req.JobID)
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					_ = os.WriteFile(marker, []byte("running"), 0600)
					time.Sleep(30 * time.Second)
				}
			}
			key = "confidence"
			if len(req.Bindings) != 1 || req.Bindings[0].Kind != "audio" {
				os.Exit(13)
			}
		} else {
			if len(req.Bindings) == 0 || req.Bindings[0].Kind != "tag" {
				os.Exit(14)
			}
		}
		if input.Omit {
			continue
		}
		tags = append(tags, map[string]any{"key": key, "version": 1, "target_index": i, "value": 0.4})
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol_version": 2, "result": map[string]any{"tags": tags}})
	os.Exit(0)
}

func graphFixture(t *testing.T) (*store.Store, string, []int64) {
	t.Helper()
	s, path := externalFixture(t, "graph")
	ctx := context.Background()
	summary, err := (&media.Importer{Store: s}).Import(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(ctx, summary.MediaIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	template, err := s.Module(ctx, hub.Flows, "test_external")
	if err != nil {
		t.Fatal(err)
	}
	var f hub.Flow
	if err = json.Unmarshal(template.Manifest, &f); err != nil {
		t.Fatal(err)
	}
	producer := f
	producer.Key = "producer"
	producer.Execution = &hub.Execution{ProtocolVersion: 2, Requires: []hub.Requirement{{Name: "audio", AnyOf: []hub.DataSpec{{Kind: "audio", Key: "audio", Version: 1}}, Producer: hub.Producer{Builtin: "extract_audio", Version: 1}}}, Produces: []hub.DataSpec{{Kind: "tag", Key: "confidence", Version: 1}}}
	root := f
	root.Key = "consumer"
	root.TagTypes = hub.Versions{"answer": 1, "confidence": 1}
	root.Execution = &hub.Execution{ProtocolVersion: 2, Requires: []hub.Requirement{{Name: "transcript", AnyOf: []hub.DataSpec{{Kind: "tag", Key: "confidence", Version: 1}}, Producer: hub.Producer{Flow: "producer", Version: 1, Input: json.RawMessage(`{"step":"producer"}`)}}}, Produces: []hub.DataSpec{{Kind: "tag", Key: "answer", Version: 1}}}
	mods := []store.Module{}
	for _, v := range []hub.Flow{producer, root} {
		mod := template
		mod.Key = v.Key
		mod.Manifest = canonicalJSON(v)
		mods = append(mods, mod)
	}
	if err = s.RegisterInstall(ctx, []hub.TagType{{Key: "answer", Version: 1, DataType: "number"}}, mods, "", "test"); err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, r := range [][2]string{{"00:00:00.100000", "00:00:00.400000"}, {"00:00:00.500000", "00:00:00.800000"}} {
		id, err := s.AddSegment(ctx, m.ID, r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return s, path, ids
}
func countSQL(t *testing.T, s *store.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPrerequisiteGraphConcurrentReuseAndSegmentLinks(t *testing.T) {
	s, _, segments := graphFixture(t)
	e := newTestEngine(t, s)
	ctx := context.Background()
	ids := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), []int64{segments[i]})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countSQL(t, s, `SELECT count(*) FROM job_metadata WHERE flow_key='producer'`); n != 1 {
		t.Fatalf("shared producers=%d", n)
	}
	if n := countSQL(t, s, `SELECT count(*) FROM job_metadata WHERE flow_key='extract_audio'`); n != 1 {
		t.Fatalf("shared extraction=%d", n)
	}
	j, err := e.Get(ctx, ids[0])
	if err != nil || j.State != "waiting" || len(j.PrerequisiteIDs) != 1 {
		t.Fatalf("waiting job %+v %v", j, err)
	}
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	for _, id := range ids {
		waitJob(t, e, id, "completed")
	}
	if n := countSQL(t, s, `SELECT count(*) FROM audio_artifacts`); n != 1 {
		t.Fatalf("audio artifacts=%d", n)
	}
	for _, id := range ids {
		if n := countSQL(t, s, `SELECT count(*) FROM tag t JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id JOIN tagtype d ON d.id=t.tag_type_id WHERE t.source_job_id=? AND d.key='answer' AND s.start!='00:00:00.000000'`, id); n != 1 {
			t.Fatalf("segment output links=%d", n)
		}
	}
	next, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, next, "completed")
	if n := countSQL(t, s, `SELECT count(*) FROM job_metadata WHERE flow_key='producer'`); n != 1 {
		t.Fatalf("reused transcript triggered %d producers", n)
	}
}

func TestPrerequisiteGraphCancellationRetryAndRecovery(t *testing.T) {
	s, _, segments := graphFixture(t)
	e := newTestEngine(t, s)
	ctx := context.Background()
	id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Get(ctx, id)
	producer := j.PrerequisiteIDs[0]
	if err = e.CancelJob(ctx, producer); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatchGraph(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatchGraph(ctx); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, "failed")
	retry, err := e.Retry(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if retry == id {
		t.Fatal("retry reused ID")
	}
	e.Abort() // No worker has run: every submission and dependency exists only in durable metadata.
	restarted := newTestEngine(t, s)
	if err = restarted.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, restarted)
	waitJob(t, restarted, retry, "completed")
}

func TestPrerequisiteGraphCoveringResultWithoutSourceFile(t *testing.T) {
	s, path, segments := graphFixture(t)
	ctx := context.Background()
	m, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	full, err := s.AddSegment(ctx, m.ID, "00:00:00.000000", "00:00:01.000000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddTag(ctx, "confidence", 1, json.RawMessage(`0.2`), []store.TagLink{{SegmentID: full}}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	if countSQL(t, s, `SELECT count(*) FROM job_dependencies`) != 0 {
		t.Fatal("queued producer despite covering data")
	}
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	waitJob(t, e, id, "completed")
}

func TestPrerequisiteGraphChangedBindingFailsBeforeInference(t *testing.T) {
	s, _, segments := graphFixture(t)
	ctx := context.Background()
	m, _ := s.Get(ctx, 1)
	full, err := s.AddSegment(ctx, m.ID, "00:00:00.000000", "00:00:01.000000")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := s.AddTag(ctx, "confidence", 1, json.RawMessage(`0.2`), []store.TagLink{{SegmentID: full}})
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, s)
	id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err = e.dispatchGraph(ctx); err != nil {
		t.Fatal(err)
	} // Freeze the original binding in the workflow history.
	if err = s.UpdateTag(ctx, tag, json.RawMessage(`0.3`)); err != nil {
		t.Fatal(err)
	}
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	j := waitJob(t, e, id, "failed")
	if !strings.Contains(j.Error, "changed") {
		t.Fatal(j.Error)
	}
}

func TestExtractAudioMissingAndMultipleStreams(t *testing.T) {
	for _, mode := range []string{"silent", "multiple"} {
		t.Run(mode, func(t *testing.T) {
			s, _, segments := graphFixture(t)
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, "source.mkv")
			args := []string{"-v", "error", "-f", "lavfi", "-i", "color=size=16x16:duration=1"}
			if mode == "multiple" {
				args = append(args, "-f", "lavfi", "-i", "sine=duration=1", "-f", "lavfi", "-i", "sine=duration=1", "-map", "0:v", "-map", "1:a", "-map", "2:a")
			}
			args = append(args, "-c:v", "ffv1", path)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture %v %s", err, out)
			}
			summary, err := (&media.Importer{Store: s}).Import(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			segment, err := s.AddSegment(ctx, summary.MediaIDs[0], "00:00:00.100000", "00:00:00.400000")
			if err != nil {
				t.Fatal(err)
			}
			_ = segments
			e := newTestEngine(t, s)
			id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), []int64{segment})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Start(); err != nil {
				t.Fatal(err)
			}
			defer stopEngine(t, e)
			waitJob(t, e, id, "failed")
			if mode == "multiple" {
				retry, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{"audio_stream_index":1}`), []int64{segment})
				if err != nil {
					t.Fatal(err)
				}
				waitJob(t, e, retry, "completed")
			}
		})
	}
}

func TestGraphWaitingCancelDoesNotCancelPrerequisite(t *testing.T) {
	s, _, segments := graphFixture(t)
	e := newTestEngine(t, s)
	ctx := context.Background()
	id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Get(ctx, id)
	if err = e.CancelJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatchGraph(ctx); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, "cancelled")
	producer, err := e.Get(ctx, j.PrerequisiteIDs[0])
	if err != nil || producer.CancelRequested {
		t.Fatalf("prerequisite cancelled %+v %v", producer, err)
	}
	e.Abort()
}

func TestProducerFailureAndMissingPromisedOutput(t *testing.T) {
	for _, scenario := range []string{"fail", "omit"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, segments := graphFixture(t)
			ctx := context.Background()
			m, err := s.Module(ctx, hub.Flows, "consumer")
			if err != nil {
				t.Fatal(err)
			}
			var f hub.Flow
			_ = json.Unmarshal(m.Manifest, &f)
			f.Execution.Requires[0].Producer.Input = json.RawMessage(fmt.Sprintf(`{"step":"producer","%s":true}`, scenario))
			if _, err = s.DB.Exec("UPDATE flow SET manifest=? WHERE key='consumer'", string(canonicalJSON(f))); err != nil {
				t.Fatal(err)
			}
			e := newTestEngine(t, s)
			id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Start(); err != nil {
				t.Fatal(err)
			}
			defer stopEngine(t, e)
			j := waitJob(t, e, id, "failed")
			if !strings.Contains(j.Error, "prerequisite") {
				t.Fatal(j.Error)
			}
			if countSQL(t, s, "SELECT count(*) FROM instances WHERE id=?", id) != 0 {
				t.Fatal("dependent entered workflow engine before prerequisites")
			}
		})
	}
}

func TestAudioArtifactReuseAndCorruption(t *testing.T) {
	s, _, segments := graphFixture(t)
	ctx := context.Background()
	e := newTestEngine(t, s)
	id, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, e)
	waitJob(t, e, id, "completed")
	for _, corrupt := range []bool{false, true} {
		if _, err = s.DB.Exec(`DELETE FROM tag WHERE tag_type_id=(SELECT id FROM tagtype WHERE key='confidence')`); err != nil {
			t.Fatal(err)
		}
		if corrupt {
			var path string
			if err = s.DB.QueryRow(`SELECT path FROM audio_artifacts LIMIT 1`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		next, err := e.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
		if err != nil {
			t.Fatal(err)
		}
		waitJob(t, e, next, "completed")
		want := 1
		if corrupt {
			want = 2
		}
		if n := countSQL(t, s, `SELECT count(*) FROM job_metadata WHERE flow_key='extract_audio'`); n != want {
			t.Fatalf("extractions=%d, want %d", n, want)
		}
	}
}

func TestPrerequisiteGraphInterruptedProducerResumesWithSameIDs(t *testing.T) {
	s, _, segments := graphFixture(t)
	t.Setenv("MF_TEST_GRAPH_PAUSE", "1")
	ctx := context.Background()
	first := newTestEngine(t, s)
	id, err := first.SubmitSegments(ctx, "consumer", json.RawMessage(`{}`), segments[:1])
	if err != nil {
		t.Fatal(err)
	}
	root, err := first.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	producer := root.PrerequisiteIDs[0]
	if err = first.Start(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(os.Getenv("MF_TEST_DOWNLOADED_MARKERS"), producer)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stopEngine(t, first)
			t.Fatal("producer did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopEngine(t, first)
	restarted := newTestEngine(t, s)
	if err = restarted.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopEngine(t, restarted)
	waitJob(t, restarted, id, "completed")
	if countSQL(t, s, `SELECT count(*) FROM job_metadata WHERE flow_key='producer'`) != 1 || countSQL(t, s, `SELECT count(*) FROM tag WHERE source_job_id=?`, producer) != 1 {
		t.Fatal("restart duplicated producer or ingestion")
	}
}
