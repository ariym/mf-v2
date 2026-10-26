# Request media outputs

`mf request` takes desired tag-type keys and returns their stored values. It discovers producing protocol-v2 flows from `execution.produces`, installs missing software after approval, provisions declared model profiles, and queues missing data prerequisites.

```sh
mf request diarized_transcript video.mp4
mf request diarized_transcript@2 video.mp4 --json
mf request audio_events video.mp4 --tagtype speech
mf request diarized_transcript --segment 42
mf request diarized_transcript video.mp4 --input '{"audio_stream_index":1}'
mf request diarized_transcript video.mp4 --flow transcribe@3 --dry-run
mf request diarized_transcript video.mp4 --background --yes
mf requests show REQUEST_ID
mf requests wait REQUEST_ID
```

Use exact catalog keys; descriptions such as “labeled yawns” are not interpreted as natural language. A corresponding event producer must exist in the catalog. The command uses installed tag-type versions by default, otherwise resolves the hub's latest once. Explicit `@version` wins. Installed version conflicts require explicit resolution; requests do not upgrade modules.

## Review and execution

Each missing flow/program and preparation profile has its own download-size prompt ending in `Y/n`. Empty input accepts; `n`, invalid answers, or EOF stop before execution. The command then prints all new preparation and inference jobs in dependency order and asks to assign them. All approvals precede downloads, except missing FFprobe must be separately approved and installed to inspect uncataloged files before generating the exact plan.

Prompts, choices and progress use stderr. Stdout contains requested results, or a request ID with `--background`. `--json` retains JSON output and does not imply approval. `--yes` accepts installation/job prompts; ambiguous producers still need a selection or `--flow`. `--dry-run` installs nothing and submits nothing; it reports an incomplete preview if FFprobe is unavailable.

Requests retain their exact approved manifests. A changed preview requires another review. Admission may share identical work admitted concurrently; it never adds unapproved work. Graph nodes, edges, frozen ranges and executable snapshots are committed together before dispatch. Setup state is saved before installation and reconciled after restart. Preparation is rerun only when its readiness check fails. Stopping a waiting CLI leaves its accepted request running; its error reports the request ID.

Successfully installed modules and completed outputs survive a later failure. Request setup logs are in `<sqlite_path>.mf/requests/<request_id>.log`; inference diagnostics use `mf jobs logs`. Retry a failed request by issuing the command again and reviewing the remaining work. Unfinished requests protect their pinned modules from removal or replacement.

## Manifest additions

Executable artifacts may declare `download_bytes`, a nonnegative integer covering the compressed archive. Legacy explicit installation commands accept omitted sizes; automatic requests require sizes for missing packages. Installations check archive size and SHA-256. Release generators must calculate both from the final archive.

Programs can declare named preparation profiles:

```json
{
  "preparation": {
    "transcribe": {
      "ready_args": ["ready", "--models", "mlx-small,cpu-small,align-en"],
      "prepare_args": ["prepare", "--models", "mlx-small,cpu-small,align-en"],
      "download_bytes": 1200000000
    }
  }
}
```

The size above illustrates the contract; release tooling must supply the size of its pinned files. Optional `required_env` lists credential variable names. Optional `license` describes upstream access requirements. Credential values stay in the daemon environment and are never saved in request plans. Export credentials before starting the daemon; restart an existing daemon if its environment is missing them. Upstream license acceptance remains external to mf; `--yes` does not accept it.

Flows select direct program profiles and declare their outputs:

```json
{
  "execution": {
    "protocol_version": 2,
    "preparation": {"whisperx":"transcribe"},
    "produces": [{"kind":"tag","key":"audio_events","version":1}],
    "allow_empty": ["audio_events"]
  }
}
```

`allow_empty` explicitly permits successful detection runs with no matching tags. Other declared outputs must be present after ingestion. Successful coverage includes source hashes and output digests so an empty event set can be reused, and deleted event outputs invalidate coverage. Results include media/range, exact tag-type identity, tag IDs, and original values. Only the selected result generation is returned.

Existing `execution.requires` declarations continue to describe transcript/audio prerequisites; schema pins alone do not generate data. Prepared profiles are selected only for flows that will run and their executable child flows. Inference programs retain their own interfaces; no shell is used to execute preparation arguments.

## Deployment

Stop the daemon and run `mf migrate` to upgrade the local schema to v5, then restart. Deploy the matching hub manifest contract before publishing new immutable program/flow releases. The transcribe v3 source selects pyannote/WhisperX v2 preparation profiles and emits `diarized_transcript@2`; older releases remain unchanged. Finish unfinished jobs using older names releases before migration: they explicitly reject schema v5. After migration, replace those installed modules with the new compatible releases before submitting more work.

Managed FFmpeg and FFprobe packages are ordinary platform-specific hub programs. A suitable host executable is reused; otherwise requests resolve the matching managed package. Build native release artifacts locally with:

```sh
python3 scripts/release_media_tools.py --repo OWNER/REPOSITORY \
  --build-dir /tmp/mf-media-tools-build --output /tmp/mf-media-tools-release
```

The builder verifies pinned FFmpeg source, creates static native binaries, and emits `ffmpeg.json`, `ffprobe.json`, archives and checksums. Upload those artifacts to the manifest release URLs, then publish the two program manifests. Building does not publish anything. Packages are needed for each platform you intend to support; unsupported platforms fail during resolution.
