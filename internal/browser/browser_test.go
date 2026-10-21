package browser

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/store"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

func TestGraphsUseRightToLeftTimeAndDataTypes(t *testing.T) {
	numeric := Track{Name: "score", DataType: "number", Points: []Point{{Start: 0, End: 1, Value: 1.0}}}
	plot := strings.Split(Plot(numeric, 0, 10, 10), "\n")[0]
	if !strings.HasPrefix(plot, "·········") || strings.HasSuffix(plot, "·") {
		t.Fatalf("time axis reversed: %s", plot)
	}
	for _, value := range []any{true, false, "speaker A", map[string]any{"speaker": "A"}, map[string]any{"unknown": []any{1.0}}} {
		if text := Plot(Track{Points: []Point{{0, 1, value, 1}}}, 0, 2, 20); text == "" || !strings.Contains(text, "(") {
			t.Fatalf("missing categorical graph: %v %s", value, text)
		}
	}
	segments := []store.Segment{{ID: 1, Start: "00:00:00", End: "00:00:10"}, {ID: 2, Start: "00:00:01", End: "00:00:02"}}
	raw := json.RawMessage(`{"diarization":[{"start":1,"end":2,"speaker":"A"}]}`)
	tags := []store.Tag{{ID: 1, Key: "speaker", Version: 2, DataType: "json", Value: raw, Links: []store.TagLink{{SegmentID: 1}, {SegmentID: 2, ValuePointer: "/diarization/0"}}}}
	tracks := Tracks(tags, segments, false)
	if len(tracks) != 1 || len(tracks[0].Points) != 1 || tracks[0].Points[0].Start != 1 {
		t.Fatalf("did not project timed range: %+v", tracks)
	}
}

func TestDirectoryImportAndJobKeys(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	nested := filepath.Join(root, "nested")
	modules := filepath.Join(root, "mf_modules")
	for _, path := range []string{nested, modules} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Small PCM WAVs exercise actual probing without requiring an encoder.
	wav := make([]byte, 44+1600)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 8000)
	binary.LittleEndian.PutUint32(wav[28:], 16000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], 1600)
	for _, path := range []string{filepath.Join(root, "one.wav"), filepath.Join(nested, "two.wav"), filepath.Join(modules, "excluded.wav")} {
		if err := os.WriteFile(path, wav, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("not media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.mp4"), []byte("bad media"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Init(filepath.Join(dir, "mf.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var gotFlow, gotPath string
	m := New(context.Background(), s, dir, func(_ context.Context, flow, path string) (string, error) {
		gotFlow, gotPath = flow, path
		return "job-123", nil
	}, modules)
	m.files.SetItems([]list.Item{entry{path: root, name: "media", dir: true}})
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m = model.(Model)
	if cmd == nil {
		t.Fatal("i ignored a directory")
	}
	result := cmd().(actionMsg)
	if result.err == nil || !strings.Contains(result.text, "2 added, 0 updated, 1 skipped, 1 failed") {
		t.Fatalf("%+v", result)
	}
	rows, err := s.List(context.Background(), 100, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("directory media: %+v %v", rows, err)
	}
	model, _ = m.Update(result)
	m = model.(Model)
	if !strings.Contains(m.status, "2 added") || !strings.Contains(m.status, "1 failed") {
		t.Fatal(m.status)
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = model.(Model)
	if !m.prompt {
		t.Fatal("x did not prompt on directory")
	}
	m.input.SetValue("analyze@2")
	model, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(Model)
	if cmd == nil {
		t.Fatal("x ignored directory submission")
	}
	result = cmd().(actionMsg)
	if result.err != nil || gotFlow != "analyze@2" || gotPath != root || !strings.Contains(result.text, "job-123") {
		t.Fatalf("job directory: %s %s %+v", gotFlow, gotPath, result)
	}
}
func TestBrowserDirectoryNavigationResizeAndHelp(t *testing.T) {
	dir := t.TempDir()
	if e := os.Mkdir(filepath.Join(dir, "child"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "audio.wav"), []byte("not yet imported"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := store.Init(filepath.Join(dir, "mf.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	m := New(context.Background(), s, dir, nil)
	model, _ := m.Update(m.Init()())
	m = model.(Model)
	model, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m = model.(Model)
	found := false
	for _, item := range m.files.Items() {
		if item.(entry).name == "audio.wav" {
			found = true
		}
	}
	if !found {
		t.Fatal("did not list unimported media")
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = model.(Model)
	if !strings.Contains(m.details.View(), "Time starts at the right") {
		t.Fatal("missing help")
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = model.(Model)
	if !m.right {
		t.Fatal("pane did not change")
	}
	model, _ = m.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	m = model.(Model)
	if !strings.Contains(m.View(), "Enlarge") {
		t.Fatal("small terminal state")
	}
}
