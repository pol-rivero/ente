-- Retries like the up migration.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE temp_objects
                DROP COLUMN is_copy,
                DROP COLUMN reservation_released,
                DROP COLUMN resume_parts_completed,
                DROP COLUMN part_length;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
