CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS collections_id_owner_app_uidx
    ON collections (collection_id, owner_id, app);
