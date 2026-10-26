package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/kballard/go-shellquote"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

const releaseAPI = "https://api.github.com/repos/ariym/mf-v2/releases/latest"

const releasesAPI = "https://api.github.com/repos/ariym/mf-v2/releases"

var testingTag = regexp.MustCompile(`^testing-(darwin|linux)-(amd64|arm64)-[0-9]{20}-g[0-9a-f]{40}$`)

type updateRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func (r updateRelease) hasNativeAssets() bool {
	asset := "mf-" + runtime.GOOS + "-" + runtime.GOARCH
	available := make(map[string]bool, len(r.Assets))
	for _, entry := range r.Assets {
		available[entry.Name] = true
	}
	return available[asset] && available[asset+".sha256"]
}

func fetchUpdateRelease(ctx context.Context, client *http.Client, token, endpoint string, result any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// Ask intermediaries to revalidate, so a just-published build is discoverable.
	req.Header.Set("Cache-Control", "no-cache")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("check for updates: GitHub HTTP %d; ensure a release is published and, for private repositories, set GH_TOKEN/GITHUB_TOKEN with Contents read access or run gh auth login", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result); err != nil {
		return false, fmt.Errorf("read latest release: %w", err)
	}
	return strings.Contains(resp.Header.Get("Link"), `rel="next"`), nil
}

func latestTestingRelease(ctx context.Context, client *http.Client, token string) (updateRelease, error) {
	prefix := "testing-" + runtime.GOOS + "-" + runtime.GOARCH + "-"
	for page := 1; ; page++ {
		var releases []updateRelease
		next, err := fetchUpdateRelease(ctx, client, token, fmt.Sprintf("%s?per_page=100&page=%d", releasesAPI, page), &releases)
		if err != nil {
			return updateRelease{}, err
		}
		// GitHub lists newer releases first. Skip drafts, other platforms and
		// incomplete uploads; every selected tag names an immutable snapshot.
		for _, release := range releases {
			if strings.HasPrefix(release.Tag, prefix) && testingTag.MatchString(release.Tag) &&
				release.Prerelease && !release.Draft && release.hasNativeAssets() {
				return release, nil
			}
		}
		if !next || len(releases) == 0 {
			return updateRelease{}, fmt.Errorf("no published testing release with executable and checksum for %s/%s", runtime.GOOS, runtime.GOARCH)
		}
	}
}

func newUpdateCommand() *cobra.Command {
	return updateCommand(&http.Client{Timeout: 15 * time.Second}, os.Executable)
}

func updateCommand(client *http.Client, executable func() (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:               "update",
		Short:             "Check for a release and print a command to update this installation",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := ""
			if cmd.Flags().Lookup("config") != nil {
				path, _ = cmd.Flags().GetString("config")
			}
			testing, err := config.UpdateTestingBranch(path)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 25*time.Second)
			defer cancel()
			token, curlAuth := updateCredentials(ctx)
			var release updateRelease
			upToDate := false
			if testing {
				release, err = latestTestingRelease(ctx, client, token)
				upToDate = config.TestingRelease == release.Tag
			} else {
				_, err = fetchUpdateRelease(ctx, client, token, releaseAPI, &release)
				if err == nil && (!stableReleaseTag(release.Tag) || release.Draft || release.Prerelease) {
					return fmt.Errorf("latest release must be a published stable vMAJOR.MINOR.PATCH version")
				}
				current := "v" + config.Version
				if !stableReleaseTag(current) {
					return fmt.Errorf("invalid installed version %q: expected major.minor.patch", config.Version)
				}
				// Removing the opt-in also offers a return from testing to stable.
				upToDate = config.TestingRelease == "" && semver.Compare(current, release.Tag) >= 0
			}
			if err != nil {
				return err
			}
			if upToDate {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Latest Version Installed.")
				return err
			}
			if !release.hasNativeAssets() {
				return fmt.Errorf("release %s is missing the executable or checksum for %s/%s", release.Tag, runtime.GOOS, runtime.GOARCH)
			}
			path, err = executable()
			if err != nil {
				return fmt.Errorf("locate installed executable: %w", err)
			}
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve installed executable: %w", err)
			}
			command := updateInstallCommand(release.Tag, filepath.Dir(path), curlAuth)
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "New Update Available (%s)!\n%s\n", strings.TrimPrefix(release.Tag, "v"), command)
			return err
		},
	}
}

func stableReleaseTag(tag string) bool {
	return semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == "" && semver.Build(tag) == ""
}

// Keep credentials out of printed commands; refer to the user's credential source.
func updateCredentials(ctx context.Context) (token, curlAuth string) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			return token, ` -H "Authorization: Bearer $` + name + `"`
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err == nil {
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token, ` -H "Authorization: Bearer $(gh auth token --hostname github.com)"`
		}
	}
	return "", ""
}

func updateInstallCommand(tag, dir, curlAuth string) string {
	url := "https://api.github.com/repos/ariym/mf-v2/contents/scripts/install.sh?ref=" + tag
	optIn := ""
	if testingTag.MatchString(tag) {
		optIn = " MF_TESTING=1"
	}
	return "curl -fsSL" + curlAuth + " -H 'Accept: application/vnd.github.raw+json' " + shellquote.Join(url) +
		" |" + optIn + " MF_VERSION=" + shellquote.Join(tag) + " MF_INSTALL_DIR=" + shellquote.Join(dir) + " sh"
}
