package hub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPrivateReleaseCredentialsStayOnGitHubAPI(t *testing.T) {
	for _, source := range []string{"GH_TOKEN", "GITHUB_TOKEN", "gh login"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")
			if source == "gh login" {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n[ \"$*\" = \"auth token --hostname github.com\" ] || exit 1\nprintf '%s\\n' test-secret\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir)
			} else {
				t.Setenv(source, "test-secret")
			}
			testPrivateReleaseDownload(t)
		})
	}
}

func testPrivateReleaseDownload(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	var requests []string
	http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		header := http.Header{}
		body := ""
		status := 200
		switch r.URL.Hostname() {
		case "api.github.com":
			if r.Header.Get("Authorization") != "Bearer test-secret" {
				t.Error("missing GitHub authorization")
			}
			if strings.Contains(r.URL.Path, "/releases/tags/") {
				body = `{"assets":[{"name":"tool.tar.gz","url":"https://api.github.com/repos/o/r/releases/assets/1"}]}`
			} else {
				status = 302
				header.Set("Location", "https://cdn.example.com/asset")
			}
		case "cdn.example.com":
			if r.Header.Get("Authorization") != "" {
				t.Error("leaked GitHub credentials")
			}
			body = "verified archive"
		default:
			t.Fatalf("unexpected host %s", r.URL.Hostname())
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	var out bytes.Buffer
	if e := Download(context.Background(), "https://github.com/o/r/releases/download/v1/tool.tar.gz", &out); e != nil {
		t.Fatal(e)
	}
	if out.String() != "verified archive" || len(requests) != 3 {
		t.Fatalf("%s %v", out.String(), requests)
	}
}

func TestGitHubTokenPrecedenceAndMissingLogin(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GH_TOKEN", "first")
	t.Setenv("GITHUB_TOKEN", "second")
	if got := githubToken(context.Background()); got != "first" {
		t.Fatal("GH_TOKEN did not take precedence")
	}
	t.Setenv("GH_TOKEN", "")
	if got := githubToken(context.Background()); got != "second" {
		t.Fatal("GITHUB_TOKEN fallback was ignored")
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := githubToken(context.Background()); got != "" {
		t.Fatal("expected anonymous access without gh login")
	}
}

func TestGitHub404ExplainsAuthentication(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credentials")
		}
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("Not Found")), Request: r}, nil
	})
	err := Download(context.Background(), "https://github.com/o/r/releases/download/v1/tool.tar.gz", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "private") || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
