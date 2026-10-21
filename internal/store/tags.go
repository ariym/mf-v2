package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ariym/mf-v2/internal/hub"
)

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type TagType struct {
	ID         int64  `json:"id"`
	Definition any    `json:"definition"`
	Source     string `json:"source"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
}
type Segment struct {
	ID          int64   `json:"id"`
	MediaFileID int64   `json:"media_file_id"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Tags        []int64 `json:"tags"`
	CreatedAt   string  `json:"created_at"`
	ModifiedAt  string  `json:"modified_at"`
}
type Tag struct {
	ID          int64           `json:"id"`
	TagTypeID   int64           `json:"tag_type_id"`
	Key         string          `json:"tag_type_key"`
	Version     int             `json:"tag_type_version"`
	DataType    string          `json:"data_type"`
	Value       json.RawMessage `json:"value"`
	SourceJobID *string         `json:"source_job_id,omitempty"`
	CreatedAt   string          `json:"created_at"`
	ModifiedAt  string          `json:"modified_at"`
	Links       []TagLink       `json:"links"`
}
type TagLink struct {
	SegmentID    int64  `json:"segment_id"`
	ValuePointer string `json:"value_pointer"`
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func definition(ctx context.Context, q queryer, key string, version int) (hub.TagType, error) {
	var raw string
	var def hub.TagType
	err := q.QueryRowContext(ctx, "SELECT manifest FROM tagtype WHERE key=? AND (?=0 OR version=?)", key, version, version).Scan(&raw)
	if err != nil {
		return def, fmt.Errorf("tag type %s@%d: %w", key, version, err)
	}
	err = json.Unmarshal([]byte(raw), &def)
	return def, err
}
func (s *Store) Definition(ctx context.Context, key string, version int) (hub.TagType, error) {
	return definition(ctx, s.DB, key, version)
}
func insertTagType(ctx context.Context, q queryer, def hub.TagType, source string) error {
	raw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	if _, err = hub.Decode(hub.TagTypes, raw); err != nil {
		return err
	}
	for _, ref := range tagTypeDependencies(def) {
		if _, err = definition(ctx, q, ref.Identifier, ref.Version); err != nil {
			return err
		}
	}
	var existing string
	err = q.QueryRowContext(ctx, "SELECT manifest FROM tagtype WHERE key=?", def.Key).Scan(&existing)
	if err == nil {
		if !SameJSON([]byte(existing), raw) {
			return fmt.Errorf("tag type %s conflicts with installed definition", def.Key)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = q.ExecContext(ctx, "INSERT INTO tagtype(key,version,manifest,source,created_at,modified_at) VALUES(?,?,?,?,?,?)", def.Key, def.Version, string(raw), source, now(), now())
	return err
}
func tagTypeDependencies(def hub.TagType) []hub.Ref {
	var refs []hub.Ref
	if def.Format != nil {
		for key, v := range def.Format.TagTypes {
			refs = append(refs, hub.Ref{Kind: hub.TagTypes, Identifier: key, Version: v})
		}
	}
	return refs
}
func (s *Store) AddTagType(ctx context.Context, def hub.TagType, source string) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = insertTagType(ctx, tx, def, source); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) TagTypes(ctx context.Context) ([]TagType, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,manifest,source,created_at,modified_at FROM tagtype ORDER BY key")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []TagType{}
	for rows.Next() {
		var v TagType
		var raw string
		if e = rows.Scan(&v.ID, &raw, &v.Source, &v.CreatedAt, &v.ModifiedAt); e != nil {
			return nil, e
		}
		var def hub.TagType
		if e = json.Unmarshal([]byte(raw), &def); e != nil {
			return nil, e
		}
		v.Definition = hub.ClientView(def)
		out = append(out, v)
	}
	return out, rows.Err()
}
func segment(ctx context.Context, q queryer, id int64) (Segment, error) {
	v := Segment{Tags: []int64{}}
	err := q.QueryRowContext(ctx, "SELECT id,media_file_id,start,end,created_at,modified_at FROM segments WHERE id=?", id).Scan(&v.ID, &v.MediaFileID, &v.Start, &v.End, &v.CreatedAt, &v.ModifiedAt)
	if err != nil {
		return v, err
	}
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT tag_id FROM segment_tags WHERE segment_id=? ORDER BY tag_id", id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return v, err
		}
		v.Tags = append(v.Tags, id)
	}
	return v, rows.Err()
}
func (s *Store) Segment(ctx context.Context, id int64) (Segment, error) {
	return segment(ctx, s.DB, id)
}
func validRange(ctx context.Context, q queryer, mediaID int64, start, end string) (string, string, error) {
	a, e := ParseTimecode(start)
	if e != nil {
		return "", "", e
	}
	b, e := ParseTimecode(end)
	if e != nil {
		return "", "", e
	}
	if b <= a {
		return "", "", fmt.Errorf("segment end must be after start")
	}
	var duration *float64
	if e = q.QueryRowContext(ctx, "SELECT duration FROM media_files WHERE id=?", mediaID).Scan(&duration); e != nil {
		return "", "", e
	}
	if duration != nil && b > int64(*duration*1e6+0.5) {
		return "", "", fmt.Errorf("segment exceeds media duration")
	}
	return Timecode(a), Timecode(b), nil
}
func addSegment(ctx context.Context, q queryer, mediaID int64, start, end string) (int64, error) {
	a, b, e := validRange(ctx, q, mediaID, start, end)
	if e != nil {
		return 0, e
	}
	var id int64
	e = q.QueryRowContext(ctx, "INSERT INTO segments(media_file_id,start,end,created_at,modified_at) VALUES(?,?,?,?,?) ON CONFLICT(media_file_id,start,end) DO UPDATE SET media_file_id=excluded.media_file_id RETURNING id", mediaID, a, b, now(), now()).Scan(&id)
	return id, e
}
func (s *Store) AddSegment(ctx context.Context, mediaID int64, start, end string) (int64, error) {
	return addSegment(ctx, s.DB, mediaID, start, end)
}
func (s *Store) UpdateSegment(ctx context.Context, id int64, start, end string) error {
	v, e := s.Segment(ctx, id)
	if e != nil {
		return e
	}
	a, b, e := validRange(ctx, s.DB, v.MediaFileID, start, end)
	if e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "UPDATE segments SET start=?,end=?,modified_at=? WHERE id=?", a, b, now(), id)
	return e
}
func (s *Store) Segments(ctx context.Context, mediaID int64) ([]Segment, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id FROM segments WHERE (?=0 OR media_file_id=?)", mediaID, mediaID)
	if e != nil {
		return nil, e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []Segment{}
	for _, id := range ids {
		v, e := s.Segment(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MediaFileID != out[j].MediaFileID {
			return out[i].MediaFileID < out[j].MediaFileID
		}
		a, _ := ParseTimecode(out[i].Start)
		b, _ := ParseTimecode(out[j].Start)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
func addTag(ctx context.Context, q queryer, key string, version int, value json.RawMessage, links []TagLink, job, sourceKey *string) (int64, error) {
	if len(links) == 0 {
		return 0, fmt.Errorf("a tag must attach to at least one segment")
	}
	def, e := definition(ctx, q, key, version)
	if e != nil {
		return 0, e
	}
	if e = hub.ValidateValue(def, value, func(k string, v int) (hub.TagType, error) { return definition(ctx, q, k, v) }); e != nil {
		return 0, e
	}
	// Validate all targets before inserting the instance.
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if e = decoder.Decode(&decoded); e != nil {
		return 0, e
	}
	for _, link := range links {
		if _, ok := JSONPointer(decoded, link.ValuePointer); !ok {
			return 0, fmt.Errorf("invalid value pointer %q", link.ValuePointer)
		}
		var exists int
		if e = q.QueryRowContext(ctx, "SELECT 1 FROM segments WHERE id=?", link.SegmentID).Scan(&exists); e != nil {
			return 0, e
		}
	}
	var id int64
	e = q.QueryRowContext(ctx, `INSERT INTO tag(tag_type_id,value,source_job_id,source_key,created_at,modified_at) VALUES((SELECT id FROM tagtype WHERE key=?),?,?,?,?,?) ON CONFLICT(source_job_id,source_key) DO UPDATE SET source_key=excluded.source_key RETURNING id`, key, string(value), job, sourceKey, now(), now()).Scan(&id)
	if e != nil {
		return 0, e
	}
	for _, link := range links {
		if _, e = q.ExecContext(ctx, "INSERT OR IGNORE INTO segment_tags(segment_id,tag_id,value_pointer) VALUES(?,?,?)", link.SegmentID, id, link.ValuePointer); e != nil {
			return 0, e
		}
		if _, e = q.ExecContext(ctx, "UPDATE segments SET modified_at=? WHERE id=?", now(), link.SegmentID); e != nil {
			return 0, e
		}
	}
	return id, nil
}
func (s *Store) AddTag(ctx context.Context, key string, version int, value json.RawMessage, links []TagLink) (int64, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	id, e := addTag(ctx, tx, key, version, value, links, nil, nil)
	if e != nil {
		return 0, e
	}
	return id, tx.Commit()
}
func (s *Store) Tags(ctx context.Context, mediaID int64) ([]Tag, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT DISTINCT t.id,t.tag_type_id,d.key,d.version,json_extract(d.manifest,'$.data_type'),t.value,t.source_job_id,t.created_at,t.modified_at FROM tag t JOIN tagtype d ON d.id=t.tag_type_id JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id WHERE (?=0 OR s.media_file_id=?) ORDER BY d.key,t.id`, mediaID, mediaID)
	if e != nil {
		return nil, e
	}
	out := []Tag{}
	for rows.Next() {
		var v Tag
		var raw string
		if e = rows.Scan(&v.ID, &v.TagTypeID, &v.Key, &v.Version, &v.DataType, &raw, &v.SourceJobID, &v.CreatedAt, &v.ModifiedAt); e != nil {
			rows.Close()
			return nil, e
		}
		v.Value = json.RawMessage(raw)
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for i := range out {
		rows, e = s.DB.QueryContext(ctx, "SELECT segment_id,value_pointer FROM segment_tags WHERE tag_id=? ORDER BY segment_id,value_pointer", out[i].ID)
		if e != nil {
			return nil, e
		}
		out[i].Links = []TagLink{}
		for rows.Next() {
			var link TagLink
			if e = rows.Scan(&link.SegmentID, &link.ValuePointer); e != nil {
				rows.Close()
				return nil, e
			}
			out[i].Links = append(out[i].Links, link)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}
func (s *Store) UpdateTag(ctx context.Context, id int64, value json.RawMessage) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var key string
	var version int
	e = tx.QueryRowContext(ctx, "SELECT d.key,d.version FROM tag t JOIN tagtype d ON d.id=t.tag_type_id WHERE t.id=?", id).Scan(&key, &version)
	if e != nil {
		return e
	}
	def, e := definition(ctx, tx, key, version)
	if e != nil {
		return e
	}
	if e = hub.ValidateValue(def, value, func(k string, v int) (hub.TagType, error) { return definition(ctx, tx, k, v) }); e != nil {
		return e
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if e = decoder.Decode(&decoded); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, "SELECT value_pointer FROM segment_tags WHERE tag_id=? AND value_pointer<>''", id)
	if e != nil {
		return e
	}
	for rows.Next() {
		var pointer string
		if e = rows.Scan(&pointer); e != nil {
			rows.Close()
			return e
		}
		if _, ok := JSONPointer(decoded, pointer); !ok {
			rows.Close()
			return fmt.Errorf("updated value would invalidate linked pointer %s", pointer)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE tag SET value=?,modified_at=? WHERE id=?", string(value), now(), id)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE segments SET modified_at=? WHERE id IN(SELECT segment_id FROM segment_tags WHERE tag_id=?)", now(), id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) RemoveSegment(ctx context.Context, id int64) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, "DELETE FROM segments WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM tag WHERE NOT EXISTS(SELECT 1 FROM segment_tags WHERE tag_id=tag.id)"); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) RemoveTag(ctx context.Context, id int64) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE segments SET modified_at=? WHERE id IN(SELECT segment_id FROM segment_tags WHERE tag_id=?)", now(), id); e != nil {
		return e
	}
	r, e := tx.ExecContext(ctx, "DELETE FROM tag WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) ByPath(ctx context.Context, path string) (Media, error) {
	var id int64
	if e := s.DB.QueryRowContext(ctx, "SELECT id FROM media_files WHERE file_path=?", path).Scan(&id); e != nil {
		return Media{}, e
	}
	return s.Get(ctx, id)
}
