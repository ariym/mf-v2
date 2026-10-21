// Package browser provides the live directory and tag timeline terminal UI.
package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/ariym/mf-v2/internal/media"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type Runner func(context.Context, string, string) (string, error)
type entry struct {
	path, name   string
	dir, catalog bool
}

func (e entry) Title() string {
	prefix := "  "
	if e.dir {
		prefix = "▸ "
	} else if e.catalog {
		prefix = "● "
	}
	return prefix + safe(e.name)
}
func (e entry) Description() string {
	if e.dir {
		return "directory · i import all · x run flow"
	}
	if e.catalog {
		return "in catalog"
	}
	return "not imported · i to add"
}
func (e entry) FilterValue() string { return e.name }

type directoryMsg struct {
	path  string
	items []list.Item
	err   error
}
type detailsMsg struct {
	path     string
	media    store.Media
	tags     []store.Tag
	segments []store.Segment
	err      error
}
type actionMsg struct {
	text string
	err  error
}
type Model struct {
	ctx                                   context.Context
	s                                     *store.Store
	run                                   Runner
	excludeDirs                           []string
	path, initialFile                     string
	files                                 list.Model
	details                               viewport.Model
	input                                 textinput.Model
	selected                              detailsMsg
	status                                string
	width, height                         int
	right, help, inspect, history, prompt bool
	tagIndex                              int
	start, span                           float64
}

func safe(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text)
}
func New(ctx context.Context, s *store.Store, path string, run Runner, excludeDirs ...string) Model {
	l := list.New(nil, list.NewDefaultDelegate(), 30, 20)
	l.Title = "Directory"
	l.SetShowHelp(false)
	l.SetShowStatusBar(true)
	field := textinput.New()
	field.Placeholder = "installed flow key"
	field.CharLimit = 128
	return Model{ctx: ctx, s: s, run: run, excludeDirs: excludeDirs, path: path, files: l, details: viewport.New(60, 20), input: field, width: 100, height: 28}
}
func (m Model) Init() tea.Cmd { return m.readDirectory(m.path) }
func (m Model) readDirectory(path string) tea.Cmd {
	return func() tea.Msg {
		abs, e := filepath.Abs(path)
		if e != nil {
			return directoryMsg{path: path, err: e}
		}
		st, e := os.Stat(abs)
		if e != nil {
			return directoryMsg{path: abs, err: e}
		}
		if !st.IsDir() {
			abs = filepath.Dir(abs)
		}
		entries, e := os.ReadDir(abs)
		if e != nil {
			return directoryMsg{path: abs, err: e}
		}
		known := map[string]bool{}
		rows, e := m.s.DB.QueryContext(m.ctx, "SELECT file_path FROM media_files")
		if e != nil {
			return directoryMsg{path: abs, err: e}
		}
		for rows.Next() {
			var p string
			if e = rows.Scan(&p); e != nil {
				rows.Close()
				return directoryMsg{path: abs, err: e}
			}
			known[p] = true
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return directoryMsg{path: abs, err: e}
		}
		items := []list.Item{}
		if parent := filepath.Dir(abs); parent != abs {
			items = append(items, entry{path: parent, name: "..", dir: true})
		}
		var ordinary []entry
		for _, d := range entries {
			p := filepath.Join(abs, d.Name())
			info, e := os.Stat(p)
			if e != nil {
				continue
			}
			resolved, e := filepath.EvalSymlinks(p)
			if e != nil {
				continue
			}
			// FFprobe accepts formats/extensions absent from the static catalog.
			// Show regular files so those can still be imported deliberately.
			if info.IsDir() || info.Mode().IsRegular() {
				ordinary = append(ordinary, entry{p, d.Name(), info.IsDir(), known[resolved]})
			}
		}
		sort.Slice(ordinary, func(i, j int) bool {
			if ordinary[i].dir != ordinary[j].dir {
				return ordinary[i].dir
			}
			return ordinary[i].name < ordinary[j].name
		})
		for _, e := range ordinary {
			items = append(items, e)
		}
		return directoryMsg{path: abs, items: items}
	}
}
func (m Model) selectedEntry() (entry, bool) { v, ok := m.files.SelectedItem().(entry); return v, ok }
func (m Model) readDetails(item entry) tea.Cmd {
	return func() tea.Msg {
		msg := detailsMsg{path: item.path}
		if item.dir {
			return msg
		}
		path, e := filepath.EvalSymlinks(item.path)
		if e != nil {
			msg.err = e
			return msg
		}
		msg.media, e = m.s.ByPath(m.ctx, path)
		if e != nil {
			if e == sql.ErrNoRows {
				msg.err = fmt.Errorf("Not imported. Press i to add this file or x to run a flow.")
			} else {
				msg.err = e
			}
			return msg
		}
		msg.tags, e = m.s.Tags(m.ctx, msg.media.ID)
		if e == nil {
			msg.segments, e = m.s.Segments(m.ctx, msg.media.ID)
		}
		msg.err = e
		return msg
	}
}
func (m *Model) layout() {
	left := max(20, m.width/3)
	m.files.SetSize(left-4, max(5, m.height-8))
	m.details.Width = max(10, m.width-left-6)
	m.details.Height = max(5, m.height-8)
	m.input.Width = max(10, m.width-14)
	m.refresh()
}
func (m *Model) setContent(text string) {
	m.details.SetContent(ansi.Wrap(text, max(1, m.details.Width), " "))
}
func (m *Model) refresh() {
	if m.help {
		m.setContent("Directory: ↑/↓ or j/k select · Enter opens a directory\nBackspace parent · / filter · i import · x run an installed flow\ni / x on directories recursively import all valid media.\nTab switch pane · r refresh · q quit\n\nTimeline: ←/→ pan · +/- zoom · 0 reset\nn/p select tag instance · Enter inspect JSON · H toggle older results\n\nTime starts at the right and increases toward the left.\nNumeric values use plots; Booleans and categories use interval lanes.\nTag sets expand into child tracks; structured results expose timed events.\n\n? closes help")
		return
	}
	if m.selected.err != nil {
		m.setContent(safe(m.selected.err.Error()))
		return
	}
	if m.selected.media.ID == 0 {
		m.setContent("Select a media file to inspect its tags and timelines.")
		return
	}
	var out strings.Builder
	file := m.selected.media
	fmt.Fprintf(&out, "%s\n%s · %d bytes · %d segments\n", safe(filepath.Base(file.FilePath)), file.MediaType, file.FileSize, len(m.selected.segments))
	if len(m.selected.tags) == 0 {
		out.WriteString("\nNo tags yet. Press x to run an installed flow.")
		m.setContent(out.String())
		return
	}
	if m.inspect {
		index := m.tagIndex % len(m.selected.tags)
		tag := m.selected.tags[index]
		fmt.Fprintf(&out, "\nTag #%d · %s@%d\n", tag.ID, tag.Key, tag.Version)
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, tag.Value, "", "  ")
		text := pretty.String()
		if len(text) > 64<<10 {
			text = text[:64<<10] + "\n… use mf tags show for the complete value"
		}
		out.WriteString(safeMultiline(text))
		m.setContent(out.String())
		return
	}
	span := m.span
	if span <= 0 {
		span = 1
		if file.Duration != nil {
			span = *file.Duration
		}
	}
	width := max(8, m.details.Width-2)
	left, _ := store.TimecodeSeconds(m.start + span)
	right, _ := store.TimecodeSeconds(m.start)
	padding := max(1, width-len(left)-len(right))
	fmt.Fprintf(&out, "\n%s%s%s\n← time increases\n", left, strings.Repeat(" ", padding), right)
	for _, track := range Tracks(m.selected.tags, m.selected.segments, m.history) {
		fmt.Fprintf(&out, "\n%s [%s]\n%s\n", track.Name, track.DataType, Plot(track, m.start, span, width))
	}
	index := m.tagIndex % len(m.selected.tags)
	tag := m.selected.tags[index]
	fmt.Fprintf(&out, "\nSelected tag #%d %s@%d\nn/p select · Enter inspect\n", tag.ID, tag.Key, tag.Version)
	m.setContent(out.String())
}
func safeMultiline(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '�'
		}
		return r
	}, text)
}
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
		m.layout()
		return m, nil
	case directoryMsg:
		if v.err != nil {
			m.status = v.err.Error()
			return m, nil
		}
		m.path = v.path
		m.files.Title = safe(v.path)
		cmd := m.files.SetItems(v.items)
		if m.initialFile != "" {
			for n, item := range v.items {
				if item.(entry).path == m.initialFile {
					m.files.Select(n)
					break
				}
			}
			m.initialFile = ""
		}
		m.selected = detailsMsg{}
		m.layout()
		if item, ok := m.selectedEntry(); ok {
			return m, tea.Batch(cmd, m.readDetails(item))
		}
		return m, cmd
	case detailsMsg:
		if item, ok := m.selectedEntry(); ok && item.path == v.path {
			m.selected = v
			m.tagIndex = 0
			m.start = 0
			m.span = 0
			m.details.GotoTop()
			m.refresh()
		}
		return m, nil
	case actionMsg:
		if v.err != nil {
			m.status = v.text + " · " + v.err.Error()
		} else {
			m.status = v.text
		}
		return m, m.readDirectory(m.path)
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.prompt {
			switch key {
			case "esc":
				m.prompt = false
				return m, nil
			case "enter":
				flow := m.input.Value()
				item, ok := m.selectedEntry()
				m.prompt = false
				if !ok || flow == "" {
					return m, nil
				}
				m.status = "Submitting job…"
				return m, func() tea.Msg {
					if m.run == nil {
						return actionMsg{err: fmt.Errorf("job submission unavailable")}
					}
					id, e := m.run(m.ctx, flow, item.path)
					return actionMsg{text: "Job submitted: " + id, err: e}
				}
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		if m.files.FilterState() != list.Filtering {
			switch key {
			case "q":
				return m, tea.Quit
			case "?":
				m.help = !m.help
				m.refresh()
				return m, nil
			case "tab":
				m.right = !m.right
				return m, nil
			case "r":
				return m, m.readDirectory(m.path)
			case "H":
				m.history = !m.history
				m.refresh()
				return m, nil
			case "n", "p":
				if len(m.selected.tags) > 0 {
					step := 1
					if key == "p" {
						step = len(m.selected.tags) - 1
					}
					m.tagIndex = (m.tagIndex + step) % len(m.selected.tags)
					m.refresh()
				}
				return m, nil
			case "x":
				m.prompt = true
				m.input.SetValue("")
				m.input.Focus()
				return m, textinput.Blink
			case "i":
				item, ok := m.selectedEntry()
				if !ok {
					return m, nil
				}
				m.status = "Importing…"
				return m, func() tea.Msg {
					summary, e := (&media.Importer{Store: m.s, ExcludeDirs: m.excludeDirs}).Import(m.ctx, item.path)
					text := fmt.Sprintf("%s: %d added, %d updated, %d skipped, %d failed", item.name, summary.Added, summary.Updated, summary.Skipped, summary.Failed)
					return actionMsg{text: text, err: e}
				}
			case "backspace":
				if !m.right {
					return m, m.readDirectory(filepath.Dir(m.path))
				}
			case "enter":
				if m.right {
					m.inspect = !m.inspect
					m.details.GotoTop()
					m.refresh()
					return m, nil
				}
				if item, ok := m.selectedEntry(); ok && item.dir {
					return m, m.readDirectory(item.path)
				}
			case "+", "=", "-", "0", "left", "right":
				if m.right {
					duration := 1.0
					if m.selected.media.Duration != nil {
						duration = *m.selected.media.Duration
					}
					if m.span <= 0 {
						m.span = duration
					}
					switch key {
					case "+", "=":
						m.span = max(0.001, m.span/2)
					case "-":
						m.span = min(duration, m.span*2)
					case "0":
						m.span = duration
						m.start = 0
					case "left":
						m.start += m.span / 4
					case "right":
						m.start -= m.span / 4
					}
					m.start = max(0, min(m.start, max(0, duration-m.span)))
					m.refresh()
					return m, nil
				}
			}
		}
	}
	var cmd tea.Cmd
	if m.right {
		m.details, cmd = m.details.Update(msg)
		return m, cmd
	}
	before, okBefore := m.selectedEntry()
	m.files, cmd = m.files.Update(msg)
	after, okAfter := m.selectedEntry()
	if okAfter && (!okBefore || before.path != after.path) {
		m.selected = detailsMsg{}
		m.refresh()
		return m, tea.Batch(cmd, m.readDetails(after))
	}
	return m, cmd
}
func (m Model) View() string {
	if m.width < 55 || m.height < 12 {
		return "Enlarge the terminal to at least 55 columns × 12 rows. Press q to quit.\n"
	}
	leftWidth := max(20, m.width/3)
	leftStyle := lipgloss.NewStyle().Width(leftWidth - 2).Height(m.height - 6).Border(lipgloss.RoundedBorder())
	rightStyle := lipgloss.NewStyle().Width(max(10, m.width-leftWidth-4)).Height(m.height - 6).Border(lipgloss.RoundedBorder())
	if m.right {
		rightStyle = rightStyle.BorderForeground(lipgloss.Color("6"))
	} else {
		leftStyle = leftStyle.BorderForeground(lipgloss.Color("6"))
	}
	footer := "Tab pane · i import · x job · ? help · q quit"
	if m.prompt {
		footer = "Run flow: " + m.input.View()
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, leftStyle.Render(m.files.View()), rightStyle.Render(m.details.View())) + "\n" + safe(m.status) + "\n" + footer + "\n"
}
func Run(ctx context.Context, s *store.Store, path string, in io.Reader, out io.Writer, run Runner, excludeDirs ...string) error {
	abs, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	m := New(ctx, s, abs, run, excludeDirs...)
	if st, e := os.Stat(abs); e == nil && !st.IsDir() {
		m.initialFile = abs
		m.path = filepath.Dir(abs)
	}
	_, e = tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return e
}
