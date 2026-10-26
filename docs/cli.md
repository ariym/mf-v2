# CLI reference

All commands accept `--config /path/to/mf.yaml`; otherwise configuration is read from the invoking directory. Relative SQLite/module paths resolve beside that config. Relative CLI media/output paths resolve from the invoking working directory. `mf help <command>` describes command flags. Structured hub, module, annotation and job output uses formatted tables or labeled lists by default. Pass `--json` for the full raw JSON response, for example `mf jobs list --json` or `mf tags show 1 --json`. List tables summarize long values; `show` displays complete details. ID-producing commands print the new ID; job submission with `--json` returns an ID object, or one final job object with `--wait`. Logs, executable paths and program I/O retain their text output.

## Initialization and server

| Command | Behavior |
| --- | --- |
| `mf init [-y]` | Create config and a new schema-v4 database; `-y` accepts defaults |
| `mf migrate` | Upgrade a validated v1 database with the server stopped |
| `mf version` | Print executable version |
| `mf update` | Check GitHub and print a pinned install command; `testing_branch: true` in the selected config opts into native testing releases, otherwise stable; works without config or database |
| `mf add <file-or-directory>` | Probe/import synchronously; directories recurse |
| `mf server start` | Run the API and worker in this terminal |
| `mf daemon start` | Start a detached server |
| `mf daemon stop [-f]` | Stop; `-f` interrupts unfinished work for restart |
| `mf daemon logs [--follow]` | Show the last 20 lines and optionally follow |

## Hub and modules

| Command | Flags / behavior |
| --- | --- |
| `mf hub health` | Check registry readiness |
| `mf hub list <tagtypes\|flows\|programs>` | Table with copyable `key@version` install names; `--json`, `--limit 100`, `--offset 0` |
| `mf hub browse [tagtypes\|flows\|programs]` | Interactive checkbox catalog; all kinds and pages by default |
| `mf hub show <kind> <key[@version]>` | Latest or exact definition |
| `mf hub versions <kind> <key>` | `--limit 100`, `--offset 0` |
| `mf hub resolve <flow[@version]>` | `--os`, `--arch`; defaults to this platform |
| `mf hub publish <kind> <manifest.json>` | Requires `MF_HUB_ADMIN_TOKEN` |
| `mf <tagtypes\|flows\|programs> list` | Installed entities |
| `mf <tagtypes\|flows\|programs> show <key[@version]>` | Installed definition/metadata |
| `mf <tagtypes\|flows\|programs> install <key[@version]>` | `--hub-url URL`; installs exact dependencies |
| `mf <tagtypes\|flows\|programs> remove <key>` | Refuses removal while referenced |
| `mf tagtypes add <manifest.json>` | Add a local definition, after its children |
| `mf <flows\|programs> path <key>` | Absolute executable path |
| `mf programs link <key> <executable>` | `--version 1`, optional `--input-formats '<JSON>'` |
| `mf programs exec <key> -- [arguments...]` | Ordinary program CLI and I/O passthrough |

`mf hub --url URL ...` overrides the hub endpoint. Remote hub reads and browsing work without local initialization; installation requires `mf init`. In `hub browse`, move with arrows or `j`/`k`, filter with `/`, toggle checkboxes with Space, and press Enter to review the selected exact versions. Press Enter again to install, or Esc to edit the selection. Dependencies are installed automatically. Each selection reports success or failure; Enter retries failures without repeating successful selections. Press `q` or Ctrl-C to exit (remaining work is cancelled). See [hub integration](hub.md) for installation recovery, version conflicts, private GitHub downloads and model preparation.

## Segments and tag instances

```sh
mf tagtypes add confidence.json
mf segments add /media/interview.mp4 00:00:03.125 00:00:06.5
mf tags add confidence@1 --segment 1 --value '0.95'
mf tags add confidence@1 --segment 1,2 --value '0.8'
```

For example, `confidence.json` can contain:

```json
{"key":"confidence","version":1,"data_type":"number","format":{"range":[0,1]}}
```

| Command | Behavior |
| --- | --- |
| `mf segments list [file]` | All segments or those for an imported file |
| `mf segments show <id>` | Includes unique attached instance IDs |
| `mf segments add <file> <start> <end>` | Imports the file and creates/reuses its normalized range |
| `mf segments update <id> <start> <end>` | Validate and change the range |
| `mf segments remove <id>` | Remove links and newly orphaned instances |
| `mf tags list [file]` | All instances or those attached to a file |
| `mf tags show <id>` | Full value and segment/JSON-Pointer links |
| `mf tags add <tagtype[@version]> --segment <ids> --value <JSON-or-@file>` | Create one shared instance |
| `mf tags update <id> --value <JSON-or-@file>` | Replace and revalidate the shared value |
| `mf tags remove <id>` | Remove the instance and all its links |

`--segment` accepts comma-separated IDs and repeated flags. Updates preserve existing JSON-Pointer links; edits that would invalidate one are rejected. Start/end are media timecodes, not dates. Definitions belong in `tagtype`; instances belong in `tag`. See [schema](schema.md).

## Jobs by path

```sh
mf jobs run transcribe@1 /media/interview.mp4 --wait
mf jobs run infer_audio_events . --input '{"options":{}}'
mf jobs run train_custom_speaker ./enrollment \
  --input '{"voice_id":"alice","label":"Alice"}'
mf jobs list /media --state completed
mf jobs list /media --state completed --json
mf jobs show <id>
mf jobs logs <id> --follow
mf jobs cancel <id>
mf jobs retry <id>
```

| Command | Flags / behavior |
| --- | --- |
| `mf jobs run <flow> [paths...]` | Defaults to current directory; imports media; starts daemon if needed |
| `mf jobs submit <flow>` | Raw flow input; starts daemon if needed |
| `mf jobs list [path]` | `--path`, `--state`, `--limit 100`, `--offset 0`; newest first |
| `mf jobs show <id>` | State, result, error and media-path metadata |
| `mf jobs wait <id>` | Wait; return failure for failed/cancelled jobs |
| `mf jobs logs <id>` | `--offset 0` in bytes, `--follow` |
| `mf jobs cancel <id>` | Request permanent cancellation |
| `mf jobs retry <id>` | New ID; retain the failed/cancelled job's input and pins |

Both `run` and `submit` accept `--input '{}'` (JSON or `@file`), `--output-dir PATH`, and `--wait`. `run` gathers the current recursive scan into one job, supplies absolute `input.media`, and defaults `input.output_dir` to `<database>.mf/results`. It includes file symlinks, skips deleted historical catalog records, and excludes bundled module directories and database state from recursive scans. Its input must not also supply `media`. `submit` passes its input through without that convention; any media used for result association must already be imported. Additional options/profile paths follow each flow's own input schema. Built-in `import_media` accepts one file/directory via `jobs run`.

Explicit cancellation differs from forced daemon shutdown: cancelled jobs do not resume. Retry produces a new job ID. Inspecting existing jobs requires the server to be running; submission, cancellation and retry start it when needed. Captured per-job logs are available for downloaded flows; compiled built-in activities retain their existing server diagnostics.

For local face matcher training, enrollment, and held-out accuracy evaluation with the linked InsightFace program, see [face accuracy testing](face-accuracy.md).

## Two-pane browser

```sh
mf browse /media
mf browse /media/interview.mp4
```

The left pane shows the live directory, including regular files with unknown extensions, and marks imported files. The right pane shows the selected imported file's tag timelines. Unknown files can be imported deliberately with `i`; job submission also imports missing media. On a selected directory, both `i` and `x` scan recursively for valid media, excluding bundled module directories and database state. `i` displays added, updated, skipped and failed counts; valid files are retained even if another file fails probing. `x` prompts for an installed flow and submits the directory's media together. Listing alone does not import files.

| Keys | Action |
| --- | --- |
| Up/Down or `j`/`k`, `/` | Select and filter directory entries |
| Enter, Backspace | Enter a directory or go to its parent |
| Tab | Switch active pane |
| `i`, `x` | Import the selected file/directory; enter an installed flow key to import and submit a job for it |
| Left/Right in timeline pane | Pan later/earlier |
| `+`/`-`, `0` in timeline pane | Zoom or reset |
| `n`/`p`, Enter in timeline pane | Select an instance; inspect its full JSON value |
| `H`, `r` | Toggle older analysis results; refresh directory/data |
| `?`, `q`, Ctrl-C | Help; quit and restore terminal |

Time starts on the right and increases toward the left. Numbers use value plots; Booleans and categories use interval lanes with legends/counts. Tag sets expand into child tracks. Known structured audio values expose timed event tracks; unfamiliar structures retain coverage tracks and JSON inspection. Latest job results are shown by default, with historical results available through `H`. Manual instances remain visible. The in-browser inspector truncates very large displays; `mf tags show` returns the complete value. The browser requires an interactive terminal of at least 55 columns × 12 rows.

## SQL to FFmpeg

```sh
mf sql-to-ffmpeg 'SELECT * FROM segments ORDER BY id'
mf sql-to-ffmpeg 'SELECT * FROM segments WHERE media_file_id=12 ORDER BY length(start),start' \
  --output ./selection.mp4
mf sql-to-ffmpeg 'SELECT * FROM segments ORDER BY id' \
  --output ./selection.mp4 --print-only > render.sh
```

Rendering is the default. A blinking destination prompt accepts Enter for `$cwd/<timestamp>.mp4`, or `.mp3` for an all-audio selection. Noninteractive commands require `--output` (`-o`). `--print-only` emits a shell-safe one-liner without rendering. `--overwrite` explicitly permits replacing an output; source files cannot be replaced. The printed argv and executed argv match, and execution does not invoke a shell. Prompts, FFmpeg progress and the saved path go to stderr; stdout contains the generated command.

Queries must be one read-only `SELECT` or `WITH`, returning `start`, `end`, and either `media_file_id` or `file_path`. Duplicate/ambiguous result column names are rejected. Results retain SQL order and repeated rows. Empty results, missing files and invalid ranges fail before rendering. Use `ORDER BY` to control clip order. For example:

```sql
SELECT DISTINCT s.*
FROM segments AS s
JOIN segment_tags AS st ON st.segment_id=s.id
JOIN tag AS t ON t.id=st.tag_id
JOIN tagtype AS tt ON tt.id=t.tag_type_id
WHERE tt.key='speaker' AND st.value_pointer<>''
ORDER BY s.media_file_id,length(s.start),s.start;
```

All-audio selections produce MP3 without cover art. Any playable video produces MP4, with black video for audio-only clips and silence for missing audio. Video is scaled/padded to a common size and frame rate; audio is normalized before concatenation. Sources are probed again for current stream metadata and duration. The first playable video and first audio stream are selected.

Defaults are editable under `sql_to_ffmpeg` in [mf.example.yaml](../mf.example.yaml): H.264/CRF 20/medium, 1920×1080/30 fps/yuv420p, AAC 192 kbps, stereo 48 kHz, or `libmp3lame` quality 2 for MP3. Missing extensions are appended; incompatible output extensions are rejected. FFmpeg/ffprobe must be installed, and the chosen FFmpeg build must provide configured encoders.

Protocol-v2 flows accept catalog segments without submitting file paths:

```sh
mf jobs run recognize_transcript_names@2 --segment 7 --wait
mf jobs run recognize_transcript_names@2 --segment 7,8 --wait
mf jobs list --state waiting
```

`--segment` may be repeated and cannot be combined with positional paths.
Prerequisites run as independent durable jobs; `--wait` includes prerequisite
waiting time. Set `--input '{"audio_stream_index":1}'` when the source has several
audio streams. Missing prerequisite data is generated for the entire source file.

## Request outputs

`mf request <tagtype[@version]> [paths...]` reviews missing installations and prerequisite jobs, waits, and returns requested tags. Use `--tagtype` for additional outputs, `--segment` for catalog ranges, `--flow` to choose a producer, `--input` for JSON options, `--dry-run` to preview, `--background` to return an ID, and `--yes` to accept installation/job prompts. `mf requests show <id>` and `mf requests wait <id>` retrieve accepted requests. See [output requests](requests.md).
