package repo

import (
	"database/sql"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	"github.com/stretchr/testify/require"
)

func setupDeletedCollectionWriteTest(t *testing.T, app ente.App) (*CollectionRepository, *sql.DB, int64, int64, int64) {
	t.Helper()
	repository, db, ownerID := setupCollectionMembershipTest(t)
	repository.QueueRepo = &QueueRepository{DB: db}
	source := insertObjectTestCollection(t, db, ownerID)
	target := insertObjectTestCollection(t, db, ownerID)
	_, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = ANY(ARRAY[$2, $3]::bigint[])`, app, source, target)
	require.NoError(t, err)
	return repository, db, ownerID, source, target
}

func liveMembership(t *testing.T, db *sql.DB, collectionID, fileID int64) bool {
	t.Helper()
	var live bool
	err := db.QueryRow(`SELECT NOT is_deleted FROM collection_files WHERE collection_id = $1 AND file_id = $2`, collectionID, fileID).Scan(&live)
	if err == sql.ErrNoRows {
		return false
	}
	require.NoError(t, err)
	return live
}

func TestFilesCantBeAddedOrMovedIntoADeletedDriveFolder(t *testing.T) {
	for _, app := range []ente.App{ente.Drive, ente.Photos, ente.Locker} {
		t.Run(string(app), func(t *testing.T) {
			repository, db, ownerID, source, target := setupDeletedCollectionWriteTest(t, app)
			added := insertObjectTestFile(t, db, ownerID)
			moved := insertObjectTestFile(t, db, ownerID)
			linkObjectTestFileToCollection(t, db, source, moved, ownerID)
			require.NoError(t, repository.ScheduleDeletes(t.Context(), []int64{target}, TrashCollectionDriveQueue))

			addErr := repository.AddFiles(t.Context(), target, ownerID, []ente.CollectionFileItem{collectionMembershipTestItem(added)}, ownerID, app)
			moveErr := repository.MoveFiles(t.Context(), target, source, []ente.CollectionFileItem{collectionMembershipTestItem(moved)},
				ownerID, ownerID, app)

			if app == ente.Drive {
				require.ErrorIs(t, addErr, ente.ErrCollectionDeleted)
				require.ErrorIs(t, moveErr, ente.ErrCollectionDeleted)
				require.False(t, liveMembership(t, db, target, added))
				require.False(t, liveMembership(t, db, target, moved))
				require.True(t, liveMembership(t, db, source, moved))
				return
			}
			require.NoError(t, addErr)
			require.NoError(t, moveErr)
			require.True(t, liveMembership(t, db, target, added))
			require.True(t, liveMembership(t, db, target, moved))
			require.False(t, liveMembership(t, db, source, moved))
		})
	}
}

func TestAddFilesIntoADriveFolderWaitsForItsDelete(t *testing.T) {
	repository, db, ownerID, _, target := setupDeletedCollectionWriteTest(t, ente.Drive)
	fileID := insertObjectTestFile(t, db, ownerID)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, repository.ScheduleDeletesTx(t.Context(), tx, []int64{target}, TrashCollectionDriveQueue))

	result := make(chan error, 1)
	go func() {
		result <- repository.AddFiles(t.Context(), target, ownerID, []ente.CollectionFileItem{collectionMembershipTestItem(fileID)}, ownerID, ente.Drive)
	}()
	select {
	case err := <-result:
		t.Fatalf("AddFiles() didn't wait for the delete: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-result, ente.ErrCollectionDeleted)
	require.False(t, liveMembership(t, db, target, fileID))
}
