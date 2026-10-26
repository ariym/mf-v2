package store

import (
	"context"
	"fmt"
)

const schemaV2 = `
CREATE TABLE tagtype (
 id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, version INTEGER NOT NULL CHECK(version>0),
 manifest TEXT NOT NULL CHECK(json_valid(manifest)), source TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE segments (
 id INTEGER PRIMARY KEY, media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
 start TEXT NOT NULL, end TEXT NOT NULL, created_at TEXT NOT NULL, modified_at TEXT NOT NULL,
 UNIQUE(media_file_id,start,end)
);
CREATE TABLE tag (
 id INTEGER PRIMARY KEY, tag_type_id INTEGER NOT NULL REFERENCES tagtype(id) ON DELETE RESTRICT,
 value TEXT NOT NULL CHECK(json_valid(value)), source_job_id TEXT, source_key TEXT,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL, UNIQUE(source_job_id,source_key)
);
CREATE TABLE segment_tags (
 segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
 tag_id INTEGER NOT NULL REFERENCES tag(id) ON DELETE CASCADE,
 value_pointer TEXT NOT NULL DEFAULT '', PRIMARY KEY(segment_id,tag_id,value_pointer)
);
CREATE INDEX idx_segment_tags_tag ON segment_tags(tag_id);
CREATE TABLE flow (
 id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, version INTEGER NOT NULL CHECK(version>0),
 manifest TEXT NOT NULL CHECK(json_valid(manifest)), source TEXT NOT NULL,
 install_dir TEXT NOT NULL, executable TEXT NOT NULL,
 platform_os TEXT NOT NULL, platform_arch TEXT NOT NULL, artifact_sha256 TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE program (
 id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, version INTEGER NOT NULL CHECK(version>0),
 manifest TEXT NOT NULL CHECK(json_valid(manifest)), source TEXT NOT NULL,
 install_dir TEXT NOT NULL, executable TEXT NOT NULL,
 platform_os TEXT NOT NULL, platform_arch TEXT NOT NULL, artifact_sha256 TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE job_metadata (
 id TEXT PRIMARY KEY, flow_key TEXT NOT NULL, flow_version INTEGER NOT NULL,
 input TEXT NOT NULL CHECK(json_valid(input)), snapshot TEXT NOT NULL CHECK(json_valid(snapshot)),
 paths TEXT NOT NULL CHECK(json_valid(paths)), submitted INTEGER NOT NULL DEFAULT 0,
 cancel_requested INTEGER NOT NULL DEFAULT 0, ingested INTEGER NOT NULL DEFAULT 0,
 terminal_state TEXT, created_at TEXT NOT NULL
);
CREATE TABLE job_media (
 job_id TEXT NOT NULL REFERENCES job_metadata(id) ON DELETE CASCADE,
 media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE RESTRICT,
 PRIMARY KEY(job_id,media_file_id)
);
CREATE TABLE job_modules (
 job_id TEXT NOT NULL REFERENCES job_metadata(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, key TEXT NOT NULL, version INTEGER NOT NULL,
 PRIMARY KEY(job_id,kind,key)
);
CREATE TABLE install_journal (
 id TEXT PRIMARY KEY, paths TEXT NOT NULL CHECK(json_valid(paths)), created_at TEXT NOT NULL
);
UPDATE mf_schema_migrations SET version=2;
`

var v2Objects = map[string]bool{
	"table tagtype": true, "table segments": true, "table tag": true, "table segment_tags": true,
	"index idx_segment_tags_tag": true, "table flow": true, "table program": true,
	"table job_metadata": true, "table job_media": true, "table job_modules": true, "table install_journal": true,
}

const schemaV3 = `
CREATE TABLE manifest_drafts (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, document TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE job_definitions (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, document TEXT NOT NULL,
 created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
UPDATE mf_schema_migrations SET version=3;
`

var v3Objects = map[string]bool{"table manifest_drafts": true, "table job_definitions": true}

const schemaV4 = `
CREATE TABLE job_execution (
 job_id TEXT PRIMARY KEY REFERENCES job_metadata(id) ON DELETE CASCADE,
 targets TEXT NOT NULL CHECK(json_valid(targets)), bindings TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(bindings)),
 state TEXT NOT NULL CHECK(state IN ('waiting','ready','completed','failed','cancelled')),
 dedup_key TEXT, error TEXT NOT NULL DEFAULT '', completed_at TEXT
);
CREATE UNIQUE INDEX idx_job_execution_active ON job_execution(dedup_key) WHERE state IN ('waiting','ready');
CREATE TABLE job_dependencies (
 job_id TEXT NOT NULL REFERENCES job_metadata(id) ON DELETE CASCADE,
 target_index INTEGER NOT NULL, requirement TEXT NOT NULL,
 prerequisite_id TEXT NOT NULL REFERENCES job_metadata(id) ON DELETE RESTRICT,
 PRIMARY KEY(job_id,target_index,requirement)
);
CREATE INDEX idx_job_dependencies_prerequisite ON job_dependencies(prerequisite_id);
CREATE TABLE audio_artifacts (
 id INTEGER PRIMARY KEY, job_id TEXT NOT NULL UNIQUE REFERENCES job_metadata(id) ON DELETE RESTRICT,
 media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE RESTRICT,
 stream_index INTEGER NOT NULL, path TEXT NOT NULL, sha256 TEXT NOT NULL, source_sha256 TEXT NOT NULL,
 created_at TEXT NOT NULL
);
UPDATE mf_schema_migrations SET version=4;
`

var v4Objects = map[string]bool{"table job_execution": true, "index idx_job_execution_active": true, "table job_dependencies": true, "index idx_job_dependencies_prerequisite": true, "table audio_artifacts": true}

const schemaV5 = `
CREATE TABLE media_requests (
 id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL UNIQUE, plan TEXT NOT NULL CHECK(json_valid(plan)),
 state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, modified_at TEXT NOT NULL
);
CREATE TABLE production_coverage (
 job_id TEXT NOT NULL REFERENCES job_metadata(id) ON DELETE CASCADE,
 media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
 start TEXT NOT NULL, end TEXT NOT NULL, stream_index INTEGER,
 key TEXT NOT NULL, version INTEGER NOT NULL, source_sha256 TEXT NOT NULL, output_digest TEXT NOT NULL,
 PRIMARY KEY(job_id,media_file_id,start,end,key,version)
);
UPDATE mf_schema_migrations SET version=5;
`

var v5Objects = map[string]bool{"table media_requests": true, "table production_coverage": true}

// Migrate must be called while holding the database's exclusive server lock.
// It validates the entire legacy schema before changing anything.
func Migrate(ctx context.Context, path string) error {
	db, err := connect(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err = db.QueryRowContext(ctx, "SELECT version FROM mf_schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("not an mf database: %w", err)
	}
	if version == 5 {
		return Validate(db)
	}
	if version != 1 && version != 2 && version != 3 && version != 4 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if err = validateVersion(db, version); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 1 {
		if _, err = tx.ExecContext(ctx, schemaV2); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, schemaV3); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err = tx.ExecContext(ctx, schemaV4); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, schemaV5); err != nil {
		return err
	}
	return tx.Commit()
}
