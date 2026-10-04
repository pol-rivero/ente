-- Retries like the up migration.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE collections
                DROP CONSTRAINT IF EXISTS collections_parent_key_present,
                DROP CONSTRAINT IF EXISTS collections_parent_drive_folder,
                DROP CONSTRAINT IF EXISTS collections_parent_not_self,
                DROP COLUMN IF EXISTS parent_key_nonce,
                DROP COLUMN IF EXISTS parent_encrypted_key,
                DROP COLUMN IF EXISTS parent_id;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
