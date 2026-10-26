package store

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// Target freezes a submitted range independently of subsequent segment edits.
type Target struct {
	WholeFile   bool   `json:"whole_file,omitempty"`
	MediaPath   string `json:"media_path"`
	MediaFileID int64  `json:"media_file_id"`
	SegmentID   int64  `json:"segment_id,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"`
	StreamIndex *int   `json:"stream_index,omitempty"`
}
type Binding struct {
	TargetIndex  int    `json:"target_index"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Key          string `json:"key"`
	Version      int    `json:"version"`
	TagID        int64  `json:"tag_id,omitempty"`
	SHA256       string `json:"sha256"`
	Path         string `json:"path,omitempty"`
	StreamIndex  int    `json:"stream_index,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
}

func validateTagBinding(ctx context.Context, q queryer, b Binding, target Target) error {
	var value, key string
	var version int
	if err := q.QueryRowContext(ctx, `SELECT t.value,d.key,d.version FROM tag t JOIN tagtype d ON d.id=t.tag_type_id WHERE t.id=?`, b.TagID).Scan(&value, &key, &version); err != nil {
		return fmt.Errorf("prerequisite tag %d unavailable: %w", b.TagID, err)
	}
	if key != b.Key || version != b.Version || fmt.Sprintf("%x", sha256.Sum256([]byte(value))) != b.SHA256 {
		return fmt.Errorf("prerequisite tag %d changed", b.TagID)
	}
	rows, err := q.QueryContext(ctx, `SELECT s.start,s.end FROM segment_tags st JOIN segments s ON s.id=st.segment_id WHERE st.tag_id=? AND st.value_pointer='' AND s.media_file_id=?`, b.TagID, target.MediaFileID)
	if err != nil {
		return err
	}
	defer rows.Close()
	a, _ := ParseTimecode(target.Start)
	z, _ := ParseTimecode(target.End)
	for rows.Next() {
		var start, end string
		if err = rows.Scan(&start, &end); err != nil {
			return err
		}
		x, e := ParseTimecode(start)
		if e != nil {
			return e
		}
		y, e := ParseTimecode(end)
		if e != nil {
			return e
		}
		if x <= a && y >= z {
			return nil
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("prerequisite tag %d no longer covers target", b.TagID)
}

func (s *Store) ValidateTagBinding(ctx context.Context, b Binding, t Target) error {
	return validateTagBinding(ctx, s.DB, b, t)
}
