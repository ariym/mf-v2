package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Audio struct {
	ID          int64  `json:"id"`
	MediaFileID int64  `json:"media_file_id"`
	StreamIndex int    `json:"stream_index"`
	SampleRate  *int   `json:"sample_rate"`
	Channels    *int   `json:"channels"`
	Codec       string `json:"codec"`
	CreatedAt   string `json:"created_at"`
	ModifiedAt  string `json:"modified_at"`
}
type Video struct {
	AttachedPic bool     `json:"attached_pic,omitempty"`
	ID          int64    `json:"id"`
	MediaFileID int64    `json:"media_file_id"`
	StreamIndex int      `json:"stream_index"`
	Width       *int     `json:"width"`
	Height      *int     `json:"height"`
	FPS         *float64 `json:"fps"`
	Codec       string   `json:"codec"`
	CreatedAt   string   `json:"created_at"`
	ModifiedAt  string   `json:"modified_at"`
}
type Media struct {
	ID         int64    `json:"id"`
	FilePath   string   `json:"file_path"`
	MediaType  string   `json:"media_type"`
	Duration   *float64 `json:"duration"`
	FileSize   int64    `json:"file_size"`
	CreatedAt  string   `json:"created_at"`
	ModifiedAt string   `json:"modified_at"`
	Audio      []Audio  `json:"audio_metadata,omitempty"`
	Video      []Video  `json:"video_metadata,omitempty"`
}

func (s *Store) Upsert(ctx context.Context, m Media) (bool, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var id int64
	var created string
	e = tx.QueryRowContext(ctx, "SELECT id,created_at FROM media_files WHERE file_path=?", m.FilePath).Scan(&id, &created)
	added := e == sql.ErrNoRows
	if e != nil && !added {
		return false, e
	}
	if added {
		created = now
	}
	e = tx.QueryRowContext(ctx, `INSERT INTO media_files(file_path,media_type,duration,file_size,created_at,modified_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(file_path) DO UPDATE SET media_type=excluded.media_type,duration=excluded.duration,file_size=excluded.file_size,modified_at=excluded.modified_at RETURNING id`, m.FilePath, m.MediaType, m.Duration, m.FileSize, created, now).Scan(&id)
	if e != nil {
		return false, e
	}
	for _, t := range []string{"file_audio_metadata", "file_video_metadata"} {
		if _, e = tx.ExecContext(ctx, "DELETE FROM "+t+" WHERE media_file_id=?", id); e != nil {
			return false, e
		}
	}
	for _, a := range m.Audio {
		if _, e = tx.ExecContext(ctx, `INSERT INTO file_audio_metadata(media_file_id,stream_index,sample_rate,channels,codec,created_at,modified_at) VALUES(?,?,?,?,?,?,?)`, id, a.StreamIndex, a.SampleRate, a.Channels, a.Codec, now, now); e != nil {
			return false, e
		}
	}
	for _, v := range m.Video {
		if _, e = tx.ExecContext(ctx, `INSERT INTO file_video_metadata(media_file_id,stream_index,width,height,fps,codec,created_at,modified_at) VALUES(?,?,?,?,?,?,?,?)`, id, v.StreamIndex, v.Width, v.Height, v.FPS, v.Codec, now, now); e != nil {
			return false, e
		}
	}
	return added, tx.Commit()
}

const mediaColumns = "id,file_path,media_type,duration,file_size,created_at,modified_at"

type scanner interface{ Scan(...any) error }

func scanMedia(r scanner) (Media, error) {
	var m Media
	e := r.Scan(&m.ID, &m.FilePath, &m.MediaType, &m.Duration, &m.FileSize, &m.CreatedAt, &m.ModifiedAt)
	return m, e
}
func (s *Store) List(ctx context.Context, limit, offset int) ([]Media, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT "+mediaColumns+" FROM media_files ORDER BY id LIMIT ? OFFSET ?", limit, offset)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ms := []Media{}
	for rows.Next() {
		m, e := scanMedia(rows)
		if e != nil {
			return nil, e
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}
func (s *Store) Get(ctx context.Context, id int64) (Media, error) {
	// A transaction gives callers one consistent file + stream snapshot during reimports.
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return Media{}, e
	}
	defer tx.Rollback()
	m, e := scanMedia(tx.QueryRowContext(ctx, "SELECT "+mediaColumns+" FROM media_files WHERE id=?", id))
	if e != nil {
		return m, e
	}
	rows, e := tx.QueryContext(ctx, "SELECT id,media_file_id,stream_index,sample_rate,channels,codec,created_at,modified_at FROM file_audio_metadata WHERE media_file_id=? ORDER BY stream_index", id)
	if e != nil {
		return m, e
	}
	for rows.Next() {
		var a Audio
		if e = rows.Scan(&a.ID, &a.MediaFileID, &a.StreamIndex, &a.SampleRate, &a.Channels, &a.Codec, &a.CreatedAt, &a.ModifiedAt); e != nil {
			rows.Close()
			return m, e
		}
		m.Audio = append(m.Audio, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return m, e
	}
	rows, e = tx.QueryContext(ctx, "SELECT id,media_file_id,stream_index,width,height,fps,codec,created_at,modified_at FROM file_video_metadata WHERE media_file_id=? ORDER BY stream_index", id)
	if e != nil {
		return m, e
	}
	for rows.Next() {
		var v Video
		if e = rows.Scan(&v.ID, &v.MediaFileID, &v.StreamIndex, &v.Width, &v.Height, &v.FPS, &v.Codec, &v.CreatedAt, &v.ModifiedAt); e != nil {
			rows.Close()
			return m, e
		}
		m.Video = append(m.Video, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return m, fmt.Errorf("video metadata: %w", e)
	}
	return m, tx.Commit()
}
