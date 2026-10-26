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

## Web workspace API (schema v3)

The standalone `mf-web` application uses HTTP exclusively. Its own Go server streams catalog paths and generates thumbnails/browser proxies; mf continues to own catalog data, module installation, drafts, and durable jobs. `GET /api/v1/capabilities` reports `workspace_version: 1`, `schema_version: 4`, and the mf server's `os`/`arch`.

| Method and path | Result |
| --- | --- |
| `GET /api/v1/schema` | `{items:[{name,sql}]}` for catalog tables |
| `POST /api/v1/query` | Ordered `{items:[{media,segment_id?,start,end}]}` |
| `GET /api/v1/media/{id}/annotations` | `{segments,tags}`, preserving shared links and JSON Pointers |
| `GET /api/v1/segments?media_file_id=...` | `{items}` |
| `GET /api/v1/segments/{id}` | One segment |
| `POST /api/v1/segments` | Create `{media_file_id,start,end}`; return segment |
| `PATCH /api/v1/segments/{id}` | Update `{start,end}`; return segment |
| `DELETE /api/v1/segments/{id}` | Remove segment and orphaned instances |
| `GET /api/v1/tags?media_file_id=...` | `{items}` with shared links and full values |
| `POST /api/v1/tags` | Create `{key,version,value,links:[{segment_id,value_pointer}]}` |
| `PATCH /api/v1/tags/{id}` | Replace schema-validated `{value}` across all links |
| `DELETE /api/v1/tags/{id}` | Remove instance and all its links |
| `GET /api/v1/modules/{kind}` | `{items}` containing installed key/version/manifest metadata |
| `GET /api/v1/hub/{kind}?limit=100&offset=0` | Hub catalog page |
| `GET /api/v1/hub/{kind}/{key}/versions?limit=100&offset=0` | Hub version history page |
| `POST /api/v1/modules/{kind}/resolve` | Review exact `{ref:"key@version"}` dependency plan |
| `POST /api/v1/modules/{kind}/install` | Install exact `{ref}` and dependencies |
| `POST /api/v1/modules/{kind}/update` | Stage and atomically replace unused versions for `{ref}` |
| `DELETE /api/v1/modules/{kind}/{key}` | Remove an unused installed entity |
| `GET /api/v1/drafts/{kind}` | `{items}` of saved documents |
| `GET /api/v1/drafts/{kind}/{id}` | One document |
| `POST /api/v1/drafts/{kind}` | Create `{name,document}` |
| `PUT /api/v1/drafts/{kind}/{id}` | Replace `{name,document}` |
| `DELETE /api/v1/drafts/{kind}/{id}` | Remove document |
| `POST /api/v1/drafts/jobs/{id}/run` | HTTP 202 `{id}` for an independent execution |
| `GET /api/v1/jobs/{id}/submission` | Saved `{flow,data:{kind:"raw"},input,original_paths}` for copying a run |
| `POST /api/v1/manifests/validate` | Validate `{kind,manifest}`; return normalized manifest and dependencies |

Module kinds are `tagtypes`, `flows`, and `programs`. Manifest payloads retain the hub's wire spelling (`tags`), allowing direct export; the UI labels these dependencies as tag types. Operations reuse mf's download credentials and installation lock. Replacement checks run before downloads and again in the commit transaction. Changed immutable manifests with the same version are rejected; references from tags, dependent manifests, or unfinished job pins block replacement. Downloads finish before old registrations change, and the installation journal recovers interrupted filesystem work.

Draft kinds are `manifests` and `jobs`. Document records contain `id`, `name`, `document` (text), `created_at`, and `modified_at`. Draft text may contain incomplete JSON; validation/execution rejects invalid content without changing runs or installed entities. Names are required (maximum 256 characters); documents and request bodies are limited to 1 MiB. New workspace request bodies reject unknown/duplicate fields and trailing JSON.

A selection query supplies `mode: "media"` or `"segments"` and exactly one of `sql` or `filters`. Filters support `media_ids`, `tag_type`, and `value` (case-insensitive substring in stored JSON). Media SQL must return positive catalog `media_file_id` values. Segment SQL additionally returns `start` and `end` timecode strings and may include `segment_id`. Every range is validated against the catalog duration. No filesystem probe occurs; returned paths/metadata come from mf. Order and duplicates are preserved. Empty results return an empty array; more than 1,000 results fail explicitly. SQL runs through a single-statement SELECT/WITH validator on a read-only, query-only SQLite connection with a five-second timeout.

```json
{"mode":"segments","sql":"SELECT media_file_id,id AS segment_id,start,end FROM segments ORDER BY media_file_id,length(start),start,id"}
```

A saved job document has this structure:

```json
{"flow":"transcribe@1","data":{"kind":"media","media_ids":[12,13]},"input":{"options":{}}}
```

Data kinds are `segments` (`segment_ids`), `media` (`media_ids`), `paths` (`paths`), `sql` (`sql` returning `media_file_id`), and `raw` (pass the `input` object directly). SQL resolves and deduplicates media when Run is clicked; catalog paths are then submitted using mf's existing file-oriented convention. Submitted runs retain their own normalized input and exact module pins. Editing the saved definition does not modify any prior run. The header's future search UI does not call these query endpoints.

## Segment flows and prerequisites

Schema-v4 clients support segment targets on the existing submission endpoint:

```json
{"flow":"recognize_transcript_names@2","segment_ids":[7],"input":{}}
```

`segment_ids` and `paths` are mutually exclusive. The flow must opt into protocol
v2. Ranges, media identity and stream selection are frozen at submission; later
segment edits do not change the run. Saved definitions support
`data: {"kind":"segments","segment_ids":[7]}`. The CLI equivalent is:

```sh
mf jobs run recognize_transcript_names@2 --segment 7 --wait
```

Job responses add `targets`, `prerequisite_ids`, and `waiting_reason`. `waiting`
is a nonterminal state supported by list filters, cancellation, and CLI `--wait`.
Prerequisite jobs appear independently in normal job listings. A failed or
cancelled prerequisite prevents dependent execution; errors identify the job or
missing output. Saved submissions include original targets and reconstruct the
segment/media selection when copied. Capabilities report `schema_version: 4`,
`flow_protocols: [1,2]`, and `builtin_producers: {"extract_audio":1}`.
