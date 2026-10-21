// Package hubbrowser provides catalog selection and installation in a terminal.
package hubbrowser

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/ariym/mf-v2/internal/hub"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type Installer func(context.Context, hub.Ref) error

type item struct {
	hub.Entry
	selected map[string]bool
	results  map[string]string
}

func (i item) Title() string {
	check := "[ ]"
	if i.selected[i.Label()] {
		check = "[x]"
	}
	return check + " " + i.Label()
}
func (i item) Description() string {
	if result := i.results[i.Label()]; result != "" {
		return result
	}
	return hub.DisplayText(i.Entry.Title + " · " + i.Entry.Description)
}
func (i item) FilterValue() string {
	return i.Label() + " " + i.Entry.Title + " " + i.Entry.Description
}

type loaded struct {
	entries []hub.Entry
	err     error
}
type installed struct {
	ref hub.Ref
	err error
}

type Model struct {
	ctx           context.Context
	client        hub.Client
	kind          string
	install       Installer
	files         list.Model
	entries       []hub.Entry
	selected      map[string]bool
	results       map[string]string
	queue         []hub.Ref
	next          int
	phase, status string
	width, height int
}

func New(ctx context.Context, client hub.Client, kind string, install Installer) Model {
	l := list.New(nil, list.NewDefaultDelegate(), 90, 18)
	l.Title = "mf-hub · choose definitions"
	l.SetShowHelp(false)
	return Model{ctx: ctx, client: client, kind: kind, install: install, files: l,
		selected: map[string]bool{}, results: map[string]string{}, phase: "loading", width: 90, height: 26}
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg {
		kinds := []string{hub.TagTypes, hub.Flows, hub.Programs}
		if m.kind != "" {
			kinds = []string{m.kind}
		}
		var entries []hub.Entry
		for _, kind := range kinds {
			for offset := 0; ; {
				page, err := m.client.List(m.ctx, kind, "", 100, offset)
				if err != nil {
					return loaded{err: err}
				}
				values, err := hub.Entries(kind, page)
				if err != nil {
					return loaded{err: err}
				}
				entries = append(entries, values...)
				if len(page.Items) < 100 {
					break
				}
				offset += len(page.Items)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Label() < entries[j].Label() })
		return loaded{entries: entries}
	}
}

func (m *Model) setItems(onlySelected bool) tea.Cmd {
	items := make([]list.Item, 0, len(m.entries))
	for _, entry := range m.entries {
		if !onlySelected || m.selected[entry.Label()] {
			items = append(items, item{entry, m.selected, m.results})
		}
	}
	m.files.ResetFilter()
	m.files.Select(0)
	return m.files.SetItems(items)
}

func (m Model) installNext() tea.Cmd {
	ref := m.queue[m.next]
	return func() tea.Msg {
		if m.install == nil {
			return installed{ref, fmt.Errorf("installation unavailable")}
		}
		return installed{ref, m.install(m.ctx, ref)}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.files.SetSize(max(20, msg.Width-2), max(4, msg.Height-7))
		return m, nil
	case loaded:
		if msg.err != nil {
			m.phase, m.status = "error", msg.err.Error()
			return m, nil
		}
		m.entries, m.phase = msg.entries, "browse"
		return m, m.setItems(false)
	case installed:
		label := hub.LocalKind(msg.ref.Kind) + "/" + fmt.Sprintf("%s@%d", msg.ref.Identifier, msg.ref.Version)
		if msg.err != nil {
			m.results[label] = "Failed: " + hub.DisplayText(msg.err.Error())
		} else {
			m.results[label] = "Installed"
			delete(m.selected, label)
		}
		m.next++
		if m.next < len(m.queue) {
			return m, m.installNext()
		}
		m.phase = "done"
		m.status = fmt.Sprintf("Finished: %d succeeded, %d failed. Esc returns to the catalog.", len(m.queue)-len(m.selected), len(m.selected))
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" || (key == "q" && m.files.FilterState() != list.Filtering) {
			return m, tea.Quit
		}
		if m.phase == "loading" || m.phase == "error" || m.phase == "install" {
			return m, nil
		}
		if m.files.FilterState() != list.Filtering {
			switch key {
			case "esc":
				if m.phase != "browse" {
					m.phase, m.status = "browse", ""
					m.files.Title = "mf-hub · choose definitions"
					m.files.SetFilteringEnabled(true)
					return m, m.setItems(false)
				}
			case " ":
				if m.phase == "browse" {
					if entry, ok := m.files.SelectedItem().(item); ok {
						label := entry.Label()
						if m.selected[label] {
							delete(m.selected, label)
						} else {
							m.selected[label] = true
						}
					}
					m.status = ""
					return m, nil
				}
			case "enter":
				if len(m.selected) == 0 {
					m.status = "Select at least one definition with Space."
					return m, nil
				}
				if m.phase == "browse" {
					m.phase, m.status = "review", "Dependencies are installed automatically. Existing version conflicts are reported per selection."
					m.files.Title = "Review installation selection"
					m.files.SetFilteringEnabled(false)
					return m, m.setItems(true)
				}
				m.queue = nil
				for _, entry := range m.entries {
					if m.selected[entry.Label()] {
						m.queue = append(m.queue, entry.Ref)
					}
				}
				m.next, m.phase, m.status = 0, "install", ""
				return m, m.installNext()
			}
		}
	}
	var cmd tea.Cmd
	m.files, cmd = m.files.Update(msg)
	return m, cmd
}

func (m Model) View() string {
	if m.width < 45 || m.height < 12 {
		return "Enlarge the terminal to 45 columns × 12 rows. q quits.\n"
	}
	if m.phase == "loading" {
		return "Loading mf-hub catalog…\nCtrl-C / q quit\n"
	}
	if m.phase == "error" {
		return "Could not load mf-hub: " + hub.DisplayText(m.status) + "\nq quit\n"
	}
	footer := fmt.Sprintf("%d selected · Space select/deselect · Enter review · / filter · q quit", len(m.selected))
	if m.phase == "review" {
		footer = "Enter install selected definitions · Esc edit selection · q quit"
	}
	if m.phase == "done" {
		footer = "Esc browse · Enter retry failed selections · q quit"
	}
	status := m.status
	if m.phase == "install" {
		ref := m.queue[m.next]
		status = fmt.Sprintf("Installing %d/%d: %s/%s@%d…", m.next+1, len(m.queue), hub.LocalKind(ref.Kind), ref.Identifier, ref.Version)
		footer = "Ctrl-C / q cancel remaining installations"
	}
	return m.files.View() + "\n" + ansi.Truncate(hub.DisplayText(status), m.width, "…") + "\n" + ansi.Truncate(footer, m.width, "…") + "\n"
}

func Run(ctx context.Context, client hub.Client, kind string, install Installer, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(New(ctx, client, kind, install), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return err
}
