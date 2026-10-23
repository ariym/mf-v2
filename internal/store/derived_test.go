package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDerivedResultIngestion(t *testing.T) {
	for _, scenario := range []string{"success", "changed", "deleted", "missing_hash", "missing_id", "negative_id", "unpinned_source", "no_whole_link", "media_conflict", "atomic", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, m := catalogStore(t)
			addDefinition(t, s, `{"key":"source","version":1,"data_type":"json","format":{"schema":{"type":"object"}}}`)
			addDefinition(t, s, `{"key":"derived","version":1,"data_type":"json","format":{"schema":{"type":"object"}}}`)
			segment, err := s.AddSegment(ctx, m.ID, "00:00:00", "00:00:10")
			if err != nil {
				t.Fatal(err)
			}
			timed, err := s.AddSegment(ctx, m.ID, "00:00:01", "00:00:02")
			if err != nil {
				t.Fatal(err)
			}
			value := json.RawMessage(`{"text":"Alice"}`)
			links := []TagLink{{SegmentID: segment}, {SegmentID: timed, ValuePointer: "/text"}}
			if scenario == "no_whole_link" {
				links = links[1:]
			}
			id, err := s.AddTag(ctx, "source", 1, value, links)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DB.Exec("INSERT INTO job_metadata(id,flow_key,flow_version,input,snapshot,paths,created_at) VALUES('derived-job','test',1,'{}','{}','[]','now')"); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(value)
			item := ResultTag{Key: "derived", Version: 1, SourceTagID: &id, SourceValueSHA256: fmt.Sprintf("%x", sum), Value: json.RawMessage(`{"names":["Alice"]}`)}
			pins := map[string]int{"source": 1, "derived": 1}
			switch scenario {
			case "changed":
				_, err = s.DB.Exec("UPDATE tag SET value='{}' WHERE id=?", id)
			case "deleted":
				_, err = s.DB.Exec("DELETE FROM tag WHERE id=?", id)
			case "missing_hash":
				item.SourceValueSHA256 = ""
			case "missing_id":
				item.SourceTagID = nil
			case "negative_id":
				negative := int64(-1)
				item.SourceTagID = &negative
			case "unpinned_source":
				delete(pins, "source")
			case "media_conflict":
				item.MediaPath = m.FilePath
			case "cancelled":
				_, err = s.DB.Exec("UPDATE job_metadata SET cancel_requested=1 WHERE id='derived-job'")
			}
			if err != nil {
				t.Fatal(err)
			}
			items := []ResultTag{item}
			if scenario == "atomic" {
				invalid := item
				invalid.SourceValueSHA256 = strings.Repeat("0", 64)
				items = append(items, invalid)
			}
			body, _ := json.Marshal(map[string]any{"tags": items})
			err = s.IngestResult(ctx, "derived-job", body, pins, nil)
			if scenario != "success" {
				if err == nil {
					t.Fatal("accepted invalid derived result")
				}
				var count int
				if err := s.DB.QueryRow("SELECT count(*) FROM tag WHERE source_job_id='derived-job'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial tags: %d %v", count, err)
				}
				if err := s.DB.QueryRow("SELECT count(*) FROM job_media WHERE job_id='derived-job'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial media: %d %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.IngestResult(ctx, "derived-job", body, pins, nil); err != nil {
				t.Fatal(err)
			}
			tags, err := s.Tags(ctx, m.ID)
			if err != nil || len(tags) != 2 {
				t.Fatalf("tags: %+v %v", tags, err)
			}
			for _, tag := range tags {
				if tag.Key == "derived" && (len(tag.Links) != 1 || tag.Links[0].SegmentID != segment || tag.Links[0].ValuePointer != "") {
					t.Fatalf("invalid inherited links: %+v", tag.Links)
				}
			}
			var count int
			if err := s.DB.QueryRow("SELECT count(*) FROM job_media WHERE job_id='derived-job' AND media_file_id=?", m.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("missing media association: %d %v", count, err)
			}
		})
	}
}
