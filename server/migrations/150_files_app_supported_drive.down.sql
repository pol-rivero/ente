-- Existing app = 'drive' rows survive the rollback (the constraint is NOT VALID),
-- but any later UPDATE of those rows will fail the restored check.
ALTER TABLE files
    DROP CONSTRAINT IF EXISTS files_app_supported,
    ADD CONSTRAINT files_app_supported
        CHECK (app IN ('photos', 'locker'))
        NOT VALID;
