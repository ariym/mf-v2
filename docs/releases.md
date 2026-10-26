# GitHub releases and updates

The [release workflow](../.github/workflows/release.yml) runs on pushes to `release` and stable `vMAJOR.MINOR.PATCH` tags. A local commit starts a build when it is pushed to GitHub. One push containing several commits releases the tip commit.

## GitHub setup

1. In the repository's **Settings → Actions → General**, enable GitHub Actions and permit `actions/checkout` and `actions/setup-go`. The workflow requests `contents: write` so the automatically provided `GITHUB_TOKEN` can create tags, releases, and release assets. An organization policy or tag ruleset must allow those operations. No personal access token is needed for the workflow.
2. Commit the workflow, scripts, and CLI changes along with the rest of the application you want to release. Create and push a `release` branch containing those commits:

   ```sh
   git switch -c release
   git push -u origin release
   ```

   If `release` already exists, switch to it, merge the desired application changes, then push it.
3. Watch the **Release** workflow under **Actions**. After successful tests and builds, it creates a tag and publishes a complete release at [GitHub Releases](https://github.com/ariym/mf-v2/releases). The Releases page already belongs to the repository; no GitHub Pages setup or manually created first release is needed.

For subsequent releases, commit to `release` and run `git push`. Each pushed tip gets the next patch version after the highest stable tag, starting at `v0.1.0`. Publication is serialized, with up to 100 pending runs queued, so ordinary bursts of pushes do not replace waiting builds. A rerun reuses an existing tag for that commit; a published release is left intact, and a partially uploaded draft can be completed.

To start a new major/minor version, push an explicit stable tag on the intended commit:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The next automatic release becomes `v0.2.1`. Tag only commits that contain this release workflow and the installer. Configuration compatibility still requires matching major/minor versions; review `mf.yaml` when moving between those version lines.

## Build machines and packages

GitHub Actions hosts the build machine. One Ubuntu runner cross-compiles Go with `CGO_ENABLED=0` for all existing targets:

| System | Architectures | Executable assets |
| --- | --- | --- |
| macOS | Apple Silicon, Intel | `mf-darwin-arm64`, `mf-darwin-amd64` |
| Debian / Linux | AMD64, ARM64 | `mf-linux-amd64`, `mf-linux-arm64` |

You do not need to provide a Mac and a Debian machine. The SQLite driver is pure Go. Separate macOS/Linux CI runners exercise native behavior in the existing CI workflow. The release job runs its own Linux tests and `go vet` before building. FFmpeg/ffprobe remain external dependencies, and the Linux downloads are executables rather than Debian `.deb` packages.

GitHub Actions is the automation used here. Other CI systems or a manually run build could also upload releases using GitHub's API; GitHub Releases itself does not require Actions.

## Local builds

Local builds and GitHub Actions use the same [build script](../scripts/build.sh) through Make:

```sh
make build                     # Native executable: dist/mf
./dist/mf version
make dist                      # All four platforms, checksums, and installer
make dist VERSION=0.1.1         # Embed an explicit release version
make clean                     # Remove dist/
```

Both build targets accept `VERSION=0.1.1` or `VERSION=v0.1.1`. Without it, the executable uses the development version declared in `internal/config/config.go`. These commands build files locally; they do not create tags or publish releases.

```text
dist/
  mf                           # make build: native development executable
  mf-darwin-amd64               # make dist: Intel macOS
  mf-darwin-amd64.sha256
  mf-darwin-arm64               # Apple Silicon macOS
  mf-darwin-arm64.sha256
  mf-linux-amd64                # AMD64 Debian/Linux
  mf-linux-amd64.sha256
  mf-linux-arm64                # ARM64 Debian/Linux
  mf-linux-arm64.sha256
  install.sh
```

`make build` refreshes only `dist/mf`; run `make dist` to refresh all platform builds. Cross-compilation requires only Go and either `sha256sum` or `shasum`, and disables CGO for portable Linux executables. The native build retains Go's default debug information, while platform release executables are stripped. The entire output directory is Git-ignored. Release uploads include only the platform executables, their checksums, and the installer.

## Install and update

Requires `curl`, Python 3, and `sha256sum` or `shasum`. The repository is public, so no GitHub login or token is required:

```sh
curl -fsSL https://raw.githubusercontent.com/ariym/mf-v2/release/scripts/install.sh | sh
```

The default destination is `~/.local/bin/mf`. For a local `./mf` installation, replace the final `sh` with `MF_INSTALL_DIR="$PWD" sh`.

After installation succeeds, if the destination is missing from `PATH`, the installer asks `Would you like to automatically add 'mf' to your PATH? (Y/n)`. Enter accepts; `n` declines. It reads your answer from the terminal even when the script is piped from curl. For zsh it updates `.zshrc` (respecting `ZDOTDIR`); for bash it updates `.bashrc` and the login profile; for POSIX shells it updates `.profile`. Existing settings are preserved, and rerunning the installer does not duplicate its PATH block. Unsupported shells or noninteractive installs receive manual instructions.

Open a new terminal to use the saved PATH, or run the printed `export PATH=...` command to update the current terminal immediately. An installer subprocess cannot change its parent terminal's environment.

The installer finishes by running `--help` directly on the newly installed executable, so help appears immediately whether or not the destination is on `PATH`.

```sh
mf version
mf update
```

`mf update` compares the executable version with the latest published stable release. Equal or newer installations print exactly:

```text
Latest Version Installed.
```

A newer release prints:

```text
New Update Available (0.1.1)!
curl ... | MF_VERSION=v0.1.1 MF_INSTALL_DIR=/path/to/installation sh
```

The actual command contains a full installer URL pinned to the reported tag and the resolved executable directory. Paths containing spaces or shell punctuation are quoted. It refers to the same credential source used for the check (`GH_TOKEN`, `GITHUB_TOKEN`, or `gh`) without printing the token itself. Copy and run the command to install. No config or database is needed for the check.

The installer resolves executable and checksum assets from one release, verifies SHA-256, and replaces `mf` atomically in the destination directory. Failed downloads or checksum verification preserve the existing executable. Optional `MF_VERSION=v0.1.1` selects an exact stable release; omitting it selects the latest. Restart a running server/daemon after updating so its process uses the new binary.

## Local testing releases

The `testing` branch can publish directly from your computer. GitHub Actions is not part of this publishing path. A local `post-commit` hook pushes the committed snapshot to `origin/testing`, builds only the host OS/architecture, and publishes a GitHub prerelease containing that executable, its SHA-256 checksum, and the installer. On an Apple Silicon Mac this produces `mf-darwin-arm64`; Linux/Intel clients need a publication from their own platform before testing updates are available.

Set up the publishing checkout once, after these scripts and CLI changes have been committed:

```sh
gh auth login                       # Account needs repository Contents write access
git switch -c testing                # Use git switch testing if it already exists
make testing-hooks                  # Installs this checkout's post-commit hook
make testing-release                # Publishes the current commit for the first time
```

Go, Git, Python 3, `tar`, and GitHub CLI must be on this terminal's `PATH`. The remote must be `origin` for `ariym/mf-v2`, and repository rules must permit pushing `testing` and creating release tags. An existing post-commit hook is preserved; the setup command explains how to add the publisher to it. Hooks are local to the checkout and are not installed by cloning the repository.

Subsequent `git commit` calls on `testing` build and publish synchronously, so the terminal shows failures and the time to publication. The commit remains saved if publishing fails; retry `make testing-release`. Set `MF_SKIP_TESTING_RELEASE=1 git commit ...` to skip one publication. To disable automatic publication, remove only the installed mf hook (or its call from your custom hook). Merges performed by `git merge` may need `make testing-release` afterward because Git has a separate post-merge hook.

The build uses `git archive` of the commit, so unstaged and untracked files cannot enter the release. Stable ancestor tags supply the compatibility version (at least the source's declared version); `mf version` also shows the testing tag. Build tags look like `testing-darwin-arm64-20261026120000000000-g<FULL_COMMIT_SHA>`. They are unique per commit/platform, do not start with `v`, and are published with `prerelease=true` and `latest=false`. The stable updater continues using GitHub's stable `releases/latest` endpoint. Testing branch/tag pushes skip the general Go workflow; pull requests still run CI.

Before compilation, the script checks the repository, fetches tags, pushes the testing commit, and checks for an existing release. After compilation, it only hashes the executable, uploads the three files into a draft, publishes it, and confirms it through GitHub's release API. The target is **under 30 seconds after compilation**; the script prints the measured time and reports when it exceeds the target. Upload bandwidth, API latency and GitHub availability prevent a hard timing guarantee. Warm Go caches speed up compilation; no test suite or four-platform build runs in this path. Run `make test` and `make vet` separately when needed.

On each testing client, add this top-level field to its existing `mf.yaml`:

```yaml
testing_branch: true
```

Then run:

```sh
mf update
# Or, from another directory:
mf --config /path/to/mf.yaml update
```

The updater reads the preference without validating runtime version compatibility or opening a database. It finds the first published testing prerelease for this platform with both executable and checksum assets, skipping drafts and incomplete releases. It requests API cache revalidation and paginates when necessary. If none exists it reports that explicitly. The printed install command opts in with `MF_TESTING=1` and pins the exact tag, installer source, and existing installation directory. Copy and run it to install, then restart running mf processes. Running `mf update` itself remains a check that prints instructions.

Clients need a binary containing this channel support before adding the new YAML field: older versions reject unknown config fields and only check stable releases. Either distribute one stable release containing these changes first, or bootstrap a native testing release directly using the exact tag printed by the publisher:

```sh
curl -fsSL https://raw.githubusercontent.com/ariym/mf-v2/testing/scripts/install.sh \
  | MF_TESTING=1 MF_VERSION='testing-darwin-arm64-<TIMESTAMP>-g<FULL_COMMIT_SHA>' sh
```

Replace the example tag with the complete published tag (including its 20-digit timestamp); use the matching platform. Set `MF_INSTALL_DIR` too if the client is installed somewhere other than `~/.local/bin`.

Removing the flag or setting it to `false` makes `mf update` offer stable again, including when testing and stable share the same compatibility version. Without a local config, the updater also selects stable. The installer accepts testing tags only with an exact native tag and `MF_TESTING=1`; the default installer keeps selecting stable.

This is an update-channel preference, not access control. Published prerelease assets in this public repository can be downloaded by anyone through GitHub. Enforcing download access would require authenticated private distribution. GitHub documents [prerelease/latest controls](https://cli.github.com/manual/gh_release_create) and [release API visibility](https://docs.github.com/en/rest/releases/releases).
