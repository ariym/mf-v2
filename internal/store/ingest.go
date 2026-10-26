package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type ResultTag struct {
	Key               string          `json:"key"`
	Version           int             `json:"version"`
	MediaPath         string          `json:"media_path,omitempty"`
	SourceTagID       *int64          `json:"source_tag_id,omitempty"`
	SourceValueSHA256 string          `json:"source_value_sha256,omitempty"`
	Value             json.RawMessage `json:"value"`
}
type resultBundle struct {
	Tags []ResultTag `json:"tags"`
}

// IngestResult preserves schema-valid complete values and links their known
// timed projections. All definitions, segments, instances and links commit once.
func (s *Store) IngestResult(ctx context.Context, jobID string, raw json.RawMessage, pins map[string]int, media []Media) error {
	var bundle resultBundle
	if !json.Valid(raw) {
		return fmt.Errorf("invalid result JSON")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		if tagValues, ok := object["tags"]; ok {
			if e := json.Unmarshal(tagValues, &bundle.Tags); e != nil {
				return e
			}
		}
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var ingested bool
	var cancelled bool
	if e = tx.QueryRowContext(ctx, "SELECT ingested,cancel_requested FROM job_metadata WHERE id=?", jobID).Scan(&ingested, &cancelled); e != nil {
		return e
	}
	if ingested {
		return nil
	}
	if cancelled {
		return fmt.Errorf("job cancelled before result ingestion")
	}
	byPath := map[string]Media{}
	for _, m := range media {
		byPath[m.FilePath] = m
	}
	for n, t := range bundle.Tags {
		if version, ok := pins[t.Key]; !ok || version != t.Version {
			return fmt.Errorf("result references unpinned tag type %s@%d", t.Key, t.Version)
		}
		var links []TagLink
		if t.SourceTagID != nil {
			links, e = derivedTagLinks(ctx, tx, t, pins)
			if e != nil {
				return e
			}
			for _, link := range links {
				if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO job_media(job_id,media_file_id) SELECT ?,media_file_id FROM segments WHERE id=?", jobID, link.SegmentID); e != nil {
					return e
				}
			}
		} else {
			if t.SourceValueSHA256 != "" {
				return fmt.Errorf("source_value_sha256 requires source_tag_id")
			}
			var value map[string]json.RawMessage
			_ = json.Unmarshal(t.Value, &value)
			paths := []string{}
			if t.MediaPath != "" {
				paths = append(paths, t.MediaPath)
			} else {
				var embedded struct {
					Path string `json:"path"`
				}
				_ = json.Unmarshal(value["media"], &embedded)
				if embedded.Path != "" {
					paths = append(paths, embedded.Path)
				}
				if t.Key == "voice" || (t.Key == "face_profile" && t.Version == 1) {
					var v struct {
						Examples []struct {
							Media struct {
								Path string `json:"path"`
							} `json:"media"`
						} `json:"examples"`
					}
					_ = json.Unmarshal(t.Value, &v)
					for _, example := range v.Examples {
						paths = append(paths, example.Media.Path)
					}
				}
				if t.Key == "diarized_transcript" {
					for _, key := range []string{"speech", "speaker"} {
						var v struct {
							Media struct {
								Path string `json:"path"`
							} `json:"media"`
						}
						_ = json.Unmarshal(value[key], &v)
						if v.Media.Path != "" {
							paths = append(paths, v.Media.Path)
						}
					}
				}
			}
			if len(paths) == 0 && len(media) == 1 {
				paths = append(paths, media[0].FilePath)
			}
			if len(paths) == 0 {
				return fmt.Errorf("cannot associate %s result with media", t.Key)
			}
			seen := map[string]bool{}
			for _, path := range paths {
				if seen[path] {
					continue
				}
				seen[path] = true
				m, ok := byPath[path]
				if !ok {
					return fmt.Errorf("result media %q was not submitted", path)
				}
				if m.Duration == nil || *m.Duration <= 0 {
					return fmt.Errorf("media duration required to attach complete result: %s", path)
				}
				end, e := TimecodeSeconds(*m.Duration)
				if e != nil {
					return e
				}
				id, e := addSegment(ctx, tx, m.ID, "00:00:00.000000", end)
				if e != nil {
					return e
				}
				links = append(links, TagLink{SegmentID: id})
				timed, e := projections(t.Key, t.Version, t.Value)
				if e != nil {
					return e
				}
				for _, projection := range timed {
					start, e := TimecodeSeconds(projection.start)
					if e != nil {
						return e
					}
					end, e := TimecodeSeconds(projection.end)
					if e != nil {
						return e
					}
					id, e := addSegment(ctx, tx, m.ID, start, end)
					if e != nil {
						return fmt.Errorf("%s %s: %w", t.Key, projection.pointer, e)
					}
					links = append(links, TagLink{SegmentID: id, ValuePointer: projection.pointer})
				}
			}
		}
		key := strconv.Itoa(n)
		if _, e = addTag(ctx, tx, t.Key, t.Version, t.Value, links, &jobID, &key); e != nil {
			return fmt.Errorf("result tag %s: %w", t.Key, e)
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE job_metadata SET ingested=1 WHERE id=?", jobID); e != nil {
		return e
	}
	return tx.Commit()
}

// Derived values inherit only whole-value links: source JSON pointers address
// the source value and cannot be copied onto a different result schema.
func derivedTagLinks(ctx context.Context, q queryer, result ResultTag, pins map[string]int) ([]TagLink, error) {
	if *result.SourceTagID <= 0 || result.MediaPath != "" || len(result.SourceValueSHA256) != 64 {
		return nil, fmt.Errorf("derived results require a positive source_tag_id, source_value_sha256, and no media_path")
	}
	var value, key string
	var version int
	if err := q.QueryRowContext(ctx, "SELECT t.value,d.key,d.version FROM tag t JOIN tagtype d ON d.id=t.tag_type_id WHERE t.id=?", *result.SourceTagID).Scan(&value, &key, &version); err != nil {
		return nil, fmt.Errorf("source tag %d is unavailable: %w", *result.SourceTagID, err)
	}
	if pins[key] != version {
		return nil, fmt.Errorf("source tag references unpinned tag type %s@%d", key, version)
	}
	sum := sha256.Sum256([]byte(value))
	if fmt.Sprintf("%x", sum) != result.SourceValueSHA256 {
		return nil, fmt.Errorf("source tag %d changed after inference", *result.SourceTagID)
	}
	rows, err := q.QueryContext(ctx, "SELECT segment_id FROM segment_tags WHERE tag_id=? AND value_pointer='' ORDER BY segment_id", *result.SourceTagID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []TagLink
	for rows.Next() {
		var link TagLink
		if err := rows.Scan(&link.SegmentID); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("source tag %d has no whole-value segment links", *result.SourceTagID)
	}
	return links, nil
}

type projection struct {
	start, end float64
	pointer    string
}

func projections(key string, version int, raw json.RawMessage) ([]projection, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var paths []string
	switch {
	case key == "speech" && version == 2:
		paths = []string{"/transcript/segments"}
	case key == "speaker" && version == 2:
		paths = []string{"/diarization"}
	case key == "audio_events" && version == 1:
		paths = []string{"/events"}
	case key == "face_tracks" && version == 1:
		paths = []string{"/tracks"}
	case key == "diarized_transcript" && version == 2:
		paths = []string{"/speech/transcript/segments", "/speaker/diarization"}
	}
	var out []projection
	for _, path := range paths {
		v, ok := JSONPointer(value, path)
		if !ok {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			continue
		}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			aNumber, okA := m["start"].(json.Number)
			bNumber, okB := m["end"].(json.Number)
			if !okA || !okB {
				continue
			}
			a, err := aNumber.Float64()
			if err != nil {
				return nil, fmt.Errorf("invalid media start timestamp: %w", err)
			}
			b, err := bNumber.Float64()
			if err != nil {
				return nil, fmt.Errorf("invalid media end timestamp: %w", err)
			}
			out = append(out, projection{a, b, fmt.Sprintf("%s/%d", path, i)})
		}
	}
	return out, nil
}
func JSONPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	v := value
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[part]
			if !ok {
				return nil, false
			}
		case []any:
			n, e := strconv.Atoi(part)
			if e != nil || n < 0 || n >= len(x) {
				return nil, false
			}
			v = x[n]
		default:
			return nil, false
		}
	}
	return v, true
}
