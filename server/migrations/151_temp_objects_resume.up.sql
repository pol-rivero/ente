-- A constant default keeps this metadata-only (no table rewrite) on PG >= 11.
ALTER TABLE temp_objects
    ADD COLUMN part_length BIGINT,
    ADD COLUMN resume_parts_completed INT,
    ADD COLUMN reservation_released BOOLEAN NOT NULL DEFAULT FALSE;
