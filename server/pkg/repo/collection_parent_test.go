package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func setupCollectionParentTest(t *testing.T) (*CollectionRepository, *sql.DB, int64, int64) {
	t.Helper()
	repository, db, ownerID := setupCollectionMembershipTest(t)
	repository.CollectionLinkRepo = public.NewCollectionLinkRepository(db, "")
	otherID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "collection-parent-other@ente.com", CreationTime: 1})
	return repository, db, ownerID, otherID
}

func insertParentTestCollection(db *sql.DB, ownerID int64, app ente.App, collectionType string, parentID any, key, nonce any) (int64, error) {
	var id int64
	err := db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes,
		updation_time, app, parent_id, parent_encrypted_key, parent_key_nonce)
		VALUES ($1, 'key', 'nonce', 'name', $2, '{}', 1, $3, $4, $5, $6) RETURNING collection_id`,
		ownerID, collectionType, app, parentID, key, nonce).Scan(&id)
	return id, err
}

func requireConstraintViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "error = %v, want violation of %s", err, constraint)
	require.Equal(t, constraint, pqErr.Constraint)
}

func TestCollectionParentSchema(t *testing.T) {
	_, db, _, _ := setupCollectionParentTest(t)
	rows, err := db.Query(`SELECT conname FROM pg_constraint
		WHERE conrelid = 'collections'::regclass AND conname = ANY($1) AND convalidated`,
		pq.Array([]string{"collections_parent_not_self", "collections_parent_drive_folder", "collections_parent_key_present", "fk_collections_parent"}))
	require.NoError(t, err)
	defer rows.Close()
	validated := 0
	for rows.Next() {
		validated++
	}
	require.Equal(t, 4, validated)
	var validIndexes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_index
		WHERE indexrelid IN ('collections_id_owner_app_uidx'::regclass, 'collections_parent_id_idx'::regclass) AND indisvalid`).Scan(&validIndexes))
	require.Equal(t, 2, validIndexes)
}

func TestCollectionParentConstraints(t *testing.T) {
	repository, db, ownerID, otherID := setupCollectionParentTest(t)

	photos, err := repository.Create(ente.Collection{Owner: ente.CollectionUser{ID: ownerID}, EncryptedKey: "key",
		KeyDecryptionNonce: "nonce", Name: "album", Type: "album", UpdationTime: 1, App: string(ente.Photos)})
	require.NoError(t, err)
	var parentColumnsSet bool
	require.NoError(t, db.QueryRow(`SELECT parent_id IS NOT NULL OR parent_encrypted_key IS NOT NULL OR parent_key_nonce IS NOT NULL
		FROM collections WHERE collection_id = $1`, photos.ID).Scan(&parentColumnsSet))
	require.False(t, parentColumnsSet)

	photosFolder, err := insertParentTestCollection(db, ownerID, ente.Photos, "folder", nil, nil, nil)
	require.NoError(t, err)
	root, err := insertParentTestCollection(db, ownerID, ente.Drive, "folder", nil, nil, nil)
	require.NoError(t, err)
	otherRoot, err := insertParentTestCollection(db, otherID, ente.Drive, "folder", nil, nil, nil)
	require.NoError(t, err)
	_, err = insertParentTestCollection(db, ownerID, ente.Drive, "folder", root, "pk", "pn")
	require.NoError(t, err)

	for _, tt := range []struct {
		name       string
		ownerID    int64
		app        ente.App
		typ        string
		parentID   int64
		key, nonce any
		constraint string
	}{
		{"photos child", ownerID, ente.Photos, "folder", photosFolder, "pk", "pn", "collections_parent_drive_folder"},
		{"drive album child", ownerID, ente.Drive, "album", root, "pk", "pn", "collections_parent_drive_folder"},
		{"missing parent key", ownerID, ente.Drive, "folder", root, nil, "pn", "collections_parent_key_present"},
		{"missing parent nonce", ownerID, ente.Drive, "folder", root, "pk", nil, "collections_parent_key_present"},
		{"other owner's parent", ownerID, ente.Drive, "folder", otherRoot, "pk", "pn", "fk_collections_parent"},
		{"photos parent", ownerID, ente.Drive, "folder", photosFolder, "pk", "pn", "fk_collections_parent"},
		{"missing parent", ownerID, ente.Drive, "folder", 1 << 40, "pk", "pn", "fk_collections_parent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := insertParentTestCollection(db, tt.ownerID, tt.app, tt.typ, tt.parentID, tt.key, tt.nonce)
			requireConstraintViolation(t, err, tt.constraint)
		})
	}
	_, err = insertParentTestCollection(db, ownerID, ente.Drive, "folder", nil, "pk", "pn")
	requireConstraintViolation(t, err, "collections_parent_key_present")
	_, err = db.Exec(`UPDATE collections SET parent_id = collection_id, parent_encrypted_key = 'pk', parent_key_nonce = 'pn'
		WHERE collection_id = $1`, root)
	requireConstraintViolation(t, err, "collections_parent_not_self")
}

func TestCollectionParentForeignKeyOnExistingWrites(t *testing.T) {
	_, db, ownerID, _ := setupCollectionParentTest(t)
	root, err := insertParentTestCollection(db, ownerID, ente.Drive, "folder", nil, nil, nil)
	require.NoError(t, err)
	child, err := insertParentTestCollection(db, ownerID, ente.Drive, "folder", root, "pk", "pn")
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE collections SET updation_time = 2, is_deleted = TRUE WHERE collection_id = $1`, root)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM collections WHERE collection_id = $1`, root)
	requireConstraintViolation(t, err, "fk_collections_parent")
	_, err = db.Exec(`UPDATE collections SET owner_id = $1 WHERE collection_id = $2`, ownerID+1, root)
	requireConstraintViolation(t, err, "fk_collections_parent")
	_, err = db.Exec(`DELETE FROM collections WHERE collection_id = ANY($1)`, pq.Array([]int64{root, child}))
	require.NoError(t, err)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	_, err = tx.Exec(`SET LOCAL plan_cache_mode = force_generic_plan`)
	require.NoError(t, err)
	// The referenced-side check that every DELETE (or key change) runs.
	_, err = tx.Exec(`PREPARE parent_ri(bigint, bigint, app) AS SELECT 1 FROM ONLY collections x
		WHERE $1 OPERATOR(pg_catalog.=) parent_id AND $2 OPERATOR(pg_catalog.=) owner_id AND $3 OPERATOR(pg_catalog.=) app
		FOR KEY SHARE OF x`)
	require.NoError(t, err)
	defer tx.Exec(`DEALLOCATE parent_ri`)
	plan := explainPlan(t, tx, `EXPLAIN EXECUTE parent_ri(1, 1, 'drive')`)
	require.Contains(t, plan, "collections_parent_id_idx")
}

func explainPlan(t *testing.T, tx *sql.Tx, query string) string {
	t.Helper()
	rows, err := tx.Query(query)
	require.NoError(t, err)
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan += line + "\n"
	}
	require.NoError(t, rows.Err())
	return plan
}

func TestCollectionParentOwnerReads(t *testing.T) {
	repository, _, ownerID, _ := setupCollectionParentTest(t)
	root, err := repository.Create(ente.Collection{Owner: ente.CollectionUser{ID: ownerID}, EncryptedKey: "key",
		KeyDecryptionNonce: "nonce", Name: "root", Type: "folder", UpdationTime: 1, App: string(ente.Drive)})
	require.NoError(t, err)
	key, nonce := "pk", "pn"
	child, err := repository.Create(ente.Collection{Owner: ente.CollectionUser{ID: ownerID}, EncryptedKey: "key",
		KeyDecryptionNonce: "nonce", Name: "child", Type: "folder", UpdationTime: 1, App: string(ente.Drive),
		ParentID: &root.ID, ParentEncryptedKey: &key, ParentKeyNonce: &nonce})
	require.NoError(t, err)
	unparented, err := repository.Create(ente.Collection{Owner: ente.CollectionUser{ID: ownerID}, EncryptedKey: "key",
		KeyDecryptionNonce: "nonce", Name: "unparented", Type: "folder", UpdationTime: 1, App: string(ente.Drive),
		ParentEncryptedKey: &key, ParentKeyNonce: &nonce})
	require.NoError(t, err)
	require.Nil(t, unparented.ParentEncryptedKey)

	requireParent := func(c ente.Collection, wantParent *int64) {
		t.Helper()
		if wantParent == nil {
			require.Nil(t, c.ParentID)
			require.Nil(t, c.ParentEncryptedKey)
			require.Nil(t, c.ParentKeyNonce)
			return
		}
		require.Equal(t, *wantParent, *c.ParentID)
		require.Equal(t, key, *c.ParentEncryptedKey)
		require.Equal(t, nonce, *c.ParentKeyNonce)
	}
	got, err := repository.Get(child.ID)
	require.NoError(t, err)
	requireParent(got, &root.ID)
	got, err = repository.Get(unparented.ID)
	require.NoError(t, err)
	requireParent(got, nil)

	owned, err := repository.GetCollectionsOwnedByUserV2(ownerID, 0, ente.Drive, nil)
	require.NoError(t, err)
	require.Len(t, owned, 3)
	for _, c := range owned {
		if c.ID == child.ID {
			requireParent(c, &root.ID)
		} else {
			requireParent(c, nil)
		}
	}
}

func TestCollectionParentMigrations(t *testing.T) {
	testutil.WithServerRoot(t)
	mainDB := testutil.RequireTestDB(t)
	var mainName string
	require.NoError(t, mainDB.QueryRow(`SELECT current_database()`).Scan(&mainName))
	name := mainName + "_parent"
	_, err := mainDB.Exec(fmt.Sprintf(`CREATE DATABASE %s`, pq.QuoteIdentifier(name)))
	require.NoError(t, err)
	db, err := sql.Open("postgres", "sslmode=disable dbname="+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = mainDB.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pq.QuoteIdentifier(name)))
	})
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://migrations", name, driver)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(153))

	_, err = db.Exec(`INSERT INTO users(user_id, encrypted_email, email_decryption_nonce, email_hash, creation_time)
		OVERRIDING SYSTEM VALUE VALUES (1, 'e1', 'n1', 'h1', 1), (2, 'e2', 'n2', 'h2', 1)`)
	require.NoError(t, err)
	var ids []int64
	for _, c := range []struct {
		app ente.App
		typ string
	}{{ente.Photos, "album"}, {ente.Photos, "folder"}, {ente.Locker, "folder"}, {ente.Drive, "folder"}, {ente.Drive, "uncategorized"}} {
		var id int64
		require.NoError(t, db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
			VALUES (1, 'key', 'nonce', 'name', $1, '{}', 1, $2) RETURNING collection_id`, c.typ, c.app).Scan(&id))
		_, err = db.Exec(`INSERT INTO collection_shares(collection_id, from_user_id, to_user_id, encrypted_key, updation_time)
			VALUES ($1, 1, 2, 'share-key', 1)`, id)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	snapshot := func() string {
		var s string
		require.NoError(t, db.QueryRow(`SELECT string_agg(concat_ws(',', c.collection_id, c.owner_id, c.app, c.type, c.is_deleted, cs.to_user_id), ';' ORDER BY c.collection_id)
			FROM collections c JOIN collection_shares cs USING (collection_id)`).Scan(&s))
		return s
	}
	before := snapshot()

	require.NoError(t, m.Migrate(156))
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`DROP INDEX collections_parent_id_idx`)
	require.NoError(t, err)
	fkMigration, err := os.ReadFile("migrations/157_collections_parent_fk.up.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(fkMigration))
	require.ErrorContains(t, err, "missing or invalid")
	require.NoError(t, tx.Rollback())

	require.NoError(t, m.Migrate(158))
	require.Equal(t, before, snapshot())
	_, err = insertParentTestCollection(db, 1, ente.Drive, "folder", ids[3], "pk", "pn")
	require.NoError(t, err)
	_, err = insertParentTestCollection(db, 1, ente.Photos, "folder", ids[1], "pk", "pn")
	requireConstraintViolation(t, err, "collections_parent_drive_folder")

	require.NoError(t, m.Migrate(153))
	var parentColumns int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'collections' AND column_name LIKE 'parent%'`).Scan(&parentColumns))
	require.Zero(t, parentColumns)
	require.Equal(t, before, snapshot())

	require.NoError(t, m.Migrate(158))
	version, dirty, err := m.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.EqualValues(t, 158, version)
}
