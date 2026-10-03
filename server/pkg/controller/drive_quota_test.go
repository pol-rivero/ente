package controller

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupQuotaTest(t *testing.T, storage int64) (*FileController, *sql.DB, *fakeMultipartS3) {
	t.Helper()
	fake, s3URL := newFakeMultipartS3(t)
	c, db := newUploadTestController(t, s3URL, storage)
	return c, db, fake
}

func startDriveUpload(c *FileController, userID int64, size int64) (ente.MultipartUploadURLs, error) {
	return c.GetMultipartUploadURLWithMetadata(context.Background(), userID,
		ente.MultipartUploadURLRequest{ContentLength: size, PartLength: gib}, ente.Drive, "client", false)
}

func requireDriveUpload(t *testing.T, c *FileController, userID int64, size int64) ente.MultipartUploadURLs {
	t.Helper()
	upload, err := startDriveUpload(c, userID, size)
	require.NoError(t, err)
	return upload
}

func requireQuotaExceeded(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, ente.ErrStorageLimitExceeded)
}

func waitForAdvisoryLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting))
		return waiting == want
	}, 5*gotime.Second, 10*gotime.Millisecond)
}

func holdQuotaLock(t *testing.T, db *sql.DB, subscriptionAdminID int64) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('quota:' || $1::bigint, 0))`, subscriptionAdminID)
	require.NoError(t, err)
	return tx
}

type tempObjectRow struct {
	uploadID sql.NullString
	expiry   int64
	released bool
}

func readTempObject(t *testing.T, db *sql.DB, key string) tempObjectRow {
	t.Helper()
	var row tempObjectRow
	require.NoError(t, db.QueryRow(`SELECT upload_id, expiration_time, reservation_released FROM temp_objects WHERE object_key = $1`, key).
		Scan(&row.uploadID, &row.expiry, &row.released))
	return row
}

func completeFakeUpload(t *testing.T, fake *fakeMultipartS3, key string, size int64) {
	t.Helper()
	for number := int64(1); size > 0; number++ {
		part := min(size, gib)
		fake.uploadPart(t, key, number, part)
		size -= part
	}
	fake.complete(t, key)
}

func driveThumbnail(t *testing.T, c *FileController, fake *fakeMultipartS3, size int64) string {
	t.Helper()
	upload, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: size, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Drive, "client", false)
	require.NoError(t, err)
	fake.putObject(upload.ObjectKey, size)
	return upload.ObjectKey
}

func createTestFile(t *testing.T, c *FileController, app ente.App, collectionID int64, fileKey, thumbKey string) (ente.File, error) {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
	return c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, collectionID, fileKey, thumbKey), "", app, false)
}

func setUsage(t *testing.T, db *sql.DB, userID int64, usage int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE usage SET storage_consumed = $1 WHERE user_id = $2`, usage, userID)
	require.NoError(t, err)
}

func TestConcurrentDriveStartsShareFreeQuota(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	holder := holdQuotaLock(t, db, uploadLimitsUserID)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
			results <- err
		}()
	}
	close(start)
	waitForAdvisoryLockWaiters(t, db, 2)
	require.NoError(t, holder.Rollback())

	var admitted, rejected int
	for range 2 {
		err := <-results
		if err == nil {
			admitted++
			continue
		}
		requireQuotaExceeded(t, err)
		rejected++
	}
	require.Equal(t, 1, admitted)
	require.Equal(t, 1, rejected)
	keys := tempObjectKeys(t, db)
	require.Len(t, keys, 1)
	require.True(t, readTempObject(t, db, keys[0]).uploadID.Valid)
	require.True(t, fake.hasUpload(keys[0]))
}

func TestDriveReservationReadIsConsistentWithCommits(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 100*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	const files = 40
	const fileSize, thumbSize = int64(1000), int64(10)
	expiry := time.MicrosecondsAfterDays(14)
	for i := range files {
		_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, content_length)
			VALUES ($1, $3, 'b2-eu-cen', 1, 'drive', 'file_upload', $4), ($2, $3, 'b2-eu-cen', 1, 'drive', 'file_upload', $5)`,
			fmt.Sprintf("1/consistent-file-%d", i), fmt.Sprintf("1/consistent-thumb-%d", i), expiry, fileSize, thumbSize)
		require.NoError(t, err)
	}
	want := files * (fileSize + thumbSize)

	stop := make(chan struct{})
	var reads int
	var mismatches []int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			usage, err := c.UsageCtrl.UsageRepo.GetUsageWithDriveReservations(context.Background(), nil, time.Microseconds(),
				[]int64{uploadLimitsUserID}, uploadLimitsUserID, nil)
			if err != nil {
				mismatches = append(mismatches, -1)
				return
			}
			reads++
			if usage.Combined != want {
				mismatches = append(mismatches, usage.Combined)
			}
		}
	})
	for i := range files {
		_, _, err := c.FileRepo.Create(objectCleanupTestFile(uploadLimitsUserID, collectionID,
			fmt.Sprintf("1/consistent-file-%d", i), fmt.Sprintf("1/consistent-thumb-%d", i)),
			fileSize, thumbSize, fileSize+thumbSize, uploadLimitsUserID, ente.Drive)
		require.NoError(t, err)
	}
	close(stop)
	wg.Wait()
	require.Empty(t, mismatches)
	require.Greater(t, reads, files)
	require.Empty(t, tempObjectKeys(t, db))
}

func TestAbortedDriveUploadFreesReservation(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	aborted := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
	requireQuotaExceeded(t, err)

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, aborted.ObjectKey))
	size := 6 * gib
	require.NoError(t, c.UsageCtrl.CanUploadFile(t.Context(), uploadLimitsUserID, &size, ente.Drive))

	fake.set(func(f *fakeMultipartS3) { f.failDeletes[aborted.ObjectKey] = true })
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	row := readTempObject(t, db, aborted.ObjectKey)
	require.Greater(t, row.expiry, time.Microseconds())
	require.True(t, row.released)
	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
}

func TestExpiredReservationStaysFreedAfterFailedCleanup(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	expired := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	photos, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: 6 * gib, PartLength: gib}, ente.Photos, "client", false)
	require.NoError(t, err)
	for _, key := range []string{expired.ObjectKey, photos.ObjectKey} {
		setTempObjectExpiry(t, db, key, 1)
	}
	size := 6 * gib
	require.NoError(t, c.UsageCtrl.CanUploadFile(t.Context(), uploadLimitsUserID, &size, ente.Drive))

	fake.set(func(f *fakeMultipartS3) {
		f.failDeletes[expired.ObjectKey] = true
		f.failDeletes[photos.ObjectKey] = true
	})
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	for key, wantReleased := range map[string]bool{expired.ObjectKey: true, photos.ObjectKey: false} {
		row := readTempObject(t, db, key)
		require.Greater(t, row.expiry, time.Microseconds(), key)
		require.Equal(t, wantReleased, row.released, key)
	}
	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
}

func TestFamilyMemberLimitIncludesReservations(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 12*gib)
	const adminID = uploadLimitsUserID
	memberLimit := 5 * gib
	memberID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "quota-member@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, memberID, 0)
	_, err := db.Exec(`UPDATE users SET family_admin_id = $1 WHERE user_id IN ($1, $2)`, adminID, memberID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO families(id, admin_id, member_id, status, storage_limit) VALUES
		(gen_random_uuid(), $1, $1, 'SELF', NULL), (gen_random_uuid(), $1, $2, 'ACCEPTED', $3)`, adminID, memberID, memberLimit)
	require.NoError(t, err)

	requireDriveUpload(t, c, memberID, 3*gib)
	_, err = startDriveUpload(c, memberID, 3*gib)
	requireQuotaExceeded(t, err)
	requireDriveUpload(t, c, adminID, 8*gib)
	requireDriveUpload(t, c, memberID, gib)
	_, err = startDriveUpload(c, memberID, gib)
	requireQuotaExceeded(t, err)
}

func startPublicDriveUpload(c *FileController, size int64) (ente.MultipartUploadURLs, error) {
	return c.GetMultipartUploadURLWithMetadata(context.Background(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: size, PartLength: gib}, ente.Drive, "client", true)
}

func TestPublicDriveUploadsDontReserveOwnerQuota(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	var publicKeys []string
	for range 3 {
		upload, err := startPublicDriveUpload(c, 6*gib)
		require.NoError(t, err)
		single, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
			ente.UploadURLRequest{ContentLength: 5 * gib, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Drive, "client", true)
		require.NoError(t, err)
		publicKeys = append(publicKeys, upload.ObjectKey, single.ObjectKey)
	}
	for _, key := range publicKeys {
		require.True(t, readTempObject(t, db, key).released, key)
	}
	require.True(t, readTempObject(t, db, publicKeys[0]).uploadID.Valid)

	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	_, err := startPublicDriveUpload(c, 6*gib)
	requireQuotaExceeded(t, err)

	completeFakeUpload(t, fake, publicKeys[0], 6*gib)
	thumbKey := uploadLimitsKey("public-thumb", 10)
	stageUploadLimitsObjects(t, db, thumbKey)
	fake.putObject(thumbKey, 10)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
	_, err = c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, insertUploadLimitsCollection(t, db, ente.Drive),
		publicKeys[0], thumbKey), "", ente.Drive, true)
	requireQuotaExceeded(t, err)
}

func TestReservedDriveUploadCommitsDespiteQuotaUse(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	upload := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	completeFakeUpload(t, fake, upload.ObjectKey, 6*gib)
	thumbKey := driveThumbnail(t, c, fake, 10)
	setUsage(t, db, uploadLimitsUserID, 9*gib)

	file, err := createTestFile(t, c, ente.Drive, collectionID, upload.ObjectKey, thumbKey)
	require.NoError(t, err)
	require.Equal(t, 6*gib, file.File.Size)
	usage, err := c.UsageCtrl.UsageRepo.GetUsage(uploadLimitsUserID)
	require.NoError(t, err)
	require.Equal(t, 15*gib+10, usage)
}

func TestUnreservedDriveCreateIsRechecked(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	expiredUpload := requireDriveUpload(t, c, uploadLimitsUserID, gib)
	completeFakeUpload(t, fake, expiredUpload.ObjectKey, gib)
	setTempObjectExpiry(t, db, expiredUpload.ObjectKey, 1)
	photosFile, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: 5 * gib, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Photos, "client", false)
	require.NoError(t, err)
	fake.putObject(photosFile.ObjectKey, 5*gib)
	thumbKey := driveThumbnail(t, c, fake, 5*gib)
	expiry := tempObjectExpiry(t, db, photosFile.ObjectKey)

	// The committed thumbnail's own reservation must not be counted twice.
	file, err := createTestFile(t, c, ente.Drive, collectionID, photosFile.ObjectKey, thumbKey)
	require.NoError(t, err)
	require.Equal(t, 5*gib, file.File.Size)

	copyKey := uploadLimitsKey("copy", 0)
	_, err = db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose)
		VALUES ($1, $2, 'b2-eu-cen', 1, 'drive', 'file_upload')`, copyKey, expiry)
	require.NoError(t, err)
	fake.putObject(copyKey, gib)
	for _, key := range []string{copyKey, expiredUpload.ObjectKey} {
		_, err = createTestFile(t, c, ente.Drive, collectionID, key, driveThumbnail(t, c, fake, 10))
		requireQuotaExceeded(t, err)
	}
	require.Equal(t, expiry, tempObjectExpiry(t, db, copyKey))
}

func TestReservedDriveUpdateCommitsDespiteQuotaUse(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	original := driveThumbnail(t, c, fake, 100)
	file, err := createTestFile(t, c, ente.Drive, collectionID, original, driveThumbnail(t, c, fake, 10))
	require.NoError(t, err)

	replacement := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	completeFakeUpload(t, fake, replacement.ObjectKey, 6*gib)
	replacementThumb := driveThumbnail(t, c, fake, 10)
	unreservedThumb := driveThumbnail(t, c, fake, 10)
	setUsage(t, db, uploadLimitsUserID, 9*gib)
	file.File.ObjectKey, file.Thumbnail.ObjectKey = replacement.ObjectKey, replacementThumb
	file.File.Size = 0
	file.UpdationTime = time.Microseconds()
	_, err = c.Update(t.Context(), uploadLimitsUserID, file, ente.Drive)
	require.NoError(t, err)

	unreserved := uploadLimitsKey("update-unreserved", 0)
	_, err = db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose)
		VALUES ($1, $2, 'b2-eu-cen', 1, 'drive', 'file_upload')`, unreserved, time.MicrosecondsAfterDays(1))
	require.NoError(t, err)
	fake.putObject(unreserved, 7*gib)
	file.File.ObjectKey, file.Thumbnail.ObjectKey = unreserved, unreservedThumb
	_, err = c.Update(t.Context(), uploadLimitsUserID, file, ente.Drive)
	requireQuotaExceeded(t, err)
}

func insertPendingDriveRow(t *testing.T, db *sql.DB, key string, expiry int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, is_multipart, content_length, part_length)
		VALUES ($1, $2, 'b2-eu-cen', 1, 'drive', 'file_upload', FALSE, 10, 10)`, key, expiry)
	require.NoError(t, err)
}

func TestCleanupAbortsUploadsOfPendingRow(t *testing.T) {
	for name, markers := range map[string]uploadsMarkerMode{"next markers": uploadsMarkersNormal, "no next upload ID": uploadsMarkersOmitUploadID} {
		t.Run(name, func(t *testing.T) {
			c, db, fake := setupQuotaTest(t, 10*gib)
			const key = "1/pending-upload-id"
			insertPendingDriveRow(t, db, key, 1)
			fake.startUpload(key)
			fake.startUpload(key)
			fake.startUpload(key + "-other")
			fake.set(func(f *fakeMultipartS3) { f.uploadsPageSize = 1; f.uploadsMarkers = markers })

			require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
			require.False(t, fake.hasUpload(key))
			require.True(t, fake.hasUpload(key+"-other"))
			require.Greater(t, fake.listUploadCalls, 1)
			require.Empty(t, tempObjectKeys(t, db))
		})
	}
}

func TestCleanupStopsOnNonAdvancingUploadListing(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	const key = "1/pending-upload-id"
	insertPendingDriveRow(t, db, key, 1)
	for range 3 {
		fake.startUpload(key)
	}
	fake.set(func(f *fakeMultipartS3) { f.uploadsPageSize = 1; f.uploadsMarkers = uploadsMarkersEchoRequest })

	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Equal(t, 2, fake.listUploadCalls)
	require.Greater(t, tempObjectExpiry(t, db, key), time.Microseconds())
}

// The cleanup pass of binaries without pending rows (removeUnreportedObject and
// its repository queries before this release): pending rows must look like
// single PUTs to it.
func runPreviousReleaseCleanupPass(t *testing.T, c *ObjectCleanupController, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT object_key, is_multipart, upload_id, bucket_id FROM temp_objects
		WHERE expiration_time <= $1 LIMIT 1000 FOR UPDATE SKIP LOCKED`, time.Microseconds())
	require.NoError(t, err)
	var objects []ente.TempObject
	for rows.Next() {
		var object ente.TempObject
		var uploadID, bucketID sql.NullString
		require.NoError(t, rows.Scan(&object.ObjectKey, &object.IsMultipart, &uploadID, &bucketID))
		if object.IsMultipart {
			object.UploadID = uploadID.String
		}
		object.BucketId = bucketID.String
		objects = append(objects, object)
	}
	require.NoError(t, rows.Err())
	for _, object := range objects {
		exists, err := c.ObjectRepo.DoesObjectExist(tx, object.ObjectKey)
		require.NoError(t, err)
		require.False(t, exists)
		if object.IsMultipart {
			require.NoError(t, c.abortMultipartUpload(object.ObjectKey, object.UploadID, object.BucketId))
			_, err = tx.Exec(`DELETE FROM temp_objects WHERE object_key = $1 AND upload_id = $2`, object.ObjectKey, object.UploadID)
		} else {
			require.NoError(t, c.DeleteObjectFromDataCenter(object.ObjectKey, object.BucketId))
			_, err = tx.Exec(`DELETE FROM temp_objects WHERE object_key = $1`, object.ObjectKey)
		}
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func TestPreviousReleaseCleanupRemovesPendingRow(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	fake.set(func(f *fakeMultipartS3) { f.failCreates = true })
	_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
	require.Error(t, err)
	insertPendingDriveRow(t, db, "1/pending-expired", 1)
	require.Len(t, tempObjectKeys(t, db), 2)

	runPreviousReleaseCleanupPass(t, c.ObjectCleanupCtrl, db)
	require.Empty(t, tempObjectKeys(t, db))
	require.Zero(t, fake.listUploadCalls)
}

func TestDriveStartReleasesRowWhenStorageFails(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	fake.set(func(f *fakeMultipartS3) { f.failCreates = true })
	_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
	require.Error(t, err)
	require.NotErrorIs(t, err, ente.ErrStorageLimitExceeded)

	keys := tempObjectKeys(t, db)
	require.Len(t, keys, 1)
	row := readTempObject(t, db, keys[0])
	require.False(t, row.uploadID.Valid)
	require.True(t, row.released)
	require.LessOrEqual(t, row.expiry, time.Microseconds())
	size := 6 * gib
	require.NoError(t, c.UsageCtrl.CanUploadFile(t.Context(), uploadLimitsUserID, &size, ente.Drive))

	fake.set(func(f *fakeMultipartS3) { f.failCreates = false })
	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Equal(t, 1, fake.listUploadCalls)
	require.Empty(t, tempObjectKeys(t, db))
}

// Returns the pending row's key and a function that lets the start continue
// and returns its result.
func pauseDriveStartAtStorage(t *testing.T, c *FileController, fake *fakeMultipartS3) (string, func() error) {
	t.Helper()
	entered := make(chan string, 1)
	gate := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	fake.set(func(f *fakeMultipartS3) {
		f.beforeCreate = func(key string) {
			entered <- key
			<-gate
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
		done <- err
	}()
	key := <-entered
	return key, func() error {
		release.Do(func() { close(gate) })
		return <-done
	}
}

func TestResumeWhileUploadIDPendingIsBusy(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	key, finish := pauseDriveStartAtStorage(t, c, fake)
	before := readTempObject(t, db, key)
	require.False(t, before.uploadID.Valid)

	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
	requireAPIError(t, err, http.StatusConflict, ente.UploadBusy)
	require.Equal(t, before, readTempObject(t, db, key))

	require.NoError(t, finish())
	require.True(t, readTempObject(t, db, key).uploadID.Valid)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
	require.NoError(t, err)
}

func TestAbortWhileUploadIDPendingCancelsTheStart(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	key, finish := pauseDriveStartAtStorage(t, c, fake)
	requestsBefore := fake.requestCount()

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, key))
	require.Equal(t, requestsBefore, fake.requestCount())
	requireExpiredAndReleased(t, db, key)
	size := 6 * gib
	require.NoError(t, c.UsageCtrl.CanUploadFile(t.Context(), uploadLimitsUserID, &size, ente.Drive))

	requireAPIError(t, finish(), http.StatusGone, ente.UploadGone)
	require.False(t, readTempObject(t, db, key).uploadID.Valid)
	require.False(t, fake.hasUpload(key))

	fake.startUpload(key)
	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasUpload(key))
	require.Empty(t, tempObjectKeys(t, db))
}

func TestAbortReleasesDriveSinglePutUpload(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	single, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: 5 * gib, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Drive, "client", false)
	require.NoError(t, err)
	_, err = startDriveUpload(c, uploadLimitsUserID, 6*gib)
	requireQuotaExceeded(t, err)
	fake.putObject(single.ObjectKey, 5*gib)
	requestsBefore := fake.requestCount()

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, single.ObjectKey))
	require.Equal(t, requestsBefore, fake.requestCount())
	requireExpiredAndReleased(t, db, single.ObjectKey)
	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	requireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, single.ObjectKey), http.StatusGone, ente.UploadGone)

	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasObject(single.ObjectKey))
}

func TestNonDriveQuotaChecksIgnoreReservations(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 10*gib)
	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	single := ente.UploadURLRequest{ContentLength: 5 * gib, ContentMD5: checksum}
	_, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID, single, ente.Drive, "client", false)
	requireQuotaExceeded(t, err)
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		_, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID, single, app, "client", false)
		require.NoError(t, err, app)
	}

	tableLock, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tableLock.Rollback() })
	_, err = tableLock.Exec(`LOCK TABLE temp_objects IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	size := 5 * gib
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*gotime.Second)
		require.NoError(t, c.UsageCtrl.CanUploadFile(ctx, uploadLimitsUserID, &size, app), app)
		cancel()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*gotime.Millisecond)
	defer cancel()
	require.Error(t, c.UsageCtrl.CanUploadFile(ctx, uploadLimitsUserID, &size, ente.Drive))
}

func TestNonDriveUploadStartsKeepTheirOrderAndSkipTheQuotaLock(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 100*gib)
	rowAtCreate := make(map[string]bool)
	var mu sync.Mutex
	fake.set(func(f *fakeMultipartS3) {
		f.beforeCreate = func(key string) {
			var exists bool
			if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM temp_objects WHERE object_key = $1)`, key).Scan(&exists); err != nil {
				t.Errorf("reading temp object: %v", err)
			}
			mu.Lock()
			rowAtCreate[key] = exists
			mu.Unlock()
		}
	})
	request := ente.MultipartUploadURLRequest{ContentLength: 12 * mib, PartLength: 5 * mib}
	holdQuotaLock(t, db, uploadLimitsUserID)
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*gotime.Second)
		upload, err := c.GetMultipartUploadURLWithMetadata(ctx, uploadLimitsUserID, request, app, "client", false)
		cancel()
		require.NoError(t, err, app)
		require.False(t, rowAtCreate[upload.ObjectKey], app)
		require.True(t, readTempObject(t, db, upload.ObjectKey).uploadID.Valid)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*gotime.Millisecond)
	defer cancel()
	_, err := c.GetMultipartUploadURLWithMetadata(ctx, uploadLimitsUserID, request, ente.Drive, "client", false)
	require.Error(t, err)
	require.Len(t, rowAtCreate, 2)
}

func TestDriveStartInsertsRowBeforeStorageCall(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 100*gib)
	var uploadIDAtCreate sql.NullString
	multipartAtCreate := true
	fake.set(func(f *fakeMultipartS3) {
		f.beforeCreate = func(key string) {
			if err := db.QueryRow(`SELECT upload_id, is_multipart FROM temp_objects WHERE object_key = $1`, key).
				Scan(&uploadIDAtCreate, &multipartAtCreate); err != nil {
				t.Errorf("reading temp object: %v", err)
			}
		}
	})
	upload := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	require.False(t, uploadIDAtCreate.Valid)
	require.False(t, multipartAtCreate)
	var contentLength, partLength int64
	var isMultipart bool
	require.NoError(t, db.QueryRow(`SELECT content_length, part_length, is_multipart FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).
		Scan(&contentLength, &partLength, &isMultipart))
	require.Equal(t, 6*gib, contentLength)
	require.Equal(t, gib, partLength)
	require.True(t, isMultipart)
	require.Contains(t, upload.CompleteURL, "uploadId="+readTempObject(t, db, upload.ObjectKey).uploadID.String)
}

func TestPhotosUploadCacheUnaffectedByDrive(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 10*gib)
	setUsage(t, db, uploadLimitsUserID, 20*gib)
	cached := func() (bool, bool) {
		c.UsageCtrl.mu.Lock()
		defer c.UsageCtrl.mu.Unlock()
		value, ok := c.UsageCtrl.UploadResultCache[uploadLimitsUserID]
		return value, ok
	}
	c.UsageCtrl.mu.Lock()
	c.UsageCtrl.UploadResultCache[uploadLimitsUserID] = true
	c.UsageCtrl.mu.Unlock()

	requireQuotaExceeded(t, c.UsageCtrl.CanUploadFile(context.Background(), uploadLimitsUserID, nil, ente.Drive))
	value, ok := cached()
	require.True(t, ok)
	require.True(t, value)

	require.NoError(t, c.UsageCtrl.CanUploadFile(context.Background(), uploadLimitsUserID, nil, ente.Photos))
	require.Eventually(t, func() bool {
		value, _ := cached()
		return !value
	}, 5*gotime.Second, 10*gotime.Millisecond)
	requireQuotaExceeded(t, c.UsageCtrl.CanUploadFile(context.Background(), uploadLimitsUserID, nil, ente.Photos))
}

func setDriveReservationLimits(t *testing.T, slots int, timeout gotime.Duration) {
	t.Helper()
	previousSlots, previousTimeout := driveReservationSlots, driveReservationTimeout
	driveReservationSlots, driveReservationTimeout = make(chan struct{}, slots), timeout
	t.Cleanup(func() { driveReservationSlots, driveReservationTimeout = previousSlots, previousTimeout })
}

func TestOverQuotaDriveStartsDontDeadlockThePool(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 10*gib)
	setUsage(t, db, uploadLimitsUserID, 10*gib)
	pool, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	const poolSize = 4
	pool.SetMaxOpenConns(poolSize)
	c.UsageCtrl = newTestUsageController(pool)
	holder := holdQuotaLock(t, pool, uploadLimitsUserID)

	const starts = 3 * poolSize
	results := make(chan error, starts)
	for range starts {
		go func() {
			_, err := startDriveUpload(c, uploadLimitsUserID, 6*gib)
			results <- err
		}()
	}
	waitForAdvisoryLockWaiters(t, db, poolSize-1)
	require.NoError(t, holder.Rollback())
	deadline := gotime.After(5 * gotime.Second)
	for range starts {
		select {
		case err := <-results:
			requireQuotaExceeded(t, err)
		case <-deadline:
			t.Fatal("Drive upload starts did not finish")
		}
	}
	require.Empty(t, tempObjectKeys(t, db))
}

func TestDriveStartFailsFastWhenTheQuotaCheckIsBusy(t *testing.T) {
	c, db, _ := setupQuotaTest(t, 10*gib)
	setDriveReservationLimits(t, 1, 300*gotime.Millisecond)
	requireBusy := func() {
		t.Helper()
		start := gotime.Now()
		_, err := startDriveUpload(c, uploadLimitsUserID, gib)
		requireAPIError(t, err, http.StatusServiceUnavailable, ente.QuotaCheckBusy)
		require.Less(t, gotime.Since(start), 2*gotime.Second)
	}

	holder := holdQuotaLock(t, db, uploadLimitsUserID)
	requireBusy()
	require.NoError(t, holder.Rollback())

	driveReservationSlots <- struct{}{}
	requireBusy()
	<-driveReservationSlots
	require.Empty(t, tempObjectKeys(t, db))
	requireDriveUpload(t, c, uploadLimitsUserID, gib)
}

func insertDriveReservation(t *testing.T, db *sql.DB, key string, userID int64, contentLength int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, content_length)
		VALUES ($1, $2, 'b2-eu-cen', $3, 'drive', 'file_upload', $4)`, key, time.MicrosecondsAfterDays(1), userID, contentLength)
	require.NoError(t, err)
}

func TestDriveCommitAdmitsReservedKeysAndChecksTheRest(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	upload := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	completeFakeUpload(t, fake, upload.ObjectKey, 6*gib)
	tooLarge := uploadLimitsKey("unreserved-thumb", 5*gib)
	fits := uploadLimitsKey("unreserved-thumb", 3*gib)
	stageUploadLimitsObjects(t, db, tooLarge, fits)
	fake.putObject(tooLarge, 5*gib)
	fake.putObject(fits, 3*gib)

	_, err := createTestFile(t, c, ente.Drive, collectionID, upload.ObjectKey, tooLarge)
	requireQuotaExceeded(t, err)
	require.False(t, readTempObject(t, db, upload.ObjectKey).released)
	_, err = createTestFile(t, c, ente.Drive, collectionID, upload.ObjectKey, fits)
	require.NoError(t, err)
	usage, err := c.UsageCtrl.UsageRepo.GetUsage(uploadLimitsUserID)
	require.NoError(t, err)
	require.Equal(t, 9*gib, usage)
}

func TestDriveCommitChecksKeysWithoutAnOwnSufficientReservation(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	otherUserID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "quota-other@ente.com", CreationTime: 1})
	thumbKey := uploadLimitsKey("reserved-thumb", 10)
	insertDriveReservation(t, db, thumbKey, uploadLimitsUserID, 10)
	fake.putObject(thumbKey, 10)

	undersized := uploadLimitsKey("undersized", 2*gib)
	insertDriveReservation(t, db, undersized, uploadLimitsUserID, gib)
	foreign := uploadLimitsKey("foreign", 2*gib)
	insertDriveReservation(t, db, foreign, otherUserID, 2*gib)
	aborted := requireDriveUpload(t, c, uploadLimitsUserID, 2*gib)
	completeFakeUpload(t, fake, aborted.ObjectKey, 2*gib)
	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, aborted.ObjectKey))
	setTempObjectExpiry(t, db, aborted.ObjectKey, time.MicrosecondsAfterDays(1))
	for _, key := range []string{undersized, foreign} {
		fake.putObject(key, 2*gib)
	}
	setUsage(t, db, uploadLimitsUserID, 8*gib+gib/2)

	for _, key := range []string{undersized, foreign, aborted.ObjectKey} {
		_, err := createTestFile(t, c, ente.Drive, collectionID, key, thumbKey)
		requireQuotaExceeded(t, err)
	}
}

func TestUpdateThumbnailUsesTheStoredDriveApp(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	driveFile, err := createTestFile(t, c, ente.Drive, insertUploadLimitsCollection(t, db, ente.Drive),
		driveThumbnail(t, c, fake, 100), driveThumbnail(t, c, fake, 10))
	require.NoError(t, err)
	staged := func(name string, size int64) string {
		key := uploadLimitsKey(name, size)
		stageUploadLimitsObjects(t, db, key)
		fake.putObject(key, size)
		return key
	}
	photosFile, err := createTestFile(t, c, ente.Photos, insertUploadLimitsCollection(t, db, ente.Photos),
		staged("photos-file", 100), staged("photos-thumb", 10))
	require.NoError(t, err)
	reservedThumb := driveThumbnail(t, c, fake, 5)
	requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	setUsage(t, db, uploadLimitsUserID, 5*gib)
	updateThumbnail := func(fileID int64, key string) error {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPut, "/files/thumbnail", nil)
		ctx.Request.Header.Set("X-Auth-User-ID", fmt.Sprint(uploadLimitsUserID))
		return c.UpdateThumbnail(ctx, fileID, ente.FileAttributes{ObjectKey: key, DecryptionHeader: "header"}, ente.Photos)
	}

	requireQuotaExceeded(t, updateThumbnail(driveFile.ID, staged("drive-thumb-new", 5)))
	require.NoError(t, updateThumbnail(driveFile.ID, reservedThumb))
	require.NoError(t, updateThumbnail(photosFile.ID, staged("photos-thumb-new", 5)))
}
