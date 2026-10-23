# mf

A local media catalog with an `mf` CLI, HTTP API, and SQLite-backed durable Go workflows. The same executable runs a foreground server or a detached daemon on macOS and Linux.

The client integrates with mf-hub v1 for tag-type definitions, executable flows, and external programs. It includes shared segment tags, a two-pane directory/timeline browser, path-based job management, and SQL-driven FFmpeg rendering. See the [complete CLI reference](docs/cli.md), [client schema](docs/schema.md), and [hub integration](docs/hub.md).

## Install the latest release

The installer detects macOS or Linux (including Debian) and Intel/AMD (`amd64`) or ARM (`arm64`), downloads the matching executable from the latest GitHub release, verifies its SHA-256 checksum, and installs it to `~/.local/bin/mf`. These are standalone executables, not `.deb` packages.

Run this one-liner (requires `curl` and Python 3). The repository is public; no GitHub login or token is required:

```sh
curl -fsSL https://raw.githubusercontent.com/ariym/mf-v2/release/scripts/install.sh | sh
```

After a successful installation, if the destination is missing from `PATH`, the installer asks `Would you like to automatically add 'mf' to your PATH? (Y/n)`. Press Enter to accept, or `n` to skip. It updates your shell startup files for zsh, bash, or a POSIX shell, then prints a command you can run to update the current terminal. New terminals pick up the saved change automatically. Without an interactive terminal, it prints manual instructions instead.

To choose another destination, replace the final `sh` with `MF_INSTALL_DIR=/your/bin sh`. FFprobe must also be installed and on `PATH` for media imports.

The command becomes available after this installer is pushed to `release` and the first release is published. Each push to `release` runs the [release workflow](.github/workflows/release.yml), which tests the project, assigns the next patch version (starting at `v0.1.0`), builds all four executables, and publishes them with their checksums and `install.sh`. The binary's reported version is set from the release tag. Release assets use these names:

- `mf-darwin-amd64` and `mf-darwin-arm64` for macOS.
- `mf-linux-amd64` and `mf-linux-arm64` for Linux.

Each executable has a corresponding `.sha256` file. Other branch pushes and pull requests run CI. Explicit stable tags such as `v0.2.0` can also publish releases to start a new minor or major version. See [release setup](docs/releases.md) for GitHub settings, branch creation, and installation examples.

Run `mf update` from any directory to check for an update without requiring local initialization:

```text
Latest Version Installed.
```

When a newer version exists, it prints `New Update Available (0.1.1)!` followed by a copyable curl command. That command installs the exact reported version into the directory containing the running executable, resolving symlinks; it does not depend on your current working directory. The check itself only prints instructions. Public releases require no authentication. Restart any running mf server/daemon after installing to use the new executable.

## Build from source

Install Go 1.25 or newer and FFmpeg (including `ffprobe`) on `PATH`. FFprobe is required for imports; FFmpeg itself is used to generate test fixtures and regenerate the format catalog. Go 1.21+ can download the required toolchain automatically when `GOTOOLCHAIN=auto` is enabled.

```sh
make build
./dist/mf version
```

The native executable is written to `dist/mf`. Put it on `PATH`, or use `./dist/mf` in place of `mf` in the examples.

`make dist` builds macOS and Linux executables for AMD64 and ARM64 into `dist/`, alongside SHA-256 checksums and the installer. Set `VERSION` to embed an explicit version, for example `make dist VERSION=0.1.1`. GitHub Releases uses this same build command. `make clean` removes the generated `dist/` directory. See [local builds](docs/releases.md#local-builds) for the output layout.

## Quick start

```sh
mf init -y
mf add /path/to/media
mf server start
```

`mf init` asks for the five core configuration values. Enter accepts a default; `-y` uses all defaults. New configurations also include hub, module, and rendering defaults. Initialization creates schema v2 and the workflow tables. It never overwrites an existing config. Existing v1 databases require `mf migrate` with the server stopped.

```sh
mf flows install transcribe@1
mf jobs run transcribe@1 /path/to/media.mp4 --wait
mf browse /path/to/media
mf hub browse
mf sql-to-ffmpeg 'SELECT * FROM segments ORDER BY id'
```

Private hub release assets require `GH_TOKEN`, `GITHUB_TOKEN`, or an existing GitHub CLI login (`gh auth login`); ML programs may need explicit model preparation. Audio-only render selections produce MP3 without cover art; video and mixed selections produce MP4.

For background operation:

```sh
mf daemon start
mf daemon logs
mf daemon logs --follow
mf daemon stop
mf daemon stop -f
```

A normal stop refuses while started jobs remain unfinished and reports their count. A forced stop interrupts work and preserves it for restart. Queued jobs survive either kind of stop. Ctrl-C or SIGTERM stops a foreground server using the same resumable behavior.

## Configuration

By default, commands read only `mf.yaml` in the **current working directory**, independently of where the executable is installed. Specify another file with a global flag:

```sh
mf --config=/path/to/mf.yaml server start
mf --config=/path/to/mf.yaml add /path/to/file
mf --config=/path/to/new/mf.yaml init -y
```

```yaml
app_name: mf-processor
mf_version: 0.1.0
server_ip: localhost
server_port: 11747
sqlite_path: mf.db
hub_url: http://127.0.0.1:11748
modules_path: mf_modules
```

The optional `sql_to_ffmpeg` block configures codecs, quality, dimensions, frame rate, and audio normalization for SQL-driven exports. See [mf.example.yaml](mf.example.yaml) for every default. Existing configurations that omit these fields retain the documented defaults.

Relative `sqlite_path` values are relative to the config directory. Relative CLI import paths are relative to the invoking working directory; API import paths are relative to the server's config directory. Existing database symlinks resolve to the same catalog and daemon state.

`mf_version` defaults to the executable version when omitted. Config versions must have the same major/minor and a patch no newer than the executable. `mf version`, `mf update`, help, and initialization work without a config. Operational commands validate config and database structure; only startup checks whether the configured address can bind. Startup retains that listener.

## Imports and storage

`mf add` imports synchronously and works while the server is stopped or running. Directories are walked recursively in lexical order without following directory symlinks. File symlinks are canonicalized, and importing the same canonical file updates its existing record. Recursive scans skip the configured module directory and database process state, so bundled runtimes are not cataloged. Explicit file inputs remain available.

All regular files are probed, including unknown extensions. The generated catalog in `internal/media/formats_generated.go` records the source FFprobe build, demuxers, and advertised extensions. Installed FFprobe capabilities remain authoritative; a static catalog cannot guarantee support in every build. Regenerate it with `python3 scripts/generate_formats.py` and format the result with `gofmt`.

The catalog stores `media_files`, `file_audio_metadata`, and `file_video_metadata`. Each audio/video stream gets its own metadata row, identified by `stream_index`. Video files may have both audio and video metadata. Attached cover art does not turn an audio file into a video file. Duration is in seconds, file size in bytes, and timestamps are UTC; unavailable numeric metadata is null.

Each file's record and stream metadata commit together. Reimporting preserves the media ID and creation timestamp. Directory scans continue after errors and print added, updated, skipped, and failed counts (up to 100 detailed errors). Ordinary non-media files are skipped; failed expected media imports produce a nonzero exit status. Explicit non-media inputs fail. Probes time out after 30 seconds, and network media protocols are disabled.

SQLite uses WAL, foreign keys, a 5-second busy timeout, and short transactions. App and engine migrations have separate bookkeeping. `mf migrate` validates and upgrades the known v1 schema transactionally; foreign, changed, and future schemas are rejected. See [schema v2](docs/schema.md) for timecodes, shared tag instances, definitions, and result ingestion.

## API and custom jobs

See [HTTP API](docs/api.md) for request/response examples and [custom Go workflows](docs/flows.md) for adding compiled flows and activities.

The server embeds one worker with one activity executing at a time. API imports are durable jobs; direct CLI imports use the same import service synchronously. Job states are `queued`, `running`, `completed`, `failed`, and `cancelled`. Started jobs waiting on retries/timers still count as running for shutdown. Downloaded flows use a generic activity adapter with pinned dependency paths and transactional, idempotent tag ingestion.

This initial API has no authentication and is intended for trusted local/private networks. The default binds to localhost. Lifecycle control uses an owner-only Unix socket, never the HTTP API. There is no file-upload endpoint: import paths refer to the server's filesystem.

## Process state and logs

The canonical SQLite filename plus `.mf/` contains a process lock, `pid.json`, `control.sock`, and `daemon.log`. Only one server can own a database at a time. Stale PID files are never used to signal processes. Startup waits for readiness before returning; failures point to `mf daemon logs`.

Daemon logs are append-only; `mf daemon logs` shows the final 20 lines, and `--follow` streams new output. Foreground logs go to the terminal. Downloaded-flow diagnostics are available through `mf jobs logs`. Unix socket paths must fit the host OS pathname limit (use a shorter database path if startup reports a control-socket error). Service-manager installation, Windows daemon management, log rotation, and client API authentication are not included.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
make build
```

Tests include actual FFmpeg media fixtures, durable retry/restart recovery, HTTP handlers, and temporary daemon subprocesses. Integration tests require permission to bind loopback ports and Unix sockets. Real media fixture tests skip if FFmpeg is absent; CI installs it on macOS and Linux.
