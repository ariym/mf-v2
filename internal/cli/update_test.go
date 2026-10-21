package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ariym/mf-v2/internal/config"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func updateFixture(t *testing.T, body string, status int, executable string) (string, error) {
	t.Helper()
	client := &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != releaseAPI || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("unexpected release request: %s %v", r.URL, r.Header)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-secret" {
			t.Errorf("authorization = %q", got)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	root := New(strings.NewReader(""), io.Discard, io.Discard)
	for _, cmd := range root.Commands() {
		if cmd.Name() == "update" {
			root.RemoveCommand(cmd)
		}
	}
	root.AddCommand(updateCommand(client, func() (string, error) { return executable, nil }))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"update"})
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func releaseBody(tag string, assets bool) string {
	asset := "mf-" + runtime.GOOS + "-" + runtime.GOARCH
	release := map[string]any{"tag_name": tag}
	if assets {
		release["assets"] = []map[string]string{{"name": asset}, {"name": asset + ".sha256"}}
	}
	raw, _ := json.Marshal(release)
	return string(raw)
}

func TestUpdateVersionComparison(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-secret")
	t.Chdir(t.TempDir()) // Update must work without mf.yaml or a database.
	before := config.Version
	t.Cleanup(func() { config.Version = before })
	config.Version = "0.1.10"
	for _, tag := range []string{"v0.1.10", "v0.1.9", "v0.0.99"} {
		t.Run(tag, func(t *testing.T) {
			out, err := updateFixture(t, releaseBody(tag, true), http.StatusOK, "unused")
			if err != nil || out != "Latest Version Installed.\n" {
				t.Fatalf("output = %q, error = %v", out, err)
			}
		})
	}
}

func TestUpdateCommandPinsVersionAndResolvesInstallation(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-secret")
	dir := t.TempDir()
	t.Chdir(dir)
	installDir := filepath.Join(dir, "install with spaces and 'quotes' $(false)")
	if err := os.Mkdir(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(installDir, "mf")
	if err := os.WriteFile(binary, []byte("previous binary"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mf")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(installDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v0.1.11", "v0.2.0", "v1.0.0"} {
		t.Run(tag, func(t *testing.T) {
			out, err := updateFixture(t, releaseBody(tag, true), http.StatusOK, link)
			if err != nil {
				t.Fatal(err)
			}
			prefix := "New Update Available (" + tag[1:] + ")!\n"
			if !strings.HasPrefix(out, prefix) || strings.Contains(out, "fixture-secret") {
				t.Fatalf("unexpected output: %q", out)
			}
			command := strings.TrimSpace(strings.TrimPrefix(out, prefix))
			if !strings.Contains(command, "?ref="+tag) {
				t.Fatalf("installer script is not pinned: %s", command)
			}
			// Run the printed command against a fake curl to verify shell quoting
			// and that the installer receives the exact version and resolved directory.
			tools := t.TempDir()
			capture := filepath.Join(tools, "capture")
			curl := "#!/bin/sh\nprintf '%s\\n' 'printf \"%s\\n%s\\n\" \"$MF_VERSION\" \"$MF_INSTALL_DIR\" > \"$MF_TEST_CAPTURE\"'\n"
			if err := os.WriteFile(filepath.Join(tools, "curl"), []byte(curl), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "MF_TEST_CAPTURE="+capture)
			if raw, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("printed command failed: %v %s", err, raw)
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != tag+"\n"+resolved+"\n" {
				t.Fatalf("installer environment = %q, error = %v", got, err)
			}
			unchanged, err := os.ReadFile(binary)
			if err != nil || string(unchanged) != "previous binary" {
				t.Fatal("update check changed the installed binary")
			}
		})
	}
}

func TestUpdateRejectsUnavailableAndInvalidReleases(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-secret")
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"private or missing", `{}`, "GH_TOKEN/GITHUB_TOKEN", 404},
		{"rate limited", `{}`, "HTTP 403", 403},
		{"server failure", `{}`, "HTTP 500", 500},
		{"invalid json", `not json`, "read latest release", 200},
		{"missing version", `{}`, "stable", 200},
		{"noncanonical version", `{"tag_name":"v1.2"}`, "stable", 200},
		{"prerelease version", `{"tag_name":"v1.2.0-rc.1"}`, "stable", 200},
		{"prerelease flag", `{"tag_name":"v1.2.0","prerelease":true}`, "stable", 200},
		{"draft", `{"tag_name":"v1.2.0","draft":true}`, "stable", 200},
		{"missing assets", releaseBody("v1.2.0", false), "missing the executable or checksum", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := updateFixture(t, tc.body, tc.status, "unused")
			if err == nil || !strings.Contains(err.Error(), tc.want) || out != "" {
				t.Fatalf("output = %q, error = %v, want %q", out, err, tc.want)
			}
		})
	}
}

func TestUpdateRespectsCancellation(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-secret")
	client := &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := updateCommand(client, os.Executable)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestUpdateCredentialSources(t *testing.T) {
	for _, source := range []string{"GH_TOKEN", "GITHUB_TOKEN", "gh", "public"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")
			tools := t.TempDir()
			t.Setenv("PATH", tools)
			script := "#!/bin/sh\nexit 1\n"
			if source == "gh" {
				script = "#!/bin/sh\nprintf fixture-secret\n"
			} else if source != "public" {
				t.Setenv(source, "fixture-secret")
			}
			if err := os.WriteFile(filepath.Join(tools, "gh"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			token, auth := updateCredentials(context.Background())
			if source == "public" {
				if token != "" || auth != "" {
					t.Fatalf("public credentials = %q, %q", token, auth)
				}
			} else if token != "fixture-secret" || strings.Contains(auth, token) || !strings.Contains(auth, map[string]string{
				"GH_TOKEN": "$GH_TOKEN", "GITHUB_TOKEN": "$GITHUB_TOKEN", "gh": "$(gh auth token --hostname github.com)",
			}[source]) {
				t.Fatalf("unexpected credentials for %s", source)
			}
		})
	}
}
