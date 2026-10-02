package repo

import (
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
