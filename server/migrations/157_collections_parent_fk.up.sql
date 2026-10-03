-- 155/156 use IF NOT EXISTS, so a rerun after an interrupted CREATE INDEX
-- CONCURRENTLY would silently keep the invalid index it left behind.
DO $$
BEGIN
    IF (SELECT count(*) FROM pg_index
        WHERE indexrelid IN (to_regclass('collections_id_owner_app_uidx'), to_regclass('collections_parent_id_idx'))
          AND indisvalid) <> 2 THEN
        RAISE EXCEPTION 'collections_id_owner_app_uidx or collections_parent_id_idx is missing or invalid (interrupted CREATE INDEX CONCURRENTLY): drop it with DROP INDEX CONCURRENTLY <name> (a plain DROP INDEX takes ACCESS EXCLUSIVE on collections), then migrate force 154 and restart';
    END IF;
END $$;

ALTER TABLE collections
    ADD CONSTRAINT fk_collections_parent
        FOREIGN KEY (parent_id, owner_id, app)
        REFERENCES collections (collection_id, owner_id, app) NOT VALID;
