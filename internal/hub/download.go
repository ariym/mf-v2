package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Download streams an HTTPS artifact. GitHub credentials are used only against
// api.github.com; signed CDN redirects never receive Authorization.
func Download(ctx context.Context, rawURL string, dst io.Writer) error {
	u, e := url.Parse(rawURL)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("artifact URL must use HTTPS without credentials")
	}
	token := ""
	if u.Hostname() == "github.com" || u.Hostname() == "api.github.com" {
		token = githubToken(ctx)
	}
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		r.Header.Del("Authorization")
		if r.URL.Scheme != "https" {
			return fmt.Errorf("refuse non-HTTPS artifact redirect")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}}
	request := func(raw string, accept string, auth bool) (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Accept", accept)
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}
	// Private browser download URLs must be mapped to a release asset API URL.
	if token != "" && u.Hostname() == "github.com" {
		p := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
		if len(p) == 6 && p[2] == "releases" && p[3] == "download" {
			for n := range p {
				decoded, err := url.PathUnescape(p[n])
				if err != nil {
					return err
				}
				p[n] = decoded
			}
			metadata := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", url.PathEscape(p[0]), url.PathEscape(p[1]), url.PathEscape(p[4]))
			resp, e := request(metadata, "application/vnd.github+json", true)
			if e != nil {
				return e
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				return fmt.Errorf("GitHub release metadata HTTP %d: release unavailable or credentials lack repository access (check GH_TOKEN, GITHUB_TOKEN, or gh auth login)", resp.StatusCode)
			}
			var release struct {
				Assets []struct {
					Name string `json:"name"`
					URL  string `json:"url"`
				}
			}
			e = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&release)
			resp.Body.Close()
			if e != nil {
				return e
			}
			found := false
			for _, asset := range release.Assets {
				if asset.Name == p[5] {
					assetURL, e := url.Parse(asset.URL)
					if e != nil || assetURL.Scheme != "https" || assetURL.Host != "api.github.com" || assetURL.User != nil || assetURL.Fragment != "" {
						return fmt.Errorf("invalid GitHub asset URL")
					}
					rawURL = asset.URL
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("GitHub release asset %s not found", p[5])
			}
		}
	}
	u, e = url.Parse(rawURL)
	if e != nil {
		return e
	}
	resp, e := request(rawURL, "application/octet-stream", token != "" && u.Hostname() == "api.github.com")
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == http.StatusNotFound && (u.Hostname() == "github.com" || u.Hostname() == "api.github.com") {
			return fmt.Errorf("GitHub artifact download HTTP 404: release asset missing or private; set GH_TOKEN/GITHUB_TOKEN with repository access, or run gh auth login")
		}
		return fmt.Errorf("artifact download HTTP %d", resp.StatusCode)
	}
	const limit = 8 << 30
	n, e := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if e == nil && n > limit {
		e = fmt.Errorf("archive exceeds 8 GiB download limit")
	}
	return e
}

// Reuse the user's GitHub CLI login when explicit environment credentials are
// absent. Credentials remain in memory and are sent only to api.github.com.
func githubToken(ctx context.Context) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			return token
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
