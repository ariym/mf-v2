# mf-hub client and executable modules

The client implements mf-hub's v1 catalog reads, pagination, immutable version history, publication, health check and flow dependency resolution. The hub's `tags` definitions are called `tagtypes` in the client. The HTTP mapping stays inside `internal/hub`; the client does not import the sibling mf-hub Go module or depend on that checkout at build time.

```sh
mf hub health
mf hub list tagtypes --limit 25 --offset 0
mf hub list flows --json
mf hub browse
mf hub browse programs
mf hub show flows transcribe@1
mf hub versions programs whisperx
mf hub resolve transcribe@1 --os darwin --arch arm64
mf hub --url https://hub.example.com list flows
```

Remote reads and browsing work without an initialized client database. Without a config they use `http://127.0.0.1:11748`; `--config` and `hub --url` select another configuration or endpoint. `hub list` displays a table with exact `key@version` install names and the matching install command; `--json` retains structured pagination output.

`mf hub browse` loads all pages of tag types, flows and programs; an optional kind limits the catalog. Use arrows or `j`/`k` to navigate, `/` to filter, and Space to select/deselect checkboxes. Enter opens the installation review; Enter again installs the selection, including dependencies, using the listed exact versions. Esc returns to selection. Installation requires an initialized local config/database. Results are reported per selection, and Enter retries only failed selections. Completed installations are retained if a later selection fails or the browser is cancelled. `q` / Ctrl-C exit and cancel remaining installation work.

## Install and remove

```sh
mf flows install transcribe@1
mf programs install auditok@1
mf tagtypes install speech@2
mf flows list
mf programs show auditok
mf programs path auditok
mf flows remove transcribe
```

Omitting `@version` selects the hub's latest version once, then pins exact versions for the complete installation. `install --hub-url URL` overrides the configured hub for that operation. Flow installation installs all required programs, child flows and tag types. Standalone tag-type installation resolves its nested exact-version dependencies too.

The default layout, relative to the config directory, is:

```text
mf_modules/
  programs/<key>/<version>/<archive contents>
  flows/<key>/<version>/<archive contents>
```

`modules_path` changes the root. Downloaded flows are standalone compiled Go executables. Programs are external executables with their ordinary interfaces; the client preserves bundled runtimes and does not impose the flow protocol on programs.

Only one version of a key is installed at a time. Identical installed manifests are reused. Conflicting versions or changed immutable manifests fail before downloads; there is no implicit upgrade or side-by-side installation. Remove an unused version before replacing it. Removal is blocked by dependent flows/tag sets, tag instances, and unfinished jobs. Linked external programs are unregistered without deleting their executable.

Downloads require HTTPS and a matching manifest SHA-256. Archives are extracted into staging directories, rejecting traversal, escaping/dangling links, devices, duplicates and writes through links. Contained runtime links are supported. Limits are 8 GiB downloaded, 32 GiB unpacked, and 200,000 archive entries. Installation holds a filesystem lock and records staged destinations in SQLite. Atomic renames precede transactional registration; interrupted unregistered paths are cleaned before the next module-management operation.

## Credentials and publishing

```sh
export MF_HUB_ADMIN_TOKEN='your-hub-publication-token'
mf hub publish tagtypes confidence.json
mf hub publish programs program-manifest.json
mf hub publish flows flow-manifest.json
```

Publication accepts the original hub manifest format. Publish dependencies first. Existing immutable versions return a conflict, including identical republication. Release archives must already be uploaded: hub publication registers metadata and does not build or upload release assets. Publication credentials require HTTPS outside localhost and are not sent on catalog reads or redirects.

Private GitHub release assets require repository credentials, separate from the hub's publication token:

```sh
export GH_TOKEN='your-repository-read-token'
mf flows install transcribe@1
```

`GITHUB_TOKEN` is a fallback when `GH_TOKEN` is empty. If neither is set, the client uses the existing GitHub CLI login (`gh auth login`), when available. Credentials must have access to the private repository. The client locates private release assets through GitHub's API. Authorization is sent only to `api.github.com`, never a redirected asset host. Tokens are not written into manifests or configuration. Download errors identify the failed program or flow; a GitHub 404 can mean either a missing asset or insufficient repository access.

## Existing programs and model preparation

```sh
mf programs link my-tool /absolute/path/to/tool --version 1 \
  --input-formats '[{"container":".wav"}]'
mf programs exec my-tool -- --help
mf programs exec whisperx -- prepare
mf programs exec whisperx -- ready
mf programs exec whisperx -- upstream --help
```

`link` only requires a regular executable, key and positive compatibility version. Input-format declarations are optional; absent declarations mean unknown compatibility, so file-oriented flow jobs requiring that program are rejected. `exec` forwards arguments, stdin, stdout, stderr, failure and cancellation without probing a common program interface.

The currently checked-in audio hub manifests target darwin/arm64. Other platforms fail resolution when an artifact is missing. Model preparation, licenses, downloaded weights and program-specific options remain the responsibility of each program's documented interface; the client never silently provisions ML models.

## Flow protocol and durable execution

Jobs pin their root flow, complete transitive dependencies, executable paths and versions. Newly installed flows work through a generic adapter without restarting the worker. A subprocess runs inside an activity, with one JSON request on stdin containing `protocol_version: 1`, stable `job_id`, canonical absolute `db_path`, flow `input`, and absolute `programs`/`flows` maps. The root is excluded from the child-flow map. Requests are limited to 1 MiB; responses to 64 MiB. Stdout is reserved for the response; stderr becomes a per-job log.

Success requires exit 0 and one valid protocol-v1 response with a result. Nonzero exits, malformed responses and declared errors fail. The client validates program format declarations against file extensions and stream metadata; audio codec and sample rate must match the same stream.

Process groups are terminated on cancellation or server shutdown, with forced cleanup after a bounded grace period. A forced daemon stop preserves unfinished workflow checkpoints for restart. Explicit cancellation is durable and terminal. Submission intent is saved before engine creation and reconciled on startup, preventing lost work in that crash window. Execution and ingestion are separate activities, so a checkpointed result can be ingested again safely without rerunning inference.

The files in `internal/hub/testdata` are contract fixtures copied from the sibling mf-hub audio catalog, not runtime dependencies.

## Data prerequisites (protocol v2)

A flow can opt into `execution.protocol_version: 2` and declare data requirements
separately from its existing schema/program dependencies:

```json
{
  "execution": {
    "protocol_version": 2,
    "requires": [{
      "name": "transcript",
      "any_of": [
        {"kind": "tag", "key": "speech", "version": 2},
        {"kind": "tag", "key": "diarized_transcript", "version": 2}
      ],
      "producer": {"flow": "transcribe", "version": 2, "input": {}}
    }],
    "produces": [{"kind": "tag", "key": "transcript_names", "version": 1}]
  }
}
```

Tag identities must also appear in the manifest's exact `tags` pins. Producer
flows participate in installation, pinning, conflict and cycle checks and must
declare an acceptable output. `tags` alone never triggers data generation. Audio
requirements use `{"kind":"audio","key":"audio","version":1}` and
`{"builtin":"extract_audio","version":1}` as the producer.

The client resolves existing whole-value tags covering the requested range
(preferred alternatives in declaration order, newest tag ID first), then shares
active producer jobs or creates missing prerequisites recursively. Missing data
is generated for the whole file. A transaction saves all nodes, edges, frozen
ranges, bindings and module pins before any job is dispatched. A one-second
reconciler releases dependents only after successful workflows and committed,
validated outputs. Waiting jobs consume no activity worker slot. Failures and
explicit prerequisite cancellation fail their dependents; cancelling a consumer
leaves its producer jobs reusable. Retry retains the saved flow pins and ranges
and resolves the prerequisites again. Source-tag digests are checked before
execution and transactionally during result ingestion.

Protocol v2 retains the existing envelope fields and adds `targets` and
`bindings`. Targets contain `media_file_id`, `media_path`, optional `segment_id`,
normalized `start`/`end`, optional `stream_index`, and `whole_file`. Bindings contain
`target_index`, requirement `name`, exact `kind`/`key`/`version`, and either a
`tag_id`/`sha256` or audio `path`/`sha256`/`source_sha256`/`stream_index`. Responses
must echo `protocol_version: 2`; every result tag supplies `target_index`. Derived
tags retain the original `source_tag_id` and `source_value_sha256`; their links
attach to the requested range. Original JSON values and timed coordinates remain
intact. Existing protocol-v1 jobs and manifests retain their execution contract.

Audio extraction uses local FFmpeg and FFprobe, selects the requested source
stream and atomically publishes a mono, 16 kHz PCM WAV with source/output hashes.
Suitable existing standalone WAV audio is reused directly. Multiple source audio
streams require `input.audio_stream_index`; a source without audio fails. Missing
or corrupt derived audio is regenerated. Compatibility checks use the supplied
resolved audio and each flow's direct programs. No model downloads or module
installation occur as a side effect of running a job.

Deploy mf-hub support, stop mf, run `mf migrate`, and start the updated client
before installing new immutable v2 flow releases. Install modules and prepare
models explicitly as before.

The cross-repository smoke test uses the real packaged flows and FFmpeg with
fixture inference programs. It creates a temporary catalog and stops only its own
daemon. For the currently supported darwin/arm64 flow releases, prepare local
artifacts (from the respective flow repositories) with:

```sh
go build -o /tmp/mf-prerequisite-build/mf ./cmd/mf
# In mf-flow-transcribe:
python3 scripts/release.py --repo ariym/mf-flow-transcribe --version 2 --platform darwin/arm64 --output /tmp/mf-prerequisite-build/transcribe
# In mf-flow-recognize-transcript-names:
python3 scripts/release.py --repo ariym/mf-flow-recognize-transcript-names --version 2 --platform darwin/arm64 --output /tmp/mf-prerequisite-build/names
# Back in mf-v2:
python3 scripts/test_prerequisite_flows.py --build-dir /tmp/mf-prerequisite-build
```
