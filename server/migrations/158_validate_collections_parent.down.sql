ALTER TABLE collections
    DROP CONSTRAINT fk_collections_parent,
    ADD CONSTRAINT fk_collections_parent
        FOREIGN KEY (parent_id, owner_id, app)
        REFERENCES collections (collection_id, owner_id, app) NOT VALID,
    DROP CONSTRAINT collections_parent_key_present,
    ADD CONSTRAINT collections_parent_key_present
        CHECK ((parent_id IS NULL) = (parent_encrypted_key IS NULL)
            AND (parent_id IS NULL) = (parent_key_nonce IS NULL)) NOT VALID,
    DROP CONSTRAINT collections_parent_drive_folder,
    ADD CONSTRAINT collections_parent_drive_folder
        CHECK (parent_id IS NULL OR (app = 'drive' AND type = 'folder')) NOT VALID,
    DROP CONSTRAINT collections_parent_not_self,
    ADD CONSTRAINT collections_parent_not_self
        CHECK (parent_id IS NULL OR parent_id <> collection_id) NOT VALID;
