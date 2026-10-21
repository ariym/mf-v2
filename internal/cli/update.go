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
	"runtime"
	"strings"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/kballard/go-shellquote"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

const releaseAPI = "https://api.github.com/repos/ariym/mf-v2/releases/latest"

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
			token, curlAuth := updateCredentials(cmd.Context())
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, releaseAPI, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", "application/vnd.github+json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("check for updates: GitHub HTTP %d; ensure a release is published and, for private repositories, set GH_TOKEN/GITHUB_TOKEN with Contents read access or run gh auth login", resp.StatusCode)
			}
			var release struct {
				Tag        string `json:"tag_name"`
				Draft      bool   `json:"draft"`
				Prerelease bool   `json:"prerelease"`
				Assets     []struct {
					Name string `json:"name"`
				} `json:"assets"`
			}
			if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
				return fmt.Errorf("read latest release: %w", err)
			}
			if !stableReleaseTag(release.Tag) || release.Draft || release.Prerelease {
				return fmt.Errorf("latest release must be a published stable vMAJOR.MINOR.PATCH version")
			}
			current := "v" + config.Version
			if !stableReleaseTag(current) {
				return fmt.Errorf("invalid installed version %q: expected major.minor.patch", config.Version)
			}
			if semver.Compare(current, release.Tag) >= 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Latest Version Installed.")
				return err
			}
			asset := "mf-" + runtime.GOOS + "-" + runtime.GOARCH
			available := make(map[string]bool, len(release.Assets))
			for _, entry := range release.Assets {
				available[entry.Name] = true
			}
			if !available[asset] || !available[asset+".sha256"] {
				return fmt.Errorf("release %s is missing the executable or checksum for %s/%s", release.Tag, runtime.GOOS, runtime.GOARCH)
			}
			path, err := executable()
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
	return "curl -fsSL" + curlAuth + " -H 'Accept: application/vnd.github.raw+json' " + shellquote.Join(url) +
		" | MF_VERSION=" + shellquote.Join(tag) + " MF_INSTALL_DIR=" + shellquote.Join(dir) + " sh"
}
