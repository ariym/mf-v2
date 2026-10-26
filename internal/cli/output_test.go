package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/spf13/cobra"
)

func outputConfig(t *testing.T, apiURL string) config.Config {
	t.Helper()
	dir := t.TempDir()
	c := config.Defaults()
	c.Path = filepath.Join(dir, "mf.yaml")
	c.SQLitePath = filepath.Join(dir, "mf.db")
	c.ModulesPath = filepath.Join(dir, "mf_modules")
	if apiURL != "" {
		u, err := url.Parse(apiURL)
		if err != nil {
			t.Fatal(err)
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			t.Fatal(err)
		}
		c.ServerIP = host
		c.ServerPort, err = strconv.Atoi(port)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := config.Write(c.Path, c); err != nil {
		t.Fatal(err)
	}
	s, err := store.Init(c.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCatalogOutputFormats(t *testing.T) {
	c := outputConfig(t, "")
	s, err := store.Open(c.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	duration := 10.0
	if _, err = s.Upsert(ctx, store.Media{FilePath: "/media/interview.wav", MediaType: "audio", Duration: &duration}); err != nil {
		t.Fatal(err)
	}
	media, err := s.ByPath(ctx, "/media/interview.wav")
	if err != nil {
		t.Fatal(err)
	}
	segment, err := s.AddSegment(ctx, media.ID, "00:00:01", "00:00:02")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddTagType(ctx, hub.TagType{Key: "confidence", Version: 1, DataType: "number"}, "local"); err != nil {
		t.Fatal(err)
	}
	tag, err := s.AddTag(ctx, "confidence", 1, json.RawMessage(`0.95`), []store.TagLink{{SegmentID: segment}})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"flows", "programs"} {
		if err = s.RegisterInstall(ctx, nil, []store.Module{{Kind: kind, Key: "sample", Version: 1, Manifest: json.RawMessage(`{"key":"sample","version":1}`), Source: "local", Executable: "/bin/echo", OS: "darwin", Arch: "arm64"}}, "", "local"); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"tagtypes", "list"}, []string{"KEY", "confidence", "number"}},
		{[]string{"tagtypes", "show", "confidence"}, []string{"Key:", "confidence", "Data Type:", "number"}},
		{[]string{"segments", "list"}, []string{"MEDIA ID", "START", "00:00:01"}},
		{[]string{"segments", "show", fmt.Sprint(segment)}, []string{"Media File ID:", "Start:", "Tags:"}},
		{[]string{"tags", "list"}, []string{"TAG TYPE", "confidence", "0.95"}},
		{[]string{"tags", "show", fmt.Sprint(tag)}, []string{"Tag Type Key:", "confidence", "Links:", "Segment ID:", "0.95"}},
		{[]string{"flows", "list"}, []string{"KEY", "sample", "EXECUTABLE", "/bin/echo"}},
		{[]string{"flows", "show", "sample"}, []string{"Key:", "sample", "Manifest:"}},
		{[]string{"programs", "list"}, []string{"KEY", "sample", "EXECUTABLE", "/bin/echo"}},
		{[]string{"programs", "show", "sample"}, []string{"Key:", "sample", "Manifest:"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				args := append([]string{"--config", c.Path}, tc.args...)
				if asJSON {
					args = append(args, "--json")
				}
				var out bytes.Buffer
				if err := Execute(ctx, args, strings.NewReader(""), &out, &out); err != nil {
					t.Fatal(err)
				}
				if json.Valid(out.Bytes()) != asJSON {
					t.Fatalf("--json=%v: %s", asJSON, out.String())
				}
				if !asJSON {
					for _, want := range tc.want {
						if !strings.Contains(out.String(), want) {
							t.Errorf("missing %q in %s", want, out.String())
						}
					}
				}
			}
		})
	}
}

func TestJobsOutputFormats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/jobs":
			if r.URL.Query().Get("state") != "completed" || r.URL.Query().Get("offset") != "2" {
				t.Errorf("lost filters: %s", r.URL)
			}
			fmt.Fprint(w, `{"items":[{"id":"job-1","flow":"transcribe","flow_version":2,"state":"completed","paths":["/media/a.wav"],"created_at":"2026-10-25T12:00:00Z"}],"limit":25,"offset":2}`)
		default:
			state := "completed"
			if strings.HasSuffix(r.URL.Path, "/failed") {
				state = "failed"
			} else if strings.HasSuffix(r.URL.Path, "/cancelled") {
				state = "cancelled"
			}
			fmt.Fprintf(w, `{"id":"job-1","state":%q,"result":{"count":9007199254740993,"labels":["alice","bob"]},"error":"diagnostic","created_at":"2026-10-25T12:00:00Z"}`, state)
		}
	}))
	defer server.Close()
	c := outputConfig(t, server.URL)
	for _, action := range []string{"list", "show", "wait"} {
		for _, state := range []string{"completed", "failed", "cancelled"} {
			if action == "list" && state != "completed" {
				continue
			}
			for _, asJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s_%s_json=%v", action, state, asJSON), func(t *testing.T) {
					args := []string{"--config", c.Path, "jobs", action, state}
					if action == "list" {
						args = []string{"--config", c.Path, "jobs", "list", "--state", state, "--limit", "25", "--offset", "2"}
					}
					if asJSON {
						// Persistent flags also work before the command.
						args = append([]string{"--json"}, args...)
					}
					var out bytes.Buffer
					err := Execute(context.Background(), args, strings.NewReader(""), &out, &bytes.Buffer{})
					wantFailure := action == "wait" && state != "completed"
					if (err != nil) != wantFailure {
						t.Fatalf("error: %v, output: %s", err, out.String())
					}
					if json.Valid(out.Bytes()) != asJSON || !strings.Contains(out.String(), "job-1") || !strings.Contains(out.String(), state) {
						t.Fatalf("unexpected output: %s", out.String())
					}
					if action == "list" && !asJSON && (!strings.Contains(out.String(), "FLOW") || !strings.Contains(out.String(), "offset 2")) {
						t.Fatal(out.String())
					}
					if action != "list" && !strings.Contains(out.String(), "9007199254740993") {
						t.Fatalf("result number lost precision: %s", out.String())
					}
				})
			}
		}
	}
}

func TestHubDetailAndVersionFormats(t *testing.T) {
	t.Chdir(t.TempDir())
	manifest := `{"key":"confidence","version":1,"data_type":"number"}`
	if err := os.WriteFile("confidence.json", []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MF_HUB_ADMIN_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			fmt.Fprint(w, `{"ready":true}`)
		case strings.HasSuffix(r.URL.Path, "/versions"):
			fmt.Fprintf(w, `{"items":[%s],"limit":100,"offset":0}`, manifest)
		default:
			fmt.Fprint(w, manifest)
		}
	}))
	defer server.Close()
	for _, command := range [][]string{{"health"}, {"show", "tagtypes", "confidence@1"}, {"versions", "tagtypes", "confidence"}, {"publish", "tagtypes", "confidence.json"}} {
		for _, asJSON := range []bool{false, true} {
			args := append([]string{"hub", "--url", server.URL}, command...)
			if asJSON {
				args = append(args, "--json")
			}
			var out bytes.Buffer
			if err := Execute(context.Background(), args, strings.NewReader(""), &out, &out); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if json.Valid(out.Bytes()) != asJSON {
				t.Fatalf("%v: %s", args, out.String())
			}
			if !asJSON && command[0] == "versions" && !strings.Contains(out.String(), "confidence@1") {
				t.Fatal(out.String())
			}
		}
	}
}

func TestEmptyCollections(t *testing.T) {
	c := outputConfig(t, "")
	for _, group := range []string{"tagtypes", "flows", "programs", "segments", "tags"} {
		for _, asJSON := range []bool{false, true} {
			args := []string{"--config", c.Path, group, "list"}
			if asJSON {
				args = append(args, "--json")
			}
			var out bytes.Buffer
			if err := Execute(context.Background(), args, strings.NewReader(""), &out, &out); err != nil {
				t.Fatal(err)
			}
			if (asJSON && out.String() != "[]\n") || (!asJSON && out.String() != "No items found.\n") {
				t.Fatalf("%v: %s", args, out.String())
			}
		}
	}
}

func TestStructuredDetailsAndTableValues(t *testing.T) {
	longText := strings.Repeat("complete-value-", 20)
	value := json.RawMessage(fmt.Sprintf(`{"id":7,"tag_type_key":"analysis","tag_type_version":1,"data_type":"json","value":{"description":%q,"active":false,"count":9007199254740993,"unknown":null,"people":[{"name":"\u001b[31mAlice\u001b[0m\nSmith"}]},"links":[]}`, longText))
	for _, mode := range []string{"list", "show"} {
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			root := &cobra.Command{Use: "mf"}
			root.SetOut(&out)
			root.PersistentFlags().Bool("json", false, "")
			tags := &cobra.Command{Use: "tags"}
			root.AddCommand(tags)
			tags.AddCommand(&cobra.Command{Use: mode, RunE: func(cmd *cobra.Command, _ []string) error {
				if mode == "list" {
					return printOutput(cmd, []json.RawMessage{value})
				}
				return printOutput(cmd, value)
			}})
			args := []string{"tags", mode}
			if asJSON {
				args = append(args, "--json")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if asJSON {
				want := value
				if mode == "list" {
					want = append(append([]byte{'['}, value...), ']')
				}
				if !store.SameJSON(want, out.Bytes()) {
					t.Fatalf("JSON changed: %s", out.String())
				}
			} else {
				if strings.ContainsAny(out.String(), "\x1b\t") {
					t.Fatalf("unescaped terminal controls: %q", out.String())
				}
				if mode == "show" {
					for _, want := range []string{longText, "9007199254740993", "People:", "Alice Smith", "false", "Unknown:", "—"} {
						if !strings.Contains(out.String(), want) {
							t.Errorf("missing %q in %s", want, out.String())
						}
					}
				} else if strings.Contains(out.String(), longText) || !strings.Contains(out.String(), "…") {
					t.Fatalf("table did not summarize long values: %s", out.String())
				}
			}
		}
	}
}
