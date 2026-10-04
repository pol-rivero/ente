-- Existing app = 'drive' rows survive the rollback (the constraint is NOT VALID),
-- but any later UPDATE of those rows will fail the restored check.
-- Retries like the up migration.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE files
                DROP CONSTRAINT IF EXISTS files_app_supported,
                ADD CONSTRAINT files_app_supported
                    CHECK (app IN ('photos', 'locker'))
                    NOT VALID;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
