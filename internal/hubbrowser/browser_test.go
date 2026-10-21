package hubbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/hub"
	tea "github.com/charmbracelet/bubbletea"
)

func update(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}
func key(text string) tea.KeyMsg {
	if text == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	if text == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}
func TestSelectionReviewInstallAndRetry(t *testing.T) {
	var calls []hub.Ref
	fail := true
	m := New(context.Background(), hub.Client{}, "", func(_ context.Context, ref hub.Ref) error {
		calls = append(calls, ref)
		if ref.Kind == hub.Programs && fail {
			return errors.New("download unavailable")
		}
		return nil
	})
	entries := []hub.Entry{{Ref: hub.Ref{Kind: hub.TagTypes, Identifier: "score", Version: 2}}, {Ref: hub.Ref{Kind: hub.Programs, Identifier: "analyzer", Version: 1}}}
	m, _ = update(m, loaded{entries: entries})
	m, _ = update(m, key("enter"))
	if m.phase != "browse" || !strings.Contains(m.status, "Select at least one") {
		t.Fatal("empty selection advanced")
	}
	m, _ = update(m, key(" "))
	if !strings.Contains(m.View(), "[x] tagtypes/score@2") {
		t.Fatal(m.View())
	}
	m, _ = update(m, key(" "))
	if len(m.selected) != 0 {
		t.Fatal("Space did not deselect")
	}
	m, _ = update(m, key(" "))
	m.files.Select(1)
	m, _ = update(m, key(" "))
	m, cmd := update(m, key("enter"))
	if m.phase != "review" || len(calls) != 0 || len(m.files.Items()) != 2 {
		t.Fatal("selection was not reviewed before install")
	}
	m, _ = update(m, key("esc"))
	if m.phase != "browse" || len(m.selected) != 2 {
		t.Fatal("back lost selection")
	}
	m, _ = update(m, key("enter"))
	m, cmd = update(m, key("enter"))
	if cmd == nil || m.phase != "install" {
		t.Fatal("did not start installation")
	}
	for cmd != nil {
		m, cmd = update(m, cmd())
	}
	if len(calls) != 2 || calls[0] != entries[0].Ref || calls[1] != entries[1].Ref || m.phase != "done" || len(m.selected) != 1 {
		t.Fatalf("unexpected installation: calls=%v phase=%s selected=%v", calls, m.phase, m.selected)
	}
	if !strings.Contains(m.View(), "download unavailable") || !strings.Contains(m.View(), "1 succeeded, 1 failed") {
		t.Fatal(m.View())
	}
	fail = false
	m, cmd = update(m, key("enter"))
	for cmd != nil {
		m, cmd = update(m, cmd())
	}
	if len(calls) != 3 || calls[2] != entries[1].Ref || len(m.selected) != 0 {
		t.Fatalf("retry reran successful selections: %v", calls)
	}
}

func TestCatalogLoadsAllPagesAndKinds(t *testing.T) {
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path]++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page := hub.Page{Limit: 100, Offset: offset}
		if r.URL.Path == "/api/v1/tags" {
			for n := offset; n < min(offset+100, 103); n++ {
				page.Items = append(page.Items, json.RawMessage(fmt.Sprintf(`{"key":"type_%03d","version":1,"data_type":"boolean"}`, n)))
			}
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	m := New(context.Background(), hub.Client{URL: server.URL}, "", nil)
	m, _ = update(m, m.Init()())
	if len(m.entries) != 103 || seen["/api/v1/tags"] != 2 || seen["/api/v1/flows"] != 1 || seen["/api/v1/programs"] != 1 {
		t.Fatalf("missing catalog pages: %d %v", len(m.entries), seen)
	}
	seen = map[string]int{}
	m = New(context.Background(), hub.Client{URL: server.URL}, hub.Flows, nil)
	m, _ = update(m, m.Init()())
	if len(seen) != 1 || seen["/api/v1/flows"] != 1 || m.phase != "browse" {
		t.Fatalf("kind filter: %v", seen)
	}
}

func TestCatalogLoadErrorAndResize(t *testing.T) {
	m := New(context.Background(), hub.Client{}, "", nil)
	m, _ = update(m, loaded{err: errors.New("hub offline")})
	if !strings.Contains(m.View(), "hub offline") {
		t.Fatal(m.View())
	}
	m, _ = update(m, tea.WindowSizeMsg{Width: 20, Height: 8})
	if !strings.Contains(m.View(), "Enlarge") {
		t.Fatal(m.View())
	}
}
