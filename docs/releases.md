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

Requires `curl`, Python 3, and `sha256sum` or `shasum`. The repository is private, so export a GitHub token with **Contents: read** access:

```sh
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H 'Accept: application/vnd.github.raw+json' 'https://api.github.com/repos/ariym/mf-v2/contents/scripts/install.sh?ref=release' | sh
```

If you already use `gh auth login`, fetch the installer with that login instead:

```sh
curl -fsSL -H "Authorization: Bearer $(gh auth token --hostname github.com)" -H 'Accept: application/vnd.github.raw+json' 'https://api.github.com/repos/ariym/mf-v2/contents/scripts/install.sh?ref=release' | sh
```

If the repository becomes public, no token is required:

```sh
curl -fsSL https://raw.githubusercontent.com/ariym/mf-v2/release/scripts/install.sh | sh
```

The default destination is `~/.local/bin/mf`. For a local `./mf` installation, replace the final `sh` with `MF_INSTALL_DIR="$PWD" sh`. Add the chosen directory to `PATH`, or invoke the local executable as `./mf`.

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
