package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ariym/mf-v2/internal/store"
)

func generate(t *testing.T, path string, args ...string) {
	t.Helper()
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	cmd := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, append(args, path)...)...)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("ffmpeg: %v %s", e, b)
	}
}
func TestImportRealMedia(t *testing.T) {
	dir := t.TempDir()
	s, e := store.Init(filepath.Join(dir, "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	source := filepath.Join(dir, "input")
	if e = os.MkdirAll(filepath.Join(source, "nested"), 0700); e != nil {
		t.Fatal(e)
	}
	audio := filepath.Join(source, "nested", "sound.wav")
	generate(t, audio, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-c:a", "pcm_s16le")
	video := filepath.Join(source, "video.mkv")
	generate(t, video, "-f", "lavfi", "-i", "color=c=black:s=16x16:r=10:d=0.2", "-f", "lavfi", "-i", "sine=duration=0.2", "-map", "0:v", "-map", "1:a", "-map", "1:a", "-c:v", "mpeg4", "-c:a", "pcm_s16le")
	b, e := os.ReadFile(audio)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(source, "extensionless"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(audio, filepath.Join(source, "alias.wav")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(source, filepath.Join(source, "nested", "cycle")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(source, "note.txt"), []byte("not media"), 0600); e != nil {
		t.Fatal(e)
	}
	importer := &Importer{Store: s}
	summary, e := importer.Import(context.Background(), source)
	if e != nil {
		t.Fatalf("%v: %+v", e, summary)
	}
	if summary.Added != 3 || summary.Skipped != 3 {
		t.Fatalf("unexpected import: %+v", summary)
	}
	rows, e := s.List(context.Background(), 100, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range rows {
		canonicalVideo, _ := filepath.EvalSymlinks(video)
		if row.FilePath == canonicalVideo {
			m, e := s.Get(context.Background(), row.ID)
			if e != nil {
				t.Fatal(e)
			}
			if m.MediaType != "video" || len(m.Audio) != 2 || len(m.Video) != 1 {
				t.Fatalf("missing streams: %+v", m)
			}
		}
	}
	summary, e = importer.Import(context.Background(), source)
	if e != nil || summary.Updated != 3 {
		t.Fatalf("reimport: %+v %v", summary, e)
	}
	if e = os.WriteFile(filepath.Join(source, "broken.mp4"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	summary, e = importer.Import(context.Background(), source)
	if e == nil || summary.Failed != 1 || summary.Updated != 3 {
		t.Fatalf("partial failure: %+v %v", summary, e)
	}
	if _, e = importer.Import(context.Background(), filepath.Join(source, "note.txt")); e == nil {
		t.Fatal("explicit non-media accepted")
	}
}
func TestProbeCancellationAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.wav")
	if e := os.WriteFile(path, []byte("data"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := (Probe{Binary: "/nonexistent/ffprobe"}).Check(); e == nil {
		t.Fatal("missing binary accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := (Probe{}).File(ctx, path); e != context.Canceled {
		t.Fatalf("cancellation: %v", e)
	}
	script := filepath.Join(t.TempDir(), "probe")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	if _, e := (Probe{Binary: script, Timeout: 50 * time.Millisecond}).File(context.Background(), path); e != context.DeadlineExceeded {
		t.Fatalf("timeout: %v", e)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("probe did not terminate")
	}
	if len(extensions) < 100 {
		t.Fatalf("incomplete catalog: %d", len(extensions))
	}
}
func TestCoverArtAndNullableMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audio")
	os.WriteFile(path, []byte("data"), 0600)
	script := filepath.Join(dir, "probe")
	body := `#!/bin/sh
printf '%s' '{"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3","sample_rate":"44100","channels":2},{"index":1,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1},"avg_frame_rate":"0/0"}],"format":{"duration":"N/A"}}'
`
	if e := os.WriteFile(script, []byte(body), 0700); e != nil {
		t.Fatal(e)
	}
	m, e := (Probe{Binary: script}).File(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	if m.MediaType != "audio" || len(m.Video) != 1 || m.Duration != nil || m.Video[0].FPS != nil {
		t.Fatalf("cover: %+v", m)
	}
}
