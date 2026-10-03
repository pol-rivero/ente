package controller

import (
	"database/sql"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/stretchr/testify/require"
)

type copyRows struct {
	pending  []string
	recorded []string
}

// The rows a multipart copy leaves when the process dies after
// CreateMultipartUpload: Drive rows come from the batch reservation, other
// apps' rows from GetUploadURLs.
func insertCrashedCopyRows(t *testing.T, c *FileController, db *sql.DB, fake *fakeMultipartS3) copyRows {
	t.Helper()
	partLength := 256 * mib
	size := 5 * gib
	driveObject := func(key string) ente.TempObject {
		return ente.TempObject{ObjectKey: key, BucketId: "b2-eu-cen", UserID: uploadLimitsUserID, App: ente.Drive,
			Purpose: "file_upload", ContentLength: &size, PartLength: &partLength}
	}
	driveKeys := []string{uploadLimitsKey("drive-copy-pending", 1), uploadLimitsKey("drive-copy-recorded", 1)}
	require.NoError(t, c.ReserveDriveUploads(t.Context(), uploadLimitsUserID,
		[]ente.TempObject{driveObject(driveKeys[0]), driveObject(driveKeys[1])}))
	urls, err := c.GetUploadURLs(t.Context(), uploadLimitsUserID, 2, ente.Photos, true, "client")
	require.NoError(t, err)
	rows := copyRows{pending: []string{driveKeys[0], urls[0].ObjectKey}, recorded: []string{driveKeys[1], urls[1].ObjectKey}}
	for _, url := range urls {
		require.NoError(t, c.ObjectCleanupRepo.SetTempObjectPartLength(t.Context(), url.ObjectKey, partLength, time.Microseconds()))
	}
	for _, key := range rows.pending {
		fake.startUpload(key)
	}
	for _, key := range rows.recorded {
		uploadID := fake.startUpload(key)
		require.NoError(t, c.ObjectCleanupRepo.SetTempObjectUploadID(t.Context(), key, uploadID, time.Microseconds()))
	}
	for _, key := range append(rows.pending, rows.recorded...) {
		setTempObjectExpiry(t, db, key, time.Microseconds()-1)
	}
	return rows
}

func TestCleanupAbortsCrashedCopyUploads(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 100*gib)
	rows := insertCrashedCopyRows(t, c, db, fake)

	require.Equal(t, 4, c.ObjectCleanupCtrl.removeUnreportedObjects())

	for _, key := range append(rows.pending, rows.recorded...) {
		require.False(t, fake.hasUpload(key), key)
	}
	require.Empty(t, tempObjectKeys(t, db))
}

func TestPreviousReleaseCleanupRemovesCrashedCopyRows(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 100*gib)
	rows := insertCrashedCopyRows(t, c, db, fake)

	runPreviousReleaseCleanupPass(t, c.ObjectCleanupCtrl, db)

	for _, key := range rows.recorded {
		require.False(t, fake.hasUpload(key), key)
	}
	require.Empty(t, tempObjectKeys(t, db))
}

func TestReleasedCopyRowsFreeQuotaAndWaitForDelayedCleanup(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	size := 6 * gib
	driveObject := func(key string) []ente.TempObject {
		return []ente.TempObject{{ObjectKey: key, BucketId: "b2-eu-cen", UserID: uploadLimitsUserID, App: ente.Drive,
			Purpose: "file_upload", ContentLength: &size}}
	}
	failed, next := uploadLimitsKey("drive-copy-failed", 1), uploadLimitsKey("drive-copy-next", 1)
	require.NoError(t, c.ReserveDriveUploads(t.Context(), uploadLimitsUserID, driveObject(failed)))
	requireQuotaExceeded(t, c.ReserveDriveUploads(t.Context(), uploadLimitsUserID, driveObject(next)))

	require.NoError(t, c.ObjectCleanupRepo.ReleaseTempObjects(t.Context(), []string{failed}, uploadLimitsUserID,
		time.Microseconds()+gotime.Hour.Microseconds()))

	require.NoError(t, c.ReserveDriveUploads(t.Context(), uploadLimitsUserID, driveObject(next)))
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Contains(t, tempObjectKeys(t, db), failed)
	fake.putObject(failed, size)
	setTempObjectExpiry(t, db, failed, time.Microseconds()-1)
	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasObject(failed))
	require.Equal(t, []string{next}, tempObjectKeys(t, db))
}
