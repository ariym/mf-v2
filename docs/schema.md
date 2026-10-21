# Client schema v2

The hub's **tag** is a schema definition. In the client, this is always a **tag type** (`TagType`, the `tagtype` table, and the `tagtypes` CLI group). A client **tag** (`Tag`, the `tag` table, and the `tags` CLI group) is an instantiation containing a validated value. Hub transport manifests retain the hub's `tags` spelling; client definition/plan output translates it to `tag_types`. Instance values and embedded JSON schemas are never renamed.

## Upgrade

```sh
mf daemon stop
mf migrate
mf daemon start
```

Stop foreground servers too. `mf migrate` acquires the same exclusive database lock as the server. It validates the complete v1 application and workflow schema before applying a transactional additive migration. Media IDs, file metadata, timestamps, and durable workflow records remain intact. Running it on v2 validates without changing the schema. Foreign, modified, or future schemas are rejected. Normal commands never migrate automatically. `mf init` creates v2 for a new database.

## Entities

| Table | Stored data |
| --- | --- |
| `media_files` | Existing canonical file path, media type, duration in seconds, byte size, audit timestamps |
| `file_audio_metadata`, `file_video_metadata` | Existing metadata per stream |
| `tagtype` | Local ID, unique key, exact positive version, validated hub manifest as JSON, source, audit timestamps |
| `tag` | Local ID, `tag_type_id`, JSON `value`, nullable source job/result key, audit timestamps |
| `segments` | Local ID, media file ID, `start` and `end` timecodes, audit timestamps; unique by file/range |
| `segment_tags` | Segment ID, tag ID, `value_pointer`; composite primary key |
| `flow`, `program` | Local ID, unique key, exact version, JSON manifest, source, installation directory, executable, selected OS/architecture and artifact SHA-256, audit timestamps |
| `job_metadata` | Stable job ID, flow identity, input, persisted execution snapshot, paths, submission/cancellation/ingestion flags and terminal status |
| `job_media`, `job_modules` | Media associations and exact module/definition pins for durable jobs |
| `install_journal` | Staged/final path ownership for recovery before database registration |

Definition fields such as `data_type`, `format`, title and description are preserved inside `tagtype.manifest`. Query them with SQLite JSON functions. The client never stores hub definitions in `tag`.

## Segments and instances

`start` and `end` are media timestamps such as `00:01:23.450000`, stored as TEXT. They are neither numeric offsets nor recording date/time values. Hours can exceed 23 or 99. Input fractions support up to six digits; values are normalized with six fractional digits. Ranges are start-inclusive and end-exclusive, must have positive duration, and cannot exceed a known media duration. Audit fields remain UTC RFC3339 timestamps.

The CLI's segment representation is:

```json
{
  "id": 7,
  "media_file_id": 12,
  "start": "00:00:01.000000",
  "end": "00:00:02.500000",
  "tags": [31, 32],
  "created_at": "2026-10-20T12:00:00Z",
  "modified_at": "2026-10-20T12:00:00Z"
}
```

Tags attach through `segment_tags`. One schema-valid instance can be shared across several segments. `value_pointer` is empty for the complete value or contains a JSON Pointer to a timed event within it. Instance creation and all links commit together. Removing a segment removes its links and any newly unreferenced instances. Removing a tag removes all its links. Referenced tag types cannot be removed.

Instances support `string`, `number`, `boolean`, `tag_set`, and `json`. Primitive range/choice rules are enforced. A tag set is an object containing every declared child key with each child value validated against its exact installed version. JSON values use Draft 2020-12; external schema references and `$id` are disabled, matching the hub.

## Flow result ingestion

Protocol-v1 results may contain any JSON. Results with a `tags` array must identify each instance with `key`, exact `version`, and `value`. `media_path`, embedded `media.path`, or an unambiguous submitted file associates the result with media. Voice profiles can reference several enrollment files. Results referring to unknown media, unpinned definitions, invalid values or invalid ranges fail without partial writes.

Complete JSON values remain intact, including unknown upstream fields and artifact references. Each attaches to a whole-file segment. Known timed projections are linked without duplicating those values:

| Tag type | Timed projection |
| --- | --- |
| `speech@2` | `/transcript/segments` |
| `speaker@2` | `/diarization` |
| `audio_events@1` | `/events` |
| `diarized_transcript@2` | Nested speech segments and speaker diarization |

Both start and end must be supplied to create a timed projection. Missing/null event timestamps are not invented. Whole-file attachment requires a known positive duration. A stable job/result key and ingestion flag make retries idempotent.

For timestamp ordering beyond 99 hours, use `ORDER BY length(start), start` on normalized timecodes rather than plain lexical order.
