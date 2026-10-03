ALTER TABLE collections
    ADD COLUMN parent_id BIGINT NULL,
    ADD COLUMN parent_encrypted_key TEXT NULL,
    ADD COLUMN parent_key_nonce TEXT NULL;

ALTER TABLE collections
    ADD CONSTRAINT collections_parent_not_self
        CHECK (parent_id IS NULL OR parent_id <> collection_id) NOT VALID,
    ADD CONSTRAINT collections_parent_drive_folder
        CHECK (parent_id IS NULL OR (app = 'drive' AND type = 'folder')) NOT VALID,
    ADD CONSTRAINT collections_parent_key_present
        CHECK ((parent_id IS NULL) = (parent_encrypted_key IS NULL)
            AND (parent_id IS NULL) = (parent_key_nonce IS NULL)) NOT VALID;
