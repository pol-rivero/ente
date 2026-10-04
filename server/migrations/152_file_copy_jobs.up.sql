-- A new table without foreign keys: referencing users or collections would lock them.
CREATE TABLE IF NOT EXISTS file_copy_jobs (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL,
    request_id        TEXT   NOT NULL,
    src_collection_id BIGINT NOT NULL,
    dst_collection_id BIGINT NOT NULL,
    items             JSONB  NOT NULL,
    status            TEXT   NOT NULL DEFAULT 'pending',
    result            JSONB,
    error             JSONB,
    claims            INT    NOT NULL DEFAULT 0,
    attempts          INT    NOT NULL DEFAULT 0,
    lease_token       TEXT,
    -- Pending jobs: the earliest retry.
    lease_until       BIGINT,
    created_at        BIGINT NOT NULL DEFAULT now_utc_micro_seconds(),
    updated_at        BIGINT NOT NULL DEFAULT now_utc_micro_seconds(),
    CONSTRAINT file_copy_jobs_status_check CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    CONSTRAINT file_copy_jobs_user_request_unique UNIQUE (user_id, request_id)
);

CREATE INDEX IF NOT EXISTS file_copy_jobs_unfinished_idx
    ON file_copy_jobs (id)
    WHERE status IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS file_copy_jobs_unfinished_user_idx
    ON file_copy_jobs (user_id)
    WHERE status IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS file_copy_jobs_finished_idx
    ON file_copy_jobs (updated_at)
    WHERE status IN ('completed', 'failed');

CREATE TRIGGER update_file_copy_jobs_updated_at
    BEFORE UPDATE ON file_copy_jobs
    FOR EACH ROW
EXECUTE PROCEDURE trigger_updated_at_microseconds_column();
