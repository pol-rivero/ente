CREATE INDEX CONCURRENTLY IF NOT EXISTS collections_parent_id_idx
    ON collections (parent_id) WHERE parent_id IS NOT NULL;
