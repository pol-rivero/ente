-- Waiting in the lock queue would block every query on the table, so retry
-- with a short lock_timeout instead. SET LOCAL ends with the migration's
-- transaction.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE files
                DROP CONSTRAINT IF EXISTS files_app_supported,
                ADD CONSTRAINT files_app_supported
                    CHECK (app IN ('photos', 'locker', 'drive'))
                    NOT VALID;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
