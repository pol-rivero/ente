-- A constant default keeps this metadata-only (no table rewrite) on PG >= 11.
-- Waiting in the lock queue would block every query on the table, so retry
-- with a short lock_timeout instead. SET LOCAL ends with the migration's
-- transaction.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE temp_objects
                ADD COLUMN part_length BIGINT,
                ADD COLUMN resume_parts_completed INT,
                ADD COLUMN reservation_released BOOLEAN NOT NULL DEFAULT FALSE,
                ADD COLUMN is_copy BOOLEAN NOT NULL DEFAULT FALSE;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
