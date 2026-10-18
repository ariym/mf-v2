// Package media probes local files and imports their streams atomically.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/store"
)

var ErrNotMedia = errors.New("file has no supported audio or video streams")

type Probe struct {
	Binary  string
	Timeout time.Duration
}
type probeJSON struct {
	Streams []struct {
		Index       int    `json:"index"`
		Type        string `json:"codec_type"`
		Codec       string `json:"codec_name"`
		SampleRate  string `json:"sample_rate"`
		Channels    int    `json:"channels"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		FPS         string `json:"avg_frame_rate"`
		RFrameRate  string `json:"r_frame_rate"`
		Duration    string `json:"duration"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// cappedBuffer consumes all output while retaining a bounded amount.
type cappedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.max - b.Len()
	if left < n {
		b.overflow = true
	}
	if left > 0 {
		b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}
func (p Probe) Check() error {
	binary := p.Binary
	if binary == "" {
		binary = "ffprobe"
	}
	if _, e := exec.LookPath(binary); e != nil {
		return fmt.Errorf("ffprobe is required for media imports: %w", e)
	}
	return nil
}
func positive(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}
func number(s string) *float64 {
	n, e := strconv.ParseFloat(s, 64)
	if e != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func rate(s string) *float64 {
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		return number(s)
	}
	if len(parts) != 2 {
		return nil
	}
	n := number(parts[0])
	d := number(parts[1])
	if n == nil || d == nil || *d == 0 {
		return nil
	}
	v := *n / *d
	if v == 0 {
		return nil
	}
	return &v
}
func (p Probe) File(ctx context.Context, path string) (store.Media, error) {
	var m store.Media
	stat, e := os.Stat(path)
	if e != nil {
		return m, e
	}
	if !stat.Mode().IsRegular() {
		return m, fmt.Errorf("not a regular file: %s", path)
	}
	binary := p.Binary
	if binary == "" {
		binary = "ffprobe"
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Local paths only: reject external playlist/protocol access during probing.
	cmd := exec.CommandContext(ctx, binary, "-v", "error", "-protocol_whitelist", "file,pipe", "-show_streams", "-show_format", "-of", "json", "-i", path)
	out := &cappedBuffer{max: 8 << 20}
	stderr := &cappedBuffer{max: 8192}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if e = cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return m, ctx.Err()
		}
		return m, fmt.Errorf("ffprobe: %w: %s", e, strings.TrimSpace(stderr.String()))
	}
	if out.overflow {
		return m, fmt.Errorf("ffprobe output exceeds 8 MiB")
	}
	var data probeJSON
	if e = json.Unmarshal(out.Bytes(), &data); e != nil {
		return m, fmt.Errorf("invalid ffprobe JSON: %w", e)
	}
	m = store.Media{FilePath: path, FileSize: stat.Size(), Duration: number(data.Format.Duration)}
	for _, s := range data.Streams {
		if s.Type == "audio" {
			sr, _ := strconv.Atoi(s.SampleRate)
			m.Audio = append(m.Audio, store.Audio{StreamIndex: s.Index, SampleRate: positive(sr), Channels: positive(s.Channels), Codec: s.Codec})
		}
		if s.Type == "video" {
			fps := rate(s.FPS)
			if fps == nil {
				fps = rate(s.RFrameRate)
			}
			m.Video = append(m.Video, store.Video{StreamIndex: s.Index, Width: positive(s.Width), Height: positive(s.Height), FPS: fps, Codec: s.Codec})
			if s.Disposition.AttachedPic == 0 {
				m.MediaType = "video"
			}
		}
		if d := number(s.Duration); d != nil && (m.Duration == nil || *d > *m.Duration) {
			m.Duration = d
		}
	}
	if m.MediaType == "" && len(m.Audio) > 0 {
		m.MediaType = "audio"
	}
	if m.MediaType == "" {
		return m, ErrNotMedia
	}
	return m, nil
}
