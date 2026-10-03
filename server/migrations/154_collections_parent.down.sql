ALTER TABLE collections
    DROP CONSTRAINT IF EXISTS collections_parent_key_present,
    DROP CONSTRAINT IF EXISTS collections_parent_drive_folder,
    DROP CONSTRAINT IF EXISTS collections_parent_not_self,
    DROP COLUMN IF EXISTS parent_key_nonce,
    DROP COLUMN IF EXISTS parent_encrypted_key,
    DROP COLUMN IF EXISTS parent_id;
