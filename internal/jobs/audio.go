package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ariym/mf-v2/internal/executable"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/cschleiden/go-workflows/workflow"
)

type audioResult struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	SourceSHA256 string `json:"source_sha256"`
	StreamIndex  int    `json:"stream_index"`
	SampleRate   int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
	Codec        string `json:"codec"`
}

func (e *Engine) extractAudio(ctx context.Context, in downloadedInput, log *os.File) (json.RawMessage, error) {
	if len(in.Targets) != 1 || len(in.Media) != 1 {
		return nil, workflow.NewPermanentError(fmt.Errorf("extract_audio requires one whole-file target"))
	}
	m := in.Media[0]
	t := in.Targets[0]
	probe, err := (media.Probe{Binary: in.Tools["ffprobe"].Path}).File(ctx, m.FilePath)
	if err != nil {
		return nil, workflow.NewPermanentError(err)
	}
	if len(probe.Audio) == 0 {
		return nil, workflow.NewPermanentError(fmt.Errorf("source has no audio stream: %s", m.FilePath))
	}
	selected := probe.Audio[0]
	if t.StreamIndex == nil && len(probe.Audio) > 1 {
		return nil, workflow.NewPermanentError(fmt.Errorf("multiple audio streams: select audio_stream_index"))
	}
	if t.StreamIndex != nil {
		found := false
		for _, a := range probe.Audio {
			if a.StreamIndex == *t.StreamIndex {
				selected = a
				found = true
			}
		}
		if !found {
			return nil, workflow.NewPermanentError(fmt.Errorf("audio stream %d unavailable", *t.StreamIndex))
		}
	}
	sourceSHA, err := hashFile(ctx, m.FilePath)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(e.dbPath+".mf", "audio", in.JobID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	output := filepath.Join(dir, "audio.wav")
	receipt := filepath.Join(dir, "result.json")
	var saved audioResult
	if raw, err := os.ReadFile(receipt); err == nil && json.Unmarshal(raw, &saved) == nil && saved.SourceSHA256 == sourceSHA && saved.StreamIndex == selected.StreamIndex {
		sum, err := hashFile(ctx, saved.Path)
		if err == nil && sum == saved.SHA256 {
			return canonicalJSON(saved), nil
		}
	}
	temp, err := os.CreateTemp(dir, ".audio-*.wav")
	if err != nil {
		return nil, err
	}
	path := temp.Name()
	temp.Close()
	defer os.Remove(path)
	ffmpeg := in.Tools["ffmpeg"].Path
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-y", "-i", m.FilePath, "-map", fmt.Sprintf("0:%d", selected.StreamIndex), "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", path)
	cmd.Stderr = log
	if err = executable.Run(ctx, cmd); err != nil {
		return nil, workflow.NewPermanentError(fmt.Errorf("extract audio: %w (see job logs)", err))
	}
	after, err := hashFile(ctx, m.FilePath)
	if err != nil || after != sourceSHA {
		return nil, workflow.NewPermanentError(fmt.Errorf("source changed during audio extraction"))
	}
	if err = os.Rename(path, output); err != nil {
		return nil, err
	}
	sum, err := hashFile(ctx, output)
	if err != nil {
		return nil, err
	}
	result := audioResult{Path: output, SHA256: sum, SourceSHA256: sourceSHA, StreamIndex: selected.StreamIndex, SampleRate: 16000, Channels: 1, Codec: "pcm_s16le"}
	raw := canonicalJSON(result)
	if err = os.WriteFile(receipt, raw, 0600); err != nil {
		return nil, err
	}
	return raw, nil
}
func (e *Engine) ingestAudio(ctx context.Context, in downloadedInput, raw json.RawMessage) error {
	var result audioResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	sum, err := hashFile(ctx, result.Path)
	if err != nil || sum != result.SHA256 {
		return fmt.Errorf("extracted audio missing or corrupt")
	}
	source, err := hashFile(ctx, in.Media[0].FilePath)
	if err != nil || source != result.SourceSHA256 {
		return fmt.Errorf("source changed before audio ingestion")
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cancelled bool
	if err = tx.QueryRowContext(ctx, `SELECT cancel_requested FROM job_metadata WHERE id=?`, in.JobID).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return fmt.Errorf("job cancelled before audio ingestion")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audio_artifacts(job_id,media_file_id,stream_index,path,sha256,source_sha256,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(job_id) DO NOTHING`, in.JobID, in.Media[0].ID, result.StreamIndex, result.Path, result.SHA256, result.SourceSHA256, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE job_metadata SET ingested=1 WHERE id=?`, in.JobID); err != nil {
		return err
	}
	return tx.Commit()
}
