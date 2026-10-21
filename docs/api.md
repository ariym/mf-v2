# HTTP API

Start the server with `mf server start` or `mf daemon start`. All responses are JSON. Errors use `{"error":"description"}`. The default base URL is `http://localhost:11747`.

| Method and path | Response |
| --- | --- |
| `GET /healthz` | Readiness, `app_name`, and executable `version` |
| `GET /api/v1/media?limit=100&offset=0` | `{items, limit, offset}`; limit 1–1000, nonnegative offset |
| `GET /api/v1/media/{id}` | Media record and all audio/video metadata |
| `POST /api/v1/jobs` | HTTP 202, `{id}`, and a `Location` header |
| `GET /api/v1/jobs?path=...&state=...&limit=100&offset=0` | `{items, limit, offset}`, newest first |
| `GET /api/v1/jobs/{id}` | `{id, state, result?, error?, created_at, completed_at, flow?, paths?, cancel_requested?}` |
| `POST /api/v1/jobs/{id}/cancel` | Durable cancellation request; `{cancel_requested:true}` |
| `POST /api/v1/jobs/{id}/retry` | HTTP 202, `{id}` for a new retry job |
| `GET /api/v1/jobs/{id}/logs?offset=0` | `{data,next_offset}`; up to 1 MiB from that byte offset |

Submit a media import with this JSON body:

```json
{"flow":"import_media","input":{"path":"/path/to/media"}}
```

A successful submission returns immediately:

```json
{"id":"a-job-uuid"}
```

Poll `GET /api/v1/jobs/a-job-uuid`. A completed import result contains `added`, `updated`, `skipped`, and `failed` counters. Failures expose an `error`. Direct CLI imports use `mf add /path/to/media` and wait for completion.

`input.path` is a server-local file or directory. Relative paths resolve against the config directory; directories are recursive. Uploads and remote URLs are not supported. The built-in flow retries transient errors up to three total attempts. Invalid paths and partial imports fail without automatic retry; successful files in a partial import remain committed.

Requests with malformed JSON, unknown fields/flows, or invalid input return 400. Missing records return 404, unexpected storage failures return 500, and submissions after shutdown begins return 503. JSON request bodies are limited to 1 MiB. `completed_at` is null until a terminal outcome. Waiting/retrying started workflows remain `running`.

Forced daemon shutdown is not a job cancellation: unfinished jobs resume after restart and keep their IDs. The API intentionally has no shutdown route; use `mf daemon stop` or `mf daemon stop -f` locally.

## File-oriented hub jobs

An installed flow can be submitted through the existing `flow`/`input` contract. Add optional `paths` to import missing files and supply the file-oriented input convention:

```json
{"flow":"transcribe@1","paths":["/media/interview.mp4"],"input":{"options":{}}}
```

Directories are recursive and all selected media are passed in one job. Relative API paths resolve beside the server config. The server supplies absolute `input.media` and creates `input.output_dir` (default `<database>.mf/results`). Supplying both `paths` and `input.media` is rejected. Without `paths`, input remains flow-specific, and media needed for result association must already exist in the catalog. A raw flow input must be a JSON object. Missing executable versions, incompatible program formats and invalid input return 400.

Submission metadata and exact module pins are durable before engine creation; incomplete submissions recover on server restart. Flow subprocesses receive the version-1 envelope described in [hub integration](hub.md). Result ingestion validates exact tag-type schemas and commits segments, shared instances and links transactionally. Unknown JSON result fields are retained.

Cancellation is valid for queued/running jobs and prevents resumption; completed/failed/cancelled jobs return 409. Retrying a failed/cancelled job creates a new ID using its saved input and pins. Existing pre-v2 jobs remain readable but have no client submission snapshot for retry. Cancellation and retry of missing jobs return errors; logs return 404 for missing jobs. New client CLI submissions start the local daemon when needed; the HTTP API itself does not start a server.

Job list pagination uses limit 1–1000 and a nonnegative offset. The optional path filter matches canonical file/directory associations. States include `cancelled`; a cancellation request may remain visible as `cancel_requested` until the worker reaches a terminal outcome. Logs are captured for downloaded flows; compiled activities use the server's existing logs. Force-stopping the daemon preserves work instead of setting `cancel_requested`.
