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
			combined, _, err := c.UsageCtrl.UsageRepo.GetUsageWithDriveReservations(context.Background(), nil, time.Microseconds(),
				[]int64{uploadLimitsUserID}, uploadLimitsUserID, nil)
			if err != nil {
				mismatches = append(mismatches, -1)
				return
			}
			reads++
			if combined != want {
				mismatches = append(mismatches, combined)
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

func TestPublicDriveUploadReservesOwnerQuota(t *testing.T) {
	c, _, _ := setupQuotaTest(t, 10*gib)
	_, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: 6 * gib, PartLength: gib}, ente.Drive, "client", true)
	require.NoError(t, err)
	_, err = startDriveUpload(c, uploadLimitsUserID, 6*gib)
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

func TestCleanupAbortsUploadsOfRowWithoutUploadID(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
	const key = "1/pending-upload-id"
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, is_multipart, content_length, part_length)
		VALUES ($1, 1, 'b2-eu-cen', 1, 'drive', 'file_upload', TRUE, 10, 10)`, key)
	require.NoError(t, err)
	fake.startUpload(key)
	fake.startUpload(key)
	fake.startUpload(key + "-other")
	fake.set(func(f *fakeMultipartS3) { f.uploadsPageSize = 1 })

	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasUpload(key))
	require.True(t, fake.hasUpload(key+"-other"))
	require.Greater(t, fake.listUploadCalls, 1)
	require.Empty(t, tempObjectKeys(t, db))
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

func TestResumeAndAbortWhileUploadIDPending(t *testing.T) {
	c, db, fake := setupQuotaTest(t, 10*gib)
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
	before := readTempObject(t, db, key)
	require.False(t, before.uploadID.Valid)

	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
	requireAPIError(t, err, http.StatusConflict, ente.UploadBusy)
	requireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, key), http.StatusConflict, ente.UploadBusy)
	require.Equal(t, before, readTempObject(t, db, key))

	release.Do(func() { close(gate) })
	require.NoError(t, <-done)
	require.True(t, readTempObject(t, db, key).uploadID.Valid)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
	require.NoError(t, err)
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
	fake.set(func(f *fakeMultipartS3) {
		f.beforeCreate = func(key string) {
			if err := db.QueryRow(`SELECT upload_id FROM temp_objects WHERE object_key = $1`, key).Scan(&uploadIDAtCreate); err != nil {
				t.Errorf("reading temp object: %v", err)
			}
		}
	})
	upload := requireDriveUpload(t, c, uploadLimitsUserID, 6*gib)
	require.False(t, uploadIDAtCreate.Valid)
	var contentLength, partLength int64
	require.NoError(t, db.QueryRow(`SELECT content_length, part_length FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).
		Scan(&contentLength, &partLength))
	require.Equal(t, 6*gib, contentLength)
	require.Equal(t, gib, partLength)
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
