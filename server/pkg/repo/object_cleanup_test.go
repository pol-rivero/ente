package repo

import (
	"database/sql"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/stretchr/testify/require"
)

func TestExpireTempObjectNow(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	otherUserID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "expire-other@ente.com", CreationTime: 1})
	repo := &ObjectCleanupRepository{DB: db}
	future := time.MicrosecondsAfterDays(14)
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id) VALUES
		('1/own', $1, 'b2-eu-cen', $2), ('1/legacy', $1, 'b2-eu-cen', NULL),
		('2/other', $1, 'b2-eu-cen', $3), ('1/expired', 5, 'b2-eu-cen', $2)`, future, userID, otherUserID)
	require.NoError(t, err)

	before := time.Microseconds()
	for _, key := range []string{"1/own", "1/legacy", "2/other", "1/expired", "1/missing"} {
		require.NoError(t, repo.ExpireTempObjectNow(t.Context(), key, userID))
	}
	after := time.Microseconds()

	expiry := func(key string) int64 {
		var expiration int64
		require.NoError(t, db.QueryRow(`SELECT expiration_time FROM temp_objects WHERE object_key = $1`, key).Scan(&expiration))
		return expiration
	}
	require.GreaterOrEqual(t, expiry("1/own"), before)
	require.LessOrEqual(t, expiry("1/own"), after)
	require.Equal(t, future, expiry("1/legacy"))
	require.Equal(t, future, expiry("2/other"))
	require.Equal(t, int64(5), expiry("1/expired"))

	rows, err := db.Query(`SELECT object_key FROM temp_objects WHERE reservation_released`)
	require.NoError(t, err)
	defer rows.Close()
	var released []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		released = append(released, key)
	}
	require.Equal(t, []string{"1/own"}, released)
}

func TestTempObjectsResumeMigration(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	for _, tt := range []struct {
		column, dataType, nullable string
		defaultValue               sql.NullString
	}{
		{column: "part_length", dataType: "bigint", nullable: "YES"},
		{column: "resume_parts_completed", dataType: "integer", nullable: "YES"},
		{column: "reservation_released", dataType: "boolean", nullable: "NO", defaultValue: sql.NullString{String: "false", Valid: true}},
	} {
		var dataType, nullable string
		var defaultValue sql.NullString
		require.NoError(t, db.QueryRow(`SELECT data_type, is_nullable, column_default FROM information_schema.columns
			WHERE table_name = 'temp_objects' AND column_name = $1`, tt.column).Scan(&dataType, &nullable, &defaultValue))
		require.Equal(t, tt.dataType, dataType, tt.column)
		require.Equal(t, tt.nullable, nullable, tt.column)
		require.Equal(t, tt.defaultValue, defaultValue, tt.column)
	}

	// The column list written by binaries that predate migration 151.
	_, err := db.Exec(`
		INSERT INTO temp_objects (
		    object_key, expiration_time, upload_id, is_multipart, bucket_id,
		    user_id, app, purpose, content_length, content_md5, client
		) VALUES ('1/old-binary', 1, 'upload', TRUE, 'b2-eu-cen', $1, 'drive', 'file_upload', 10, NULL, 'client')`, userID)
	require.NoError(t, err)
	var partLength, partsCompleted sql.NullInt64
	var released bool
	require.NoError(t, db.QueryRow(`SELECT part_length, resume_parts_completed, reservation_released
		FROM temp_objects WHERE object_key = '1/old-binary'`).Scan(&partLength, &partsCompleted, &released))
	require.False(t, partLength.Valid)
	require.False(t, partsCompleted.Valid)
	require.False(t, released)

	repo := &ObjectCleanupRepository{DB: db}
	require.NoError(t, repo.AddTempObject(ente.TempObject{ObjectKey: "1/single", BucketId: "b2-eu-cen", UserID: userID}, 1))
	require.NoError(t, db.QueryRow(`SELECT part_length FROM temp_objects WHERE object_key = '1/single'`).Scan(&partLength))
	require.False(t, partLength.Valid)
}

func TestGetOwnerIDAndApp(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	repo := &FileRepository{DB: db}
	// An empty app is stored as NULL, like files created before files.app.
	for stored, want := range map[ente.App]ente.App{"": ente.Photos, ente.Photos: ente.Photos, ente.Locker: ente.Locker, ente.Drive: ente.Drive} {
		var fileID int64
		require.NoError(t, db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, NULLIF($2, '')::app, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, userID, stored).Scan(&fileID))
		ownerID, app, err := repo.GetOwnerIDAndApp(fileID)
		require.NoError(t, err)
		require.Equal(t, userID, ownerID)
		require.Equal(t, want, app, "stored app %q", stored)
	}
}

func TestCleanupUpdatesMatchRowsByObjectKey(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	repo := &ObjectCleanupRepository{DB: db}
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, is_multipart, upload_id) VALUES
		('1/photos-multipart', 1, 'b2-eu-cen', $1, 'photos', TRUE, 'u'),
		('1/drive-pending', 1, 'b2-eu-cen', $1, 'drive', TRUE, NULL),
		('1/drive-single', 1, 'b2-eu-cen', $1, 'drive', FALSE, NULL),
		('1/legacy', 1, 'b2-eu-cen', NULL, NULL, FALSE, NULL)`, userID)
	require.NoError(t, err)
	tx, objects, err := repo.GetAndLockExpiredObjects()
	require.NoError(t, err)
	defer tx.Rollback()
	require.Len(t, objects, 4)
	for _, object := range objects {
		require.NoError(t, repo.SetExpiryForTempObject(tx, object, 42))
	}
	rows, err := tx.Query(`SELECT object_key, expiration_time, reservation_released FROM temp_objects`)
	require.NoError(t, err)
	released := make(map[string]bool)
	for rows.Next() {
		var key string
		var expiry int64
		var isReleased bool
		require.NoError(t, rows.Scan(&key, &expiry, &isReleased))
		require.Equal(t, int64(42), expiry, key)
		released[key] = isReleased
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]bool{
		"1/photos-multipart": false, "1/drive-pending": true, "1/drive-single": true, "1/legacy": false,
	}, released)

	for _, object := range objects {
		require.NoError(t, repo.RemoveTempObject(tx, object))
	}
	var remaining int
	require.NoError(t, tx.QueryRow(`SELECT COUNT(*) FROM temp_objects`).Scan(&remaining))
	require.Zero(t, remaining)
}
