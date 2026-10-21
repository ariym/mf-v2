package render

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/kballard/go-shellquote"
)

func fixture(t *testing.T, path string, args ...string) {
	t.Helper()
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("FFmpeg unavailable")
	}
	args = append([]string{"-v", "error", "-y"}, args...)
	args = append(args, path)
	if out, e := exec.Command("ffmpeg", args...).CombinedOutput(); e != nil {
		t.Fatalf("fixture: %v %s", e, out)
	}
}
func TestSingleSelect(t *testing.T) {
	for _, q := range []string{"select * from segments", " /* ; */ WITH x AS (SELECT ';') SELECT * FROM x; -- comment\n", "SELECT 'a'';b', \"semi;colon\" FROM segments"} {
		if e := SingleSelect(q); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	for _, q := range []string{"", "DELETE FROM segments", "SELECT 1; DELETE FROM segments", "SELECT 1; SELECT 2", "/* unterminated", "SELECT 'unclosed"} {
		if SingleSelect(q) == nil {
			t.Fatalf("accepted %s", q)
		}
	}
}
func TestAudioSQLRenderHasNoCoverAndSafeCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio ' $(literal).wav")
	fixture(t, audio, "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "pcm_s16le")
	covered := filepath.Join(dir, "covered.mp3")
	cover := filepath.Join(dir, "cover.jpg")
	fixture(t, cover, "-f", "lavfi", "-i", "color=c=blue:s=32x32:d=0.1", "-frames:v", "1", "-threads", "1")
	fixture(t, covered, "-i", audio, "-i", cover, "-map", "0:a", "-map", "1:v", "-c:a", "libmp3lame", "-c:v", "copy", "-disposition:v", "attached_pic")
	s, e := store.Init(filepath.Join(dir, "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = (&media.Importer{Store: s}).Import(ctx, covered); e != nil {
		t.Fatal(e)
	}
	canonical := covered
	if p, e := filepath.EvalSymlinks(covered); e == nil {
		canonical = p
	}
	m, e := s.ByPath(ctx, canonical)
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.AddSegment(ctx, m.ID, "00:00:00.100001", "00:00:00.600001")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AddSegment(ctx, m.ID, "00:00:01", "00:00:01.5"); e != nil {
		t.Fatal(e)
	}
	clips, e := Load(ctx, s, filepath.Join(dir, "mf.db"), "SELECT * FROM segments ORDER BY id DESC")
	if e != nil {
		t.Fatal(e)
	}
	if len(clips) != 2 || clips[0].Start != 1_000_000 || !AudioOnly(clips) {
		t.Fatalf("clips %+v", clips)
	}
	outPath := filepath.Join(dir, "out ' $(literal).mp3")
	command, e := Build(clips, outPath, config.DefaultRender(), false)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := shellquote.Split(command.String())
	if e != nil || len(parsed) != len(command.Args)+1 {
		t.Fatalf("quoted command: %v", e)
	}
	for i, arg := range command.Args {
		if parsed[i+1] != arg {
			t.Fatalf("argument %d changed", i)
		}
	}
	var progress bytes.Buffer
	if e = command.Run(ctx, &progress); e != nil {
		t.Fatalf("render: %v\n%s\n%s", e, progress.String(), command.String())
	}
	rendered, e := (media.Probe{}).File(ctx, outPath)
	if e != nil {
		t.Fatal(e)
	}
	if rendered.MediaType != "audio" || len(rendered.Video) != 0 || rendered.Duration == nil || *rendered.Duration < 0.95 || *rendered.Duration > 1.2 {
		t.Fatalf("MP3 retained art or duration wrong: %+v", rendered)
	}
	if _, e = Load(ctx, s, filepath.Join(dir, "mf.db"), "WITH x AS (SELECT 1) DELETE FROM segments RETURNING *"); e == nil {
		t.Fatal("read-only query wrote data")
	}
	v, e := s.Segment(ctx, a)
	if e != nil || v.ID != a {
		t.Fatal("query modified catalog")
	}
	if e = command.Run(ctx, &progress); e == nil {
		t.Fatal("overwrote output without permission")
	}
	if _, e = Build(clips, covered, config.DefaultRender(), true); e == nil {
		t.Fatal("allowed overwriting source")
	}
}
func TestMixedRenderNormalizesAndPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "red.mp4")
	audio := filepath.Join(dir, "audio.wav")
	fixture(t, video, "-f", "lavfi", "-i", "color=c=red:s=100x80:r=12:d=1", "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	fixture(t, audio, "-f", "lavfi", "-i", "sine=frequency=880:duration=1", "-c:a", "pcm_s16le")
	ctx := context.Background()
	v, e := (media.Probe{}).File(ctx, video)
	if e != nil {
		t.Fatal(e)
	}
	a, e := (media.Probe{}).File(ctx, audio)
	if e != nil {
		t.Fatal(e)
	}
	settings := config.DefaultRender()
	settings.Width = 160
	settings.Height = 120
	settings.FPS = 10
	clips := []Clip{{v, 0, 1_000_000}, {a, 0, 1_000_000}}
	command, e := Build(clips, filepath.Join(dir, "mixed.mp4"), settings, false)
	if e != nil {
		t.Fatal(e)
	}
	var progress bytes.Buffer
	if e = command.Run(ctx, &progress); e != nil {
		t.Fatalf("%v\n%s", e, progress.String())
	}
	m, e := (media.Probe{}).File(ctx, command.Output)
	if e != nil {
		t.Fatal(e)
	}
	if m.Duration == nil || *m.Duration < 1.95 || *m.Duration > 2.15 || len(m.Audio) != 1 || len(m.Video) != 1 || *m.Video[0].Width != 160 {
		t.Fatalf("mixed output %+v", m)
	}
	for _, tc := range []struct {
		at  string
		red bool
	}{{"0.2", true}, {"1.2", false}} {
		out, e := exec.Command("ffmpeg", "-v", "error", "-ss", tc.at, "-i", command.Output, "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1").Output()
		if e != nil || len(out) < 160*120*3 {
			t.Fatalf("frame %s: %v", tc.at, e)
		}
		pixel := (60*160 + 80) * 3
		r, g, b := out[pixel], out[pixel+1], out[pixel+2]
		if tc.red {
			if r < 200 || g > 40 || b > 40 {
				t.Fatalf("wrong first clip color: %v", out[pixel:pixel+3])
			}
		} else if r > 30 || g > 30 || b > 30 {
			t.Fatalf("audio clip was not black: %v", out[pixel:pixel+3])
		}
	}
	encoded, _ := json.Marshal(command)
	if !strings.Contains(string(encoded), "libx264") {
		t.Fatal("missing configured codec")
	}
}
