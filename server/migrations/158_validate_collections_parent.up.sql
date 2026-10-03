-- parent_id has no statistics yet; without them the planner expects most rows
-- to have a parent and validates the FK with a self hash join of the table
-- instead of the partial index.
ANALYZE collections (parent_id);

ALTER TABLE collections
    VALIDATE CONSTRAINT collections_parent_not_self,
    VALIDATE CONSTRAINT collections_parent_drive_folder,
    VALIDATE CONSTRAINT collections_parent_key_present,
    VALIDATE CONSTRAINT fk_collections_parent;
