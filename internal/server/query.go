package server

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/render"
	"github.com/ariym/mf-v2/internal/store"
)

type selectionRequest struct {
	Mode    string            `json:"mode"`
	SQL     string            `json:"sql,omitempty"`
	Filters *selectionFilters `json:"filters,omitempty"`
	forJobs bool
}
type selectionFilters struct {
	MediaIDs []int64 `json:"media_ids,omitempty"`
	TagType  string  `json:"tag_type,omitempty"`
	Value    string  `json:"value,omitempty"`
}
type playbackClip struct {
	Media     store.Media `json:"media"`
	SegmentID *int64      `json:"segment_id,omitempty"`
	Start     string      `json:"start"`
	End       string      `json:"end"`
}

func selectionSQL(in selectionRequest) (string, []any, error) {
	if in.Mode != "media" && in.Mode != "segments" {
		return "", nil, fmt.Errorf("mode must be media or segments")
	}
	if in.SQL != "" {
		if in.Filters != nil {
			return "", nil, fmt.Errorf("supply sql or filters, not both")
		}
		return in.SQL, nil, render.SingleSelect(in.SQL)
	}
	if in.Filters == nil {
		return "", nil, fmt.Errorf("supply sql or filters")
	}
	f := in.Filters
	if f == nil {
		f = &selectionFilters{}
	}
	query := "SELECT m.id AS media_file_id"
	if in.Mode == "segments" {
		query += ", s.id AS segment_id,s.start,s.end"
	}
	query += " FROM media_files m"
	if in.Mode == "segments" {
		query += " JOIN segments s ON s.media_file_id=m.id"
	}
	query += " WHERE 1=1"
	args := []any{}
	if len(f.MediaIDs) > 1000 {
		return "", nil, fmt.Errorf("select at most 1000 media IDs")
	}
	if len(f.MediaIDs) > 0 {
		query += " AND m.id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(f.MediaIDs)), ",") + ")"
		for _, id := range f.MediaIDs {
			if id < 1 {
				return "", nil, fmt.Errorf("invalid media ID")
			}
			args = append(args, id)
		}
	}
	if f.TagType != "" || f.Value != "" {
		query += " AND EXISTS (SELECT 1 FROM segments fs JOIN segment_tags st ON st.segment_id=fs.id JOIN tag t ON t.id=st.tag_id JOIN tagtype tt ON tt.id=t.tag_type_id WHERE fs.media_file_id=m.id"
		if in.Mode == "segments" {
			query += " AND fs.id=s.id"
		}
		if f.TagType != "" {
			query += " AND tt.key=?"
			args = append(args, f.TagType)
		}
		if f.Value != "" {
			query += " AND instr(lower(CAST(t.value AS TEXT)),lower(?))>0"
			args = append(args, f.Value)
		}
		query += ")"
	}
	query += " ORDER BY m.id"
	if in.Mode == "segments" {
		query += ",length(s.start),s.start,s.id"
	}
	return query, args, nil
}

// selectClips does no filesystem access. All paths and durations come from the catalog.
func selectClips(ctx context.Context, s *store.Store, dbPath string, in selectionRequest) ([]playbackClip, error) {
	query, args, err := selectionSQL(in)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(1000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for i, col := range cols {
		col = strings.ToLower(col)
		if _, ok := index[col]; ok {
			return nil, fmt.Errorf("ambiguous column %s; alias columns explicitly", col)
		}
		index[col] = i
	}
	if _, ok := index["media_file_id"]; !ok {
		return nil, fmt.Errorf("query must return media_file_id")
	}
	if in.Mode == "segments" {
		for _, key := range []string{"start", "end"} {
			if _, ok := index[key]; !ok {
				return nil, fmt.Errorf("segment query must return start and end timecode strings")
			}
		}
	}
	type selected struct {
		id         int64
		segment    *int64
		start, end string
	}
	selectedRows := []selected{}
	for rows.Next() {
		if len(selectedRows) == 1000 {
			return nil, fmt.Errorf("query exceeds 1000 results; narrow the selection")
		}
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		id, ok := values[index["media_file_id"]].(int64)
		if !ok || id < 1 {
			return nil, fmt.Errorf("media_file_id must be a positive integer")
		}
		v := selected{id: id}
		if n, ok := index["segment_id"]; ok && values[n] != nil {
			sid, ok := values[n].(int64)
			if !ok || sid < 1 {
				return nil, fmt.Errorf("invalid segment_id")
			}
			v.segment = &sid
		}
		if in.Mode == "segments" {
			var ok bool
			v.start, ok = values[index["start"]].(string)
			if !ok {
				return nil, fmt.Errorf("start must be a timecode string")
			}
			v.end, ok = values[index["end"]].(string)
			if !ok {
				return nil, fmt.Errorf("end must be a timecode string")
			}
		}
		selectedRows = append(selectedRows, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	cache := map[int64]store.Media{}
	out := []playbackClip{}
	for _, v := range selectedRows {
		m, ok := cache[v.id]
		if !ok {
			m, err = s.Get(ctx, v.id)
			if err != nil {
				return nil, fmt.Errorf("media %d: %w", v.id, err)
			}
			cache[v.id] = m
		}
		if in.forJobs && in.Mode == "media" {
			out = append(out, playbackClip{Media: m})
			continue
		}
		if m.Duration == nil || *m.Duration <= 0 || math.IsNaN(*m.Duration) || math.IsInf(*m.Duration, 0) {
			return nil, fmt.Errorf("media %d has no known positive duration", m.ID)
		}
		if in.Mode == "media" {
			v.start = store.Timecode(0)
			v.end = store.Timecode(int64(math.Round(*m.Duration * 1e6)))
		}
		a, e := store.ParseTimecode(v.start)
		if e != nil {
			return nil, e
		}
		b, e := store.ParseTimecode(v.end)
		if e != nil {
			return nil, e
		}
		if b <= a || b > int64(math.Round(*m.Duration*1e6)) {
			return nil, fmt.Errorf("invalid range for media %d", m.ID)
		}
		if v.segment != nil {
			seg, e := s.Segment(ctx, *v.segment)
			if e != nil || seg.MediaFileID != m.ID {
				return nil, fmt.Errorf("segment_id does not belong to media %d", m.ID)
			}
		}
		out = append(out, playbackClip{m, v.segment, store.Timecode(a), store.Timecode(b)})
	}
	return out, nil
}
