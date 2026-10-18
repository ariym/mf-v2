# mf

A local media catalog with an `mf` CLI, HTTP API, and SQLite-backed durable Go workflows. The same executable runs a foreground server or a detached daemon on macOS and Linux.

## Install the latest release

The installer detects macOS or Linux and Intel/AMD (`amd64`) or ARM (`arm64`), downloads the matching executable from the latest GitHub release, verifies its SHA-256 checksum, and installs it to `~/.local/bin/mf`.

This repository is currently private. Export `GH_TOKEN` with a GitHub token that has **Contents: read** access to this repository, then run this one-liner (requires `curl` and Python 3):

```sh
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H 'Accept: application/vnd.github.raw+json' 'https://api.github.com/repos/ariym/mf-v2/contents/scripts/install.sh?ref=master' | sh
```

Ensure `~/.local/bin` is on your `PATH`. To choose another destination, replace the final `sh` with `MF_INSTALL_DIR=/your/bin sh`. FFprobe must also be installed and on `PATH` for media imports.

The command becomes available after this installer is pushed to `master` and the first release is published. Pushing a stable version tag such as `v0.1.0` runs the [release workflow](.github/workflows/release.yml), which tests the project, builds all four executables, and publishes their checksums. The binary's reported version is set from the tag. Release assets use these names:

- `mf-darwin-amd64` and `mf-darwin-arm64` for macOS.
- `mf-linux-amd64` and `mf-linux-arm64` for Linux.

Each executable has a corresponding `.sha256` file. Ordinary branch pushes and pull requests continue to run CI; only version tags trigger release publication.

## Build from source

Install Go 1.25 or newer and FFmpeg (including `ffprobe`) on `PATH`. FFprobe is required for imports; FFmpeg itself is used to generate test fixtures and regenerate the format catalog. Go 1.21+ can download the required toolchain automatically when `GOTOOLCHAIN=auto` is enabled.

```sh
go build -o mf ./cmd/mf
```

Put the executable on `PATH`. The examples use `mf`; use `./mf` when running a local executable outside `PATH`.

## Quick start

```sh
mf init -y
mf add /path/to/media
mf server start
```

`mf init` asks for the five configuration values. Enter accepts a default; `-y` uses all defaults. Initialization creates the catalog and workflow tables. It never overwrites an existing config or migrates an incompatible existing database.

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
```

Relative `sqlite_path` values are relative to the config directory. Relative CLI import paths are relative to the invoking working directory; API import paths are relative to the server's config directory. Existing database symlinks resolve to the same catalog and daemon state.

`mf_version` defaults to the executable version when omitted. Config versions must have the same major/minor and a patch no newer than the executable. `mf version`, help, and initialization work without a config. Operational commands validate config and database structure; only startup checks whether the configured address can bind. Startup retains that listener.

## Imports and storage

`mf add` imports synchronously and works while the server is stopped or running. Directories are walked recursively in lexical order without following directory symlinks. File symlinks are canonicalized, and importing the same canonical file updates its existing record.

All regular files are probed, including unknown extensions. The generated catalog in `internal/media/formats_generated.go` records the source FFprobe build, demuxers, and advertised extensions. Installed FFprobe capabilities remain authoritative; a static catalog cannot guarantee support in every build. Regenerate it with `python3 scripts/generate_formats.py` and format the result with `gofmt`.

The catalog stores `media_files`, `file_audio_metadata`, and `file_video_metadata`. Each audio/video stream gets its own metadata row, identified by `stream_index`. Video files may have both audio and video metadata. Attached cover art does not turn an audio file into a video file. Duration is in seconds, file size in bytes, and timestamps are UTC; unavailable numeric metadata is null.

Each file's record and stream metadata commit together. Reimporting preserves the media ID and creation timestamp. Directory scans continue after errors and print added, updated, skipped, and failed counts (up to 100 detailed errors). Ordinary non-media files are skipped; failed expected media imports produce a nonzero exit status. Explicit non-media inputs fail. Probes time out after 30 seconds, and network media protocols are disabled.

SQLite uses WAL, foreign keys, a 5-second busy timeout, and short transactions. App and engine migrations have separate bookkeeping. Existing databases must match the bootstrap schema; there is no upgrade command yet.

## API and custom jobs

See [HTTP API](docs/api.md) for request/response examples and [custom Go workflows](docs/flows.md) for adding compiled flows and activities.

The server embeds one worker with one activity executing at a time. API imports are durable jobs; direct CLI imports use the same import service synchronously. Job states are `queued`, `running`, `completed`, and `failed`. Started jobs waiting on retries/timers still count as running for shutdown.

This initial API has no authentication and is intended for trusted local/private networks. The default binds to localhost. Lifecycle control uses an owner-only Unix socket, never the HTTP API. There is no file-upload endpoint: import paths refer to the server's filesystem.

## Process state and logs

The canonical SQLite filename plus `.mf/` contains a process lock, `pid.json`, `control.sock`, and `daemon.log`. Only one server can own a database at a time. Stale PID files are never used to signal processes. Startup waits for readiness before returning; failures point to `mf daemon logs`.

Daemon logs are append-only; `mf daemon logs` shows the final 20 lines, and `--follow` streams new output. Foreground logs go to the terminal. Unix socket paths must fit the host OS pathname limit (use a shorter database path if startup reports a control-socket error). Service-manager installation, Windows daemon management, log rotation, authentication, and a workflow dashboard are not included.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o mf ./cmd/mf
```

Tests include actual FFmpeg media fixtures, durable retry/restart recovery, HTTP handlers, and temporary daemon subprocesses. Integration tests require permission to bind loopback ports and Unix sockets. Real media fixture tests skip if FFmpeg is absent; CI installs it on macOS and Linux.
