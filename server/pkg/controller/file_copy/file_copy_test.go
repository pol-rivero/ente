package file_copy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/ente/cache"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/internal/testutil/fakes3"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/controller/access"
	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/ente/museum/pkg/repo/remotestore"
	storagebonusrepo "github.com/ente/museum/pkg/repo/storagebonus"
	"github.com/ente/museum/pkg/utils/config"
	"github.com/ente/museum/pkg/utils/handler"
	"github.com/ente/museum/pkg/utils/s3config"
	"github.com/ente/museum/pkg/utils/s3copy"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

const (
	sharerID = int64(1)
	actorID  = int64(2)
	mib      = int64(1) << 20
	gib      = int64(1) << 30
)

type copyFixture struct {
	t       *testing.T
	ctrl    *FileCopyController
	db      *sql.DB
	fake    *fakes3.Server
	srcID   int64
	dstID   int64
	nextKey int
}

func setupCopyTest(t *testing.T, app ente.App, storage int64) *copyFixture {
	t.Helper()
	testutil.WithServerRoot(t)
	viper.Reset()
	require.NoError(t, config.ConfigureViper("local"))
	fake := fakes3.New(t)
	viper.Set("s3.b2-eu-cen.key", "test-key")
	viper.Set("s3.b2-eu-cen.secret", "test-secret")
	viper.Set("s3.b2-eu-cen.endpoint", fake.URL)
	viper.Set("s3.b2-eu-cen.region", "us-east-1")
	viper.Set("s3.b2-eu-cen.bucket", fakes3.Bucket)
	viper.Set("s3.b2-eu-cen.disable_ssl", true)
	viper.Set("s3.use_path_style_urls", true)
	t.Cleanup(viper.Reset)

	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: sharerID, Email: "sharer@ente.com", CreationTime: 1})
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: actorID, Email: "actor@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, sharerID, 0)
	// Non-zero, so a first Photos copy doesn't send the first-upload email.
	testutil.InsertUsage(t, db, actorID, 1)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: actorID, Storage: storage, ExpiryTime: gotime.Now().Add(gotime.Hour).UnixMicro(),
	})

	s3Config := s3config.NewS3Config()
	objectRepo := &repo.ObjectRepository{DB: db}
	cleanupRepo := &repo.ObjectCleanupRepository{DB: db}
	fileRepo := &repo.FileRepository{
		DB: db, S3Config: s3Config, QueueRepo: &repo.QueueRepository{DB: db}, ObjectRepo: objectRepo,
		ObjectCleanupRepo: cleanupRepo, ObjectCopiesRepo: &repo.ObjectCopiesRepository{DB: db},
	}
	collectionRepo := &repo.CollectionRepository{DB: db, CollectionLinkRepo: public.NewCollectionLinkRepository(db, ""), FileRepo: fileRepo}
	users := &repo.UserRepository{DB: db}
	usageRepo := &repo.UsageRepository{DB: db}
	usageCtrl := &controller.UsageController{
		UserRepo: users, UsageRepo: usageRepo, FamilyRepo: &repo.FamilyRepository{DB: db},
		BillingCtrl: &controller.BillingController{UserRepo: users, BillingRepo: &repo.BillingRepository{DB: db}},
		UserCacheCtrl: &usercache.Controller{
			UsageRepo: usageRepo, StoreBonusRepo: &storagebonusrepo.Repository{DB: db}, UserCache: cache.NewUserCache(),
		},
		UploadResultCache: make(map[int64]bool),
	}
	fileCtrl := &controller.FileController{
		S3Config:          s3Config,
		ObjectCleanupCtrl: controller.NewObjectCleanupController(cleanupRepo, objectRepo, s3Config),
		ObjectCleanupRepo: cleanupRepo,
		FileRepo:          fileRepo,
		ObjectRepo:        objectRepo,
		CollectionRepo:    collectionRepo,
		RemoteStoreRepo:   &remotestore.Repository{DB: db},
		UsageCtrl:         usageCtrl,
	}
	f := &copyFixture{
		t:    t,
		db:   db,
		fake: fake,
		ctrl: &FileCopyController{
			S3Config:       s3Config,
			FileController: fileCtrl,
			FileRepo:       fileRepo,
			ObjectRepo:     objectRepo,
			CollectionCtrl: &collections.CollectionController{
				AccessCtrl:     access.NewAccessController(collectionRepo, fileRepo),
				CollectionRepo: collectionRepo,
				FileRepo:       fileRepo,
			},
		},
	}
	f.srcID = f.insertCollection(sharerID, app)
	f.dstID = f.insertCollection(actorID, app)
	_, err := db.Exec(`INSERT INTO collection_shares(collection_id, from_user_id, to_user_id, encrypted_key, updation_time, role_type, shared_at)
		VALUES ($1, $2, $3, 'share-key', 1, 'VIEWER', 1)`, f.srcID, sharerID, actorID)
	require.NoError(t, err)
	return f
}

func (f *copyFixture) insertCollection(ownerID int64, app ente.App) int64 {
	var id int64
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
		VALUES ($1, 'key', 'nonce', 'name', 'album', '{}', 1, $2) RETURNING collection_id`, ownerID, app).Scan(&id))
	return id
}

// The object sizes recorded in object_keys; storage holds objects of the same
// size unless the test changes them.
func (f *copyFixture) addSourceFile(app ente.App, fileSize, thumbSize int64) int64 {
	f.nextKey++
	var fileID int64
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, $2, 'file-header', 'thumb-header', 'metadata-header', 'metadata', 1) RETURNING file_id`, sharerID, app).Scan(&fileID))
	_, err := f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, f.srcID, fileID, sharerID)
	require.NoError(f.t, err)
	fileKey, thumbKey := f.sourceKeys(fileID)
	_, err = f.db.Exec(`INSERT INTO object_keys(file_id, o_type, object_key, size, datacenters) VALUES
		($1, 'file', $2, $3, ARRAY['b2-eu-cen']::s3region[]), ($1, 'thumbnail', $4, $5, ARRAY['b2-eu-cen']::s3region[])`,
		fileID, fileKey, fileSize, thumbKey, thumbSize)
	require.NoError(f.t, err)
	f.fake.PutObject(fileKey, fileSize)
	f.fake.PutObject(thumbKey, thumbSize)
	return fileID
}

func (f *copyFixture) sourceKeys(fileID int64) (string, string) {
	return fmt.Sprintf("%d/src-file-%d", sharerID, fileID), fmt.Sprintf("%d/src-thumb-%d", sharerID, fileID)
}

func (f *copyFixture) copyWithContext(ctx context.Context, header ente.App, fileIDs ...int64) (*ente.CopyResponse, error) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/files/copy", nil).WithContext(ctx)
	c.Request.Header.Set("X-Auth-User-ID", strconv.FormatInt(actorID, 10))
	c.Request.Header.Set("X-Client-Package", "io.ente."+string(header))
	items := make([]ente.CollectionFileItem, 0, len(fileIDs))
	for _, id := range fileIDs {
		items = append(items, ente.CollectionFileItem{
			ID:                 id,
			EncryptedKey:       base64.StdEncoding.EncodeToString(make([]byte, 48)),
			KeyDecryptionNonce: base64.StdEncoding.EncodeToString(make([]byte, 24)),
		})
	}
	return f.ctrl.CopyFiles(c, ente.CopyFileSyncRequest{SrcCollectionID: f.srcID, DstCollection: f.dstID, CollectionFileItems: items})
}

func (f *copyFixture) copy(header ente.App, fileIDs ...int64) (*ente.CopyResponse, error) {
	return f.copyWithContext(f.t.Context(), header, fileIDs...)
}

type tempRow struct {
	userID        int64
	app           sql.NullString
	purpose       string
	bucket        string
	isMultipart   bool
	uploadID      sql.NullString
	contentLength sql.NullInt64
	partLength    sql.NullInt64
	released      bool
	expiry        int64
}

func (f *copyFixture) tempRow(key string) (tempRow, error) {
	var row tempRow
	err := f.db.QueryRow(`SELECT user_id, app, purpose, bucket_id, is_multipart, upload_id, content_length, part_length,
		reservation_released, expiration_time FROM temp_objects WHERE object_key = $1`, key).
		Scan(&row.userID, &row.app, &row.purpose, &row.bucket, &row.isMultipart, &row.uploadID, &row.contentLength,
			&row.partLength, &row.released, &row.expiry)
	return row, err
}

func (f *copyFixture) tempRowCount() int {
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM temp_objects`).Scan(&count))
	return count
}

func (f *copyFixture) committedSizes(fileID int64) (int64, int64) {
	var fileSize, thumbSize int64
	require.NoError(f.t, f.db.QueryRow(`SELECT
		(SELECT size FROM object_keys WHERE file_id = $1 AND o_type = 'file'),
		(SELECT size FROM object_keys WHERE file_id = $1 AND o_type = 'thumbnail')`, fileID).Scan(&fileSize, &thumbSize))
	return fileSize, thumbSize
}

func (f *copyFixture) actorFileCount() int {
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM files WHERE owner_id = $1`, actorID).Scan(&count))
	return count
}

func (f *copyFixture) setActorUsage(usage int64) {
	_, err := f.db.Exec(`UPDATE usage SET storage_consumed = $1 WHERE user_id = $2`, usage, actorID)
	require.NoError(f.t, err)
}

// Records the destination row of every copy request as the storage sees it.
func (f *copyFixture) recordRowsAt(op fakes3.Op) func() map[string]tempRow {
	var mu sync.Mutex
	rows := map[string]tempRow{}
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op != op {
			return nil
		}
		row, err := f.tempRow(r.Key)
		if err != nil {
			f.t.Errorf("no temp row for %s: %v", r.Key, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if _, seen := rows[r.Key]; !seen {
			rows[r.Key] = row
		}
		return nil
	})
	return func() map[string]tempRow {
		mu.Lock()
		defer mu.Unlock()
		return rows
	}
}

func lowerCopyThresholds(t *testing.T) {
	t.Helper()
	single, part := maxSingleCopySize, minCopyPartSize
	maxSingleCopySize, minCopyPartSize = 10*mib, fakes3.MinPartSize
	t.Cleanup(func() { maxSingleCopySize, minCopyPartSize = single, part })
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

func waitForAdvisoryLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting))
		return waiting == want
	}, 5*gotime.Second, 10*gotime.Millisecond)
}

func requireAPIErrorCode(t *testing.T, err error, status int, code ente.ErrorCode) {
	t.Helper()
	var apiErr *ente.ApiError
	require.ErrorAs(t, err, &apiErr, "%v", err)
	require.Equal(t, status, apiErr.HttpStatusCode)
	require.Equal(t, code, apiErr.Code)
}

func opsOf(requests []fakes3.Request) map[fakes3.Op]int {
	ops := map[fakes3.Op]int{}
	for _, r := range requests {
		ops[r.Op]++
	}
	return ops
}

func response(err error) (int, string) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/files/copy", nil)
	handler.Error(c, err)
	return recorder.Code, recorder.Body.String()
}

func (f *copyFixture) failCopiesOf(sourceKey string, failure fakes3.Failure) {
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpCopy && r.CopySource == fakes3.Bucket+"/"+sourceKey {
			return &failure
		}
		return nil
	})
}

func (f *copyFixture) requireReleasedForDelayedCleanup(wantRows int) {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT reservation_released, expiration_time FROM temp_objects`)
	require.NoError(f.t, err)
	defer rows.Close()
	delayedExpiry := time.Microseconds() + failedDriveCopyCleanupDelay.Microseconds()
	count := 0
	for rows.Next() {
		var released bool
		var expiry int64
		require.NoError(f.t, rows.Scan(&released, &expiry))
		require.True(f.t, released)
		require.InDelta(f.t, delayedExpiry, expiry, float64(gotime.Minute.Microseconds()))
		count++
	}
	require.NoError(f.t, rows.Err())
	require.Equal(f.t, wantRows, count)
}

var copySourceTooBig = fakes3.Failure{Status: http.StatusBadRequest, Code: "InvalidRequest"}

func TestPhotosCopyOfSmallFilesUnchanged(t *testing.T) {
	f := setupCopyTest(t, ente.Photos, 100*gib)
	first := f.addSourceFile(ente.Photos, 3*mib, 1000)
	second := f.addSourceFile(ente.Photos, s3copy.MaxSingleCopySize, 2000)
	rowsAtCopy := f.recordRowsAt(fakes3.OpCopy)
	holdQuotaLock(t, f.db, actorID)

	resp, err := f.copy(ente.Photos, first, second)

	require.NoError(t, err)
	require.Len(t, resp.OldToNewFileIDMap, 2)
	requests := f.fake.Requests()
	require.Equal(t, map[fakes3.Op]int{fakes3.OpCopy: 4, fakes3.OpHead: 4}, opsOf(requests))
	sources := map[string]bool{}
	for _, r := range f.fake.RequestsOf(fakes3.OpCopy) {
		sources[r.CopySource] = true
		require.True(t, strings.HasPrefix(r.Key, fmt.Sprintf("%d/", actorID)))
	}
	for _, id := range []int64{first, second} {
		fileKey, thumbKey := f.sourceKeys(id)
		require.True(t, sources[fakes3.Bucket+"/"+fileKey])
		require.True(t, sources[fakes3.Bucket+"/"+thumbKey])
	}
	rows := rowsAtCopy()
	require.Len(t, rows, 4)
	expiry := time.Microseconds() + 2*controller.PreSignedRequestValidityDuration.Microseconds()
	for key, row := range rows {
		require.Equal(t, actorID, row.userID, key)
		require.Equal(t, "photos", row.app.String)
		require.Equal(t, "file_upload", row.purpose)
		require.Equal(t, "b2-eu-cen", row.bucket)
		require.False(t, row.isMultipart)
		require.False(t, row.uploadID.Valid)
		require.False(t, row.contentLength.Valid)
		require.False(t, row.partLength.Valid)
		require.False(t, row.released)
		require.InDelta(t, expiry, row.expiry, float64(gotime.Minute.Microseconds()))
	}
	require.Zero(t, f.tempRowCount())
	fileSize, thumbSize := f.committedSizes(resp.OldToNewFileIDMap[second])
	require.Equal(t, s3copy.MaxSingleCopySize, fileSize)
	require.Equal(t, int64(2000), thumbSize)
}

func TestPhotosCopyAboveThresholdKeepsCopyObject(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Photos, 100*gib)
	size := 22*mib + 5
	fileID := f.addSourceFile(ente.Photos, size, 1000)
	rowsAtCopy := f.recordRowsAt(fakes3.OpCopy)

	resp, err := f.copy(ente.Photos, fileID)

	require.NoError(t, err)
	require.Equal(t, map[fakes3.Op]int{fakes3.OpCopy: 2, fakes3.OpHead: 2}, opsOf(f.fake.Requests()))
	rows := rowsAtCopy()
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, "photos", row.app.String)
		require.False(t, row.isMultipart)
		require.False(t, row.uploadID.Valid)
		require.False(t, row.partLength.Valid)
		require.False(t, row.contentLength.Valid)
	}
	fileSize, _ := f.committedSizes(resp.OldToNewFileIDMap[fileID])
	require.Equal(t, size, fileSize)
	require.Zero(t, f.tempRowCount())
}

func TestPhotosCopyFallsBackToMultipartWhenCopyObjectFails(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Photos, 100*gib)
	size := 22*mib + 5
	fileID := f.addSourceFile(ente.Photos, size, 1000)
	fileKey, _ := f.sourceKeys(fileID)
	var mu sync.Mutex
	var rowAtPartCopy *tempRow
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpCopy && r.CopySource == fakes3.Bucket+"/"+fileKey {
			return &copySourceTooBig
		}
		if r.Op == fakes3.OpPartCopy {
			row, err := f.tempRow(r.Key)
			if err != nil {
				t.Errorf("no temp row for %s: %v", r.Key, err)
			}
			mu.Lock()
			rowAtPartCopy = &row
			mu.Unlock()
		}
		return nil
	})

	resp, err := f.copy(ente.Photos, fileID)

	require.NoError(t, err)
	ops := opsOf(f.fake.Requests())
	require.Equal(t, 2, ops[fakes3.OpCopy])
	require.Equal(t, 1, ops[fakes3.OpCreate])
	require.Equal(t, 5, ops[fakes3.OpPartCopy])
	require.Equal(t, 1, ops[fakes3.OpComplete])
	require.Zero(t, ops[fakes3.OpAbort])
	require.NotNil(t, rowAtPartCopy)
	require.Equal(t, "photos", rowAtPartCopy.app.String)
	require.True(t, rowAtPartCopy.isMultipart)
	require.Equal(t, "upload-1", rowAtPartCopy.uploadID.String)
	require.Equal(t, fakes3.MinPartSize, rowAtPartCopy.partLength.Int64)
	require.False(t, rowAtPartCopy.contentLength.Valid)
	fileSize, _ := f.committedSizes(resp.OldToNewFileIDMap[fileID])
	require.Equal(t, size, fileSize)
	require.Zero(t, f.tempRowCount())
}

func TestPhotosCopyObjectErrorKeptWithoutFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
	}{
		{"below threshold", 3 * mib},
		{"above the file size limit", controller.MaxFileSize + 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lowerCopyThresholds(t)
			f := setupCopyTest(t, ente.Photos, 100*gib)
			fileID := f.addSourceFile(ente.Photos, tt.size, 1000)
			fileKey, _ := f.sourceKeys(fileID)
			f.failCopiesOf(fileKey, copySourceTooBig)

			_, err := f.copy(ente.Photos, fileID)

			require.ErrorContains(t, err, "failed to copy (file) from "+fakes3.Bucket+"/"+fileKey)
			require.ErrorContains(t, err, "InvalidRequest")
			require.Equal(t, map[fakes3.Op]int{fakes3.OpCopy: 2}, opsOf(f.fake.Requests()))
			status, body := response(err)
			require.Equal(t, http.StatusInternalServerError, status)
			require.Equal(t, "{}", body)
			var withPartLength int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM temp_objects WHERE part_length IS NOT NULL`).Scan(&withPartLength))
			require.Zero(t, withPartLength)
		})
	}
}

func TestCopyDestinationGoneIsAnInternalError(t *testing.T) {
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		t.Run(string(app), func(t *testing.T) {
			lowerCopyThresholds(t)
			f := setupCopyTest(t, app, 100*gib)
			fileID := f.addSourceFile(app, 22*mib, 1000)
			fileKey, _ := f.sourceKeys(fileID)
			f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
				if r.Op == fakes3.OpCreate || (r.Op == fakes3.OpCopy && r.CopySource == fakes3.Bucket+"/"+fileKey) {
					if _, err := f.db.Exec(`UPDATE temp_objects SET reservation_released = TRUE WHERE object_key = $1`, r.Key); err != nil {
						t.Error(err)
					}
				}
				if r.Op == fakes3.OpCopy && r.CopySource == fakes3.Bucket+"/"+fileKey {
					return &copySourceTooBig
				}
				return nil
			})

			_, err := f.copy(app, fileID)

			require.Error(t, err)
			status, _ := response(err)
			require.Equal(t, http.StatusInternalServerError, status)
			require.Empty(t, f.fake.RequestsOf(fakes3.OpPartCopy))
			require.Zero(t, f.actorFileCount())
		})
	}
}

func TestDriveCopyCommitsSizesAndIsAdmitted(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, gib)
	fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
	var once sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpCopy {
			once.Do(func() { f.setActorUsage(gib) })
		}
		return nil
	})

	resp, err := f.copy(ente.Drive, fileID)

	require.NoError(t, err)
	fileSize, thumbSize := f.committedSizes(resp.OldToNewFileIDMap[fileID])
	require.Equal(t, 600*mib, fileSize)
	require.Equal(t, int64(1000), thumbSize)
	require.Zero(t, f.tempRowCount())
	var usage int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_consumed FROM usage WHERE user_id = $1`, actorID).Scan(&usage))
	require.Equal(t, gib+600*mib+1000, usage)
}

func TestDriveCopyReservesRowsBeforeCopying(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
	rowsAtCopy := f.recordRowsAt(fakes3.OpCopy)

	_, err := f.copy(ente.Drive, fileID)

	require.NoError(t, err)
	rows := rowsAtCopy()
	require.Len(t, rows, 2)
	sizes := map[int64]bool{}
	for key, row := range rows {
		require.Equal(t, actorID, row.userID, key)
		require.Equal(t, "drive", row.app.String)
		require.Equal(t, "file_upload", row.purpose)
		require.Equal(t, "b2-eu-cen", row.bucket)
		require.False(t, row.isMultipart)
		require.False(t, row.partLength.Valid)
		require.False(t, row.released)
		sizes[row.contentLength.Int64] = true
	}
	require.Equal(t, map[int64]bool{600 * mib: true, 1000: true}, sizes)
}

func TestDriveCopyRejectsCopyOfUnexpectedSize(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
	}{
		{"single copy", 3 * mib},
		{"multipart copy", 22 * mib},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lowerCopyThresholds(t)
			f := setupCopyTest(t, ente.Drive, 100*gib)
			fileID := f.addSourceFile(ente.Drive, tt.size, 1000)
			fileKey, _ := f.sourceKeys(fileID)
			f.fake.PutObject(fileKey, tt.size+1)

			_, err := f.copy(ente.Drive, fileID)

			require.ErrorIs(t, err, ente.ErrBadRequest)
			status, _ := response(err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Zero(t, f.actorFileCount())
			f.requireReleasedForDelayedCleanup(2)
		})
	}
}

func TestDriveCopyRejectsOversizedFileBeforeStorage(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, controller.DriveMaxFileSize+1, 1000)

	_, err := f.copy(ente.Drive, fileID)

	require.ErrorIs(t, err, ente.ErrFileTooLarge)
	require.Empty(t, f.fake.Requests())
	require.Zero(t, f.tempRowCount())
}

func TestDriveCopyRejectsBatchOverQuotaBeforeStorage(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, gib)
	first := f.addSourceFile(ente.Drive, 600*mib, 1000)
	second := f.addSourceFile(ente.Drive, 600*mib, 1000)

	_, err := f.copy(ente.Drive, first, second)

	require.ErrorIs(t, err, ente.ErrStorageLimitExceeded)
	require.Empty(t, f.fake.Requests())
	require.Zero(t, f.tempRowCount())
	_, err = f.copy(ente.Drive, first)
	require.NoError(t, err)
}

func TestConcurrentDriveCopiesShareFreeQuota(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, gib)
	fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
	holder := holdQuotaLock(t, f.db, actorID)

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.copy(ente.Drive, fileID)
			results <- err
		}()
	}
	waitForAdvisoryLockWaiters(t, f.db, 2)
	require.NoError(t, holder.Rollback())

	var admitted, rejected int
	for range 2 {
		err := <-results
		if err == nil {
			admitted++
		} else {
			require.ErrorIs(t, err, ente.ErrStorageLimitExceeded)
			rejected++
		}
	}
	require.Equal(t, 1, admitted)
	require.Equal(t, 1, rejected)
	require.Equal(t, 1, f.actorFileCount())
}

func TestDriveMultipartCopyRecordsUploadIDBeforeParts(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	size := 22*mib + 5
	fileID := f.addSourceFile(ente.Drive, size, 1000)
	rowsAtPartCopy := f.recordRowsAt(fakes3.OpPartCopy)

	resp, err := f.copy(ente.Drive, fileID)

	require.NoError(t, err)
	rows := rowsAtPartCopy()
	require.Len(t, rows, 1)
	for _, row := range rows {
		require.Equal(t, "drive", row.app.String)
		require.True(t, row.isMultipart)
		require.Equal(t, "upload-1", row.uploadID.String)
		require.Equal(t, fakes3.MinPartSize, row.partLength.Int64)
		require.Equal(t, size, row.contentLength.Int64)
		require.False(t, row.released)
	}
	parts := f.fake.RequestsOf(fakes3.OpPartCopy)
	require.Len(t, parts, 5)
	fileSize, _ := f.committedSizes(resp.OldToNewFileIDMap[fileID])
	require.Equal(t, size, fileSize)
	require.Zero(t, f.tempRowCount())
}

func TestDriveCopyFailureLeavesRecordedRowForCleanup(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 12*mib, 1000)
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		switch r.Op {
		case fakes3.OpPartCopy:
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
		case fakes3.OpAbort:
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
		}
		return nil
	})

	_, err := f.copy(ente.Drive, fileID)

	require.Error(t, err)
	status, _ := response(err)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Zero(t, f.actorFileCount())
	uploads := f.fake.Uploads()
	require.Len(t, uploads, 1)
	for uploadID, upload := range uploads {
		row, err := f.tempRow(upload.Key)
		require.NoError(t, err)
		require.True(t, row.isMultipart)
		require.Equal(t, uploadID, row.uploadID.String)
	}
	f.requireReleasedForDelayedCleanup(2)
}

func TestDriveCopyStopsWhenCancelled(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 100*mib, 1000)
	ctx, cancel := context.WithCancel(t.Context())
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy {
			cancel()
		}
		return nil
	})

	_, err := f.copyWithContext(ctx, ente.Drive, fileID)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, len(f.fake.RequestsOf(fakes3.OpPartCopy)), 20)
	require.Empty(t, f.fake.RequestsOf(fakes3.OpComplete))
	require.Len(t, f.fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, f.fake.Uploads())
	require.Zero(t, f.actorFileCount())
	f.requireReleasedForDelayedCleanup(2)
}

func TestFailedDriveCopyReleasesQuotaAtOnce(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, gib)
	fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
	_, thumbKey := f.sourceKeys(fileID)
	f.failCopiesOf(thumbKey, fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"})

	_, err := f.copy(ente.Drive, fileID)

	require.ErrorContains(t, err, "AccessDenied")
	f.requireReleasedForDelayedCleanup(2)
	f.fake.SetHook(nil)
	_, err = f.copy(ente.Drive, fileID)
	require.NoError(t, err)
	require.Equal(t, 1, f.actorFileCount())
}

func TestDriveBatchKeepsCommittedFilesWhenAnotherFails(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	committed := f.addSourceFile(ente.Drive, 3*mib, 1000)
	failed := f.addSourceFile(ente.Drive, 3*mib, 1000)
	failedKey, _ := f.sourceKeys(failed)
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op != fakes3.OpCopy || r.CopySource != fakes3.Bucket+"/"+failedKey {
			return nil
		}
		deadline := gotime.Now().Add(5 * gotime.Second)
		for {
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM files WHERE owner_id = $1`, actorID).Scan(&count); err != nil {
				t.Error(err)
				break
			}
			if count == 1 || gotime.Now().After(deadline) {
				break
			}
			gotime.Sleep(10 * gotime.Millisecond)
		}
		return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
	})

	_, err := f.copy(ente.Drive, committed, failed)

	require.ErrorContains(t, err, "AccessDenied")
	require.Equal(t, 1, f.actorFileCount())
	var copiedFrom int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_keys k JOIN files f ON f.file_id = k.file_id
		WHERE f.owner_id = $1 AND k.size = $2 AND k.o_type = 'file'`, actorID, 3*mib).Scan(&copiedFrom))
	require.Equal(t, 1, copiedFrom)
	f.requireReleasedForDelayedCleanup(2)
}

func TestThumbnailCopyFailureAbortsFileMultipartCopy(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 100*mib, 1000)
	_, thumbKey := f.sourceKeys(fileID)
	partStarted := make(chan struct{})
	var once sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		switch {
		case r.Op == fakes3.OpPartCopy:
			once.Do(func() { close(partStarted) })
			select {
			case <-r.Done:
			case <-gotime.After(5 * gotime.Second):
				t.Error("part copy wasn't cancelled")
			}
		case r.Op == fakes3.OpCopy && r.CopySource == fakes3.Bucket+"/"+thumbKey:
			select {
			case <-partStarted:
			case <-gotime.After(5 * gotime.Second):
				t.Error("no part copy started")
			}
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
		}
		return nil
	})

	_, err := f.copy(ente.Drive, fileID)

	require.ErrorContains(t, err, "AccessDenied")
	require.Len(t, f.fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, f.fake.RequestsOf(fakes3.OpComplete))
	require.Empty(t, f.fake.Uploads())
	require.Zero(t, f.actorFileCount())
	f.requireReleasedForDelayedCleanup(2)
}

func TestCopyRejectsAppMismatchInvolvingDrive(t *testing.T) {
	for _, tt := range []struct {
		collections, header ente.App
	}{
		{ente.Drive, ente.Photos},
		{ente.Photos, ente.Drive},
	} {
		t.Run(fmt.Sprintf("%s collections with %s header", tt.collections, tt.header), func(t *testing.T) {
			f := setupCopyTest(t, tt.collections, 100*gib)
			fileID := f.addSourceFile(tt.collections, mib, 1000)

			_, err := f.copy(tt.header, fileID)

			requireAPIErrorCode(t, err, http.StatusBadRequest, ente.CrossAppFile)
			require.Empty(t, f.fake.Requests())
			require.Zero(t, f.tempRowCount())
		})
	}
}

func TestCopyBetweenDriveAndOtherAppCollectionsIsRejected(t *testing.T) {
	for _, tt := range []struct {
		src, dst ente.App
	}{
		{ente.Drive, ente.Photos},
		{ente.Photos, ente.Drive},
		{ente.Locker, ente.Drive},
	} {
		t.Run(fmt.Sprintf("%s to %s", tt.src, tt.dst), func(t *testing.T) {
			f := setupCopyTest(t, tt.src, 100*gib)
			fileID := f.addSourceFile(tt.src, mib, 1000)
			_, err := f.db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, tt.dst, f.dstID)
			require.NoError(t, err)

			for _, header := range []ente.App{tt.src, tt.dst} {
				_, err = f.copy(header, fileID)
				requireAPIErrorCode(t, err, http.StatusBadRequest, ente.CrossAppFile)
			}
			require.Empty(t, f.fake.Requests())
			require.Zero(t, f.tempRowCount())
		})
	}
}

func TestCopyBetweenPhotosAndLockerCollectionsKeepsInvalidApp(t *testing.T) {
	f := setupCopyTest(t, ente.Photos, 100*gib)
	fileID := f.addSourceFile(ente.Photos, mib, 1000)
	_, err := f.db.Exec(`UPDATE collections SET app = 'locker' WHERE collection_id = $1`, f.dstID)
	require.NoError(t, err)

	_, err = f.copy(ente.Photos, fileID)

	require.True(t, errors.Is(err, ente.ErrInvalidApp), "%v", err)
	require.Empty(t, f.fake.Requests())
}
