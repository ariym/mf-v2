package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/hub"
)

func TestHubListTableAndJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tags" || r.URL.Query().Get("offset") != "2" || r.URL.Query().Get("limit") != "25" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		json.NewEncoder(w).Encode(hub.Page{Items: []json.RawMessage{json.RawMessage(`{"key":"confidence","version":2,"data_type":"number","title":"Confidence","desc":"A score"}`)}, Limit: 25, Offset: 2})
	}))
	defer server.Close()
	for _, asJSON := range []bool{false, true} {
		args := []string{"hub", "--url", server.URL, "list", "tagtypes", "--limit", "25", "--offset", "2"}
		if asJSON {
			args = append(args, "--json")
		}
		var out bytes.Buffer
		if err := Execute(context.Background(), args, strings.NewReader(""), &out, &out); err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var page hub.Page
			if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Offset != 2 {
				t.Fatalf("JSON output: %s (%v)", out.String(), err)
			}
		} else {
			for _, want := range []string{"INSTALL NAME", "confidence@2", "Confidence · number", "A score", "mf tagtypes install <install-name>", "offset 2"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %s", want, out.String())
				}
			}
		}
	}
}

func TestHubTableSanitizesMetadataAndEmptyCatalog(t *testing.T) {
	raw := json.RawMessage(`{"key":"confidence","version":1,"data_type":"number","title":"\u001b[31mRed\u001b[0m\nTitle","desc":"first\tsecond"}`)
	var out bytes.Buffer
	if err := printHubTable(&out, "tagtypes", hub.Page{Items: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "Red Title") || !strings.Contains(out.String(), "first second") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := printHubTable(&out, "flows", hub.Page{}); err != nil || !strings.Contains(out.String(), "No definitions found") {
		t.Fatalf("%s %v", out.String(), err)
	}
}

func TestHubBrowseRequiresTerminal(t *testing.T) {
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	if err := Execute(context.Background(), []string{"hub", "browse"}, strings.NewReader(""), &out, &out); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("%v", err)
	}
}
