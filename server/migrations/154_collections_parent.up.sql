-- Waiting in the lock queue would block every query on the table, so retry
-- with a short lock_timeout instead. SET LOCAL ends with the migration's
-- transaction.
DO $$
BEGIN
    LOOP
        BEGIN
            SET LOCAL lock_timeout = '200ms';
            ALTER TABLE collections
                ADD COLUMN parent_id BIGINT NULL,
                ADD COLUMN parent_encrypted_key TEXT NULL,
                ADD COLUMN parent_key_nonce TEXT NULL,
                ADD CONSTRAINT collections_parent_not_self
                    CHECK (parent_id IS NULL OR parent_id <> collection_id) NOT VALID,
                ADD CONSTRAINT collections_parent_drive_folder
                    CHECK (parent_id IS NULL OR (app = 'drive' AND type = 'folder')) NOT VALID,
                ADD CONSTRAINT collections_parent_key_present
                    CHECK ((parent_id IS NULL) = (parent_encrypted_key IS NULL)
                        AND (parent_id IS NULL) = (parent_key_nonce IS NULL)) NOT VALID;
            RETURN;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.5);
        END;
    END LOOP;
END $$;
