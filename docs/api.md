# HTTP API

Start the server with `mf server start` or `mf daemon start`. All responses are JSON. Errors use `{"error":"description"}`. The default base URL is `http://localhost:11747`.

| Method and path | Response |
| --- | --- |
| `GET /healthz` | Readiness, `app_name`, and executable `version` |
| `GET /api/v1/media?limit=100&offset=0` | `{items, limit, offset}`; limit 1–1000, nonnegative offset |
| `GET /api/v1/media/{id}` | Media record and all audio/video metadata |
| `POST /api/v1/jobs` | HTTP 202, `{id}`, and a `Location` header |
| `GET /api/v1/jobs/{id}` | `{id, state, result?, error?, created_at, completed_at}` |

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
