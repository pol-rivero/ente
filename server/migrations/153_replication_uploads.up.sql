-- In-progress multipart uploads of replication's streaming path, so an attempt
-- can resume after a restart. No foreign keys: object_copies rows are deleted
-- independently, and the replication sweeper cleans up leftovers.
CREATE TABLE IF NOT EXISTS replication_uploads (
    object_key  TEXT   NOT NULL,
    dest_dc     TEXT   NOT NULL,
    upload_id   TEXT   NOT NULL,
    part_size   BIGINT NOT NULL,
    source_etag TEXT   NOT NULL,
    created_at  BIGINT NOT NULL DEFAULT now_utc_micro_seconds(),
    -- The sweeper retries rows whose abort failed after the others.
    abort_failed_at BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (object_key, dest_dc)
);
