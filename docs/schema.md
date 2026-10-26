# Client schema v5

The hub's **tag** is a schema definition. In the client, this is always a **tag type** (`TagType`, the `tagtype` table, and the `tagtypes` CLI group). A client **tag** (`Tag`, the `tag` table, and the `tags` CLI group) is an instantiation containing a validated value. Hub transport manifests retain the hub's `tags` spelling; client definition/plan output translates it to `tag_types`. Instance values and embedded JSON schemas are never renamed.

## Upgrade

```sh
mf daemon stop
mf migrate
mf daemon start
```

Stop foreground servers too. `mf migrate` acquires the same exclusive database lock as the server. It validates the complete v1, v2, v3, or v4 application and workflow schema before applying a transactional additive migration. Media IDs, file metadata, timestamps, and durable workflow records remain intact. Running it on v5 validates without changing the schema. Foreign, modified, or future schemas are rejected. Normal commands never migrate automatically. `mf init` creates v5 for a new database. Schema v3 added `manifest_drafts` and `job_definitions`, each with an ID, name, editable document text, and audit timestamps. Drafts are separate from installed immutable manifests and execution snapshots.

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
| `install_journal` | Staged/final/previous path ownership for installation and replacement recovery |
| `media_requests` | Approved request fingerprint, exact plan, setup/execution state, errors and timestamps |
| `production_coverage` | Successful source/range/tag-type coverage and output digests, including zero-event results |
| `manifest_drafts`, `job_definitions` | Editable draft names, document text, and UTC audit timestamps |

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

Protocol-v1 results may contain any JSON. Results with a `tags` array must identify each instance with `key`, exact `version`, and `value`. `media_path`, embedded `media.path`, or an unambiguous submitted file associates the result with media. Voice profiles and `face_profile@1` can reference several enrollment files through `examples[].media.path`; one shared profile instance links to all submitted enrollment files. Reference photos remain provenance and need not be imported. Results referring to unknown media, unpinned definitions, invalid values or invalid ranges fail without partial writes.

Complete JSON values remain intact, including unknown upstream fields and artifact references. Each attaches to a whole-file segment. Known timed projections are linked without duplicating those values:

Flows deriving results from existing tags may supply `source_tag_id` and
`source_value_sha256` instead of `media_path`. The digest is the lowercase SHA-256
of the exact UTF-8 JSON text in the source tag's `value` column. Both fields are
required together; the source's exact tag type/version must be pinned by the flow.
Ingestion checks that the source still exists and its digest matches, then attaches
the result to the source's whole-value links (`value_pointer = ''`). Source event
pointers are not copied to the new value. The derived job also gains the source
media associations. Validation and writes share the ingestion transaction; changed
or deleted sources fail without partial tags or links. These results need no
submitted media files and are suitable for raw `mf jobs submit` calls.

| Tag type | Timed projection |
| --- | --- |
| `speech@2` | `/transcript/segments` |
| `speaker@2` | `/diarization` |
| `audio_events@1` | `/events` |
| `face_tracks@1` | `/tracks` |
| `diarized_transcript@2` | Nested speech segments and speaker diarization |

Both start and end must be supplied to create a timed projection. Missing/null event timestamps are not invented. Whole-file attachment requires a known positive duration. A stable job/result key and ingestion flag make retries idempotent.

For timestamp ordering beyond 99 hours, use `ORDER BY length(start), start` on normalized timecodes rather than plain lexical order.

## Schema v4: prerequisite execution

`mf migrate` now upgrades v1/v2/v3 catalogs transactionally to v4. Existing catalog,
draft, module, and workflow records are preserved; normal commands still require
an explicit migration with the daemon stopped.

`job_execution` persists immutable target JSON, resolved binding JSON, scheduling
state, active-work identity and failures/completion time. A partial unique index
shares active prerequisites while allowing terminal work to be retried.
`job_dependencies` connects a target's named requirement to its prerequisite job.
`audio_artifacts` records the producing job, original media and stream, derived
path, output checksum and source checksum. Job metadata and module pins remain
the durable submission outbox. V1 workflow names remain registered for replay;
v2 workflows use a separate stable name and execute/ingest activity pair.
