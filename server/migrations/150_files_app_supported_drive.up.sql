ALTER TABLE files
    DROP CONSTRAINT IF EXISTS files_app_supported,
    ADD CONSTRAINT files_app_supported
        CHECK (app IN ('photos', 'locker', 'drive'))
        NOT VALID;
