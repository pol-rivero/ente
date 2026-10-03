package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/ente/cache"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/ente/museum/pkg/repo/remotestore"
	storagebonusrepo "github.com/ente/museum/pkg/repo/storagebonus"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const uploadLimitsUserID = int64(1)

// The fake S3 reports each object's size from its key: "<user>/<name>-<size>".
func setupUploadLimitsTest(t *testing.T, storage int64) (*FileController, *sql.DB, *atomic.Int64) {
	t.Helper()
	var multipartStarts atomic.Int64
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			size, err := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "-")+1:], 10, 64)
			if err != nil {
				t.Errorf("unexpected HEAD %s", r.URL)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			multipartStarts.Add(1)
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>`))
		default:
			t.Errorf("unexpected storage request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(s3Server.Close)
	c, db := newUploadTestController(t, s3Server.URL, storage)
	return c, db, &multipartStarts
}

func newUploadTestController(t *testing.T, s3URL string, storage int64) (*FileController, *sql.DB) {
	t.Helper()
	cleanup, fileRepo, db := setupObjectCleanupRaceTest(t, s3URL)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: uploadLimitsUserID, Email: "upload-limits@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, uploadLimitsUserID, 0)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: uploadLimitsUserID, Storage: storage, ExpiryTime: gotime.Now().Add(gotime.Hour).UnixMicro(),
	})
	users := &repo.UserRepository{DB: db}
	usageRepo := &repo.UsageRepository{DB: db}
	return &FileController{
		S3Config:          cleanup.S3Config,
		ObjectCleanupCtrl: cleanup,
		ObjectCleanupRepo: cleanup.Repo,
		FileRepo:          fileRepo,
		ObjectRepo:        fileRepo.ObjectRepo,
		CollectionRepo:    &repo.CollectionRepository{DB: db, CollectionLinkRepo: public.NewCollectionLinkRepository(db, "")},
		RemoteStoreRepo:   &remotestore.Repository{DB: db},
		UsageCtrl: &UsageController{
			UserRepo: users, UsageRepo: usageRepo, FamilyRepo: &repo.FamilyRepository{DB: db},
			BillingCtrl: &BillingController{UserRepo: users, BillingRepo: &repo.BillingRepository{DB: db}},
			UserCacheCtrl: &usercache.Controller{
				UsageRepo: usageRepo, StoreBonusRepo: &storagebonusrepo.Repository{DB: db}, UserCache: cache.NewUserCache(),
			},
			UploadResultCache: make(map[int64]bool),
		},
	}, db
}

func uploadLimitsKey(name string, size int64) string {
	return fmt.Sprintf("%d/%s-%d", uploadLimitsUserID, name, size)
}

func stageUploadLimitsObjects(t *testing.T, db *sql.DB, keys ...string) int64 {
	t.Helper()
	expiry := time.MicrosecondsAfterDays(14)
	for _, key := range keys {
		_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, purpose)
			VALUES ($1, $2, 'b2-eu-cen', $3, 'file_upload')`, key, expiry, uploadLimitsUserID)
		require.NoError(t, err)
	}
	return expiry
}

func tempObjectExpiry(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var expiry int64
	require.NoError(t, db.QueryRow(`SELECT expiration_time FROM temp_objects WHERE object_key = $1`, key).Scan(&expiry))
	return expiry
}

func setInternalUser(t *testing.T, c *FileController, internal bool) {
	t.Helper()
	if internal {
		require.NoError(t, c.RemoteStoreRepo.InsertOrUpdate(t.Context(), uploadLimitsUserID, string(ente.IsInternalUser), "true"))
	} else {
		require.NoError(t, c.RemoteStoreRepo.RemoveKey(t.Context(), uploadLimitsUserID, string(ente.IsInternalUser)))
	}
}

func insertUploadLimitsCollection(t *testing.T, db *sql.DB, app ente.App) int64 {
	t.Helper()
	collectionID := insertObjectCleanupTestCollection(t, db, uploadLimitsUserID)
	_, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, app, collectionID)
	require.NoError(t, err)
	return collectionID
}

func requireBadRequestMessage(t *testing.T, err error, message string) {
	t.Helper()
	var apiErr *ente.ApiError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadRequest, apiErr.HttpStatusCode)
	require.Contains(t, apiErr.Message, message)
}

func requireLegacyBadRequest(t *testing.T, err error, logMessage string) {
	t.Helper()
	require.ErrorIs(t, err, ente.ErrBadRequest)
	var apiErr *ente.ApiError
	require.False(t, errors.As(err, &apiErr), "unexpected API error %v", err)
	require.Contains(t, err.Error(), logMessage)
}

func TestMultipartUploadSizeLimits(t *testing.T) {
	c, db, _ := setupUploadLimitsTest(t, 100*gib)
	atLimit := ente.MultipartUploadURLRequest{ContentLength: DriveMaxFileSize, PartLength: gib}
	overLimit := ente.MultipartUploadURLRequest{ContentLength: DriveMaxFileSize + 1, PartLength: gib}

	upload, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, atLimit, ente.Drive, "io.ente.drive/1.0", false)
	require.NoError(t, err)
	require.Len(t, upload.PartURLs, int(DriveMaxFileSize/gib))
	var app string
	var contentLength int64
	require.NoError(t, db.QueryRow(`SELECT app, content_length FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).
		Scan(&app, &contentLength))
	require.Equal(t, "drive", app)
	require.Equal(t, DriveMaxFileSize, contentLength)
	_, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, overLimit, ente.Drive, "client", false)
	requireBadRequestMessage(t, err, fmt.Sprintf("contentLength exceeds max file size %d", DriveMaxFileSize))

	photosOverLimit := ente.MultipartUploadURLRequest{ContentLength: MaxFileSize + 1, PartLength: gib}
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		_, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, photosOverLimit, app, "client", false)
		requireLegacyBadRequest(t, err, "contentLength exceeds max file size 10737418240")
	}

	setInternalUser(t, c, true)
	internalRequest := ente.MultipartUploadURLRequest{ContentLength: 15 * gib, PartLength: gib}
	upload, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, internalRequest, ente.Photos, "client", false)
	require.NoError(t, err)
	require.Len(t, upload.PartURLs, 15)
	_, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: InternalUserMaxFileSize + 1, PartLength: gib}, ente.Photos, "client", false)
	requireLegacyBadRequest(t, err, "contentLength exceeds max file size 10737418240")
	_, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, internalRequest, ente.Drive, "client", false)
	requireBadRequestMessage(t, err, fmt.Sprintf("contentLength exceeds max file size %d", DriveMaxFileSize))
}

func TestMultipartUploadQuotaAtStart(t *testing.T) {
	c, db, multipartStarts := setupUploadLimitsTest(t, gib)
	request := ente.MultipartUploadURLRequest{ContentLength: 5 * gib, PartLength: gib}

	_, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, request, ente.Drive, "client", false)
	require.ErrorIs(t, err, ente.ErrStorageLimitExceeded)
	var staged int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM temp_objects`).Scan(&staged))
	require.Zero(t, staged)
	require.Zero(t, multipartStarts.Load(), "multipart upload started despite the quota rejection")

	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		upload, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID, request, app, "client", false)
		require.NoError(t, err, app)
		require.Len(t, upload.PartURLs, 5)
	}
	require.Equal(t, int64(2), multipartStarts.Load())
}

func TestSingleUploadURLLimits(t *testing.T) {
	c, _, _ := setupUploadLimitsTest(t, 100*gib)
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	request := ente.UploadURLRequest{ContentLength: 6 * gib, ContentMD5: checksum}

	_, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID, request, ente.Drive, "client", false)
	requireBadRequestMessage(t, err, "use a multipart upload")
	_, err = c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: ente.MaxMultipartPartSize, ContentMD5: checksum}, ente.Drive, "client", false)
	require.NoError(t, err)

	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		upload, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID, request, app, "client", false)
		require.NoError(t, err, app)
		require.NotEmpty(t, upload.URL)
	}
	_, err = c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: 10*gib + 1, ContentMD5: checksum}, ente.Photos, "client", false)
	requireLegacyBadRequest(t, err, "contentLength exceeds max file size 10737418240")
}

func TestPublicUploadURLLimits(t *testing.T) {
	c, _, _ := setupUploadLimitsTest(t, 100*gib)
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	md5s := func(n int) []string { return strings.Split(strings.Repeat(checksum+",", n-1)+checksum, ",") }

	parts := int(DrivePublicMaxFileSize / gib)
	publicLimit := fmt.Sprintf("contentLength exceeds max file size %d", DrivePublicMaxFileSize)
	_, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: DrivePublicMaxFileSize + 1, PartLength: gib, PartMD5s: md5s(parts + 1)}, ente.Drive, "client", true)
	requireBadRequestMessage(t, err, publicLimit)
	_, err = c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: DrivePublicMaxFileSize + 1, ContentMD5: checksum}, ente.Drive, "client", true)
	requireBadRequestMessage(t, err, publicLimit)
	upload, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: DrivePublicMaxFileSize, PartLength: gib, PartMD5s: md5s(parts)}, ente.Drive, "client", true)
	require.NoError(t, err)
	require.Len(t, upload.PartURLs, parts)

	setInternalUser(t, c, true)
	_, err = c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: 15 * gib, PartLength: gib, PartMD5s: md5s(15)}, ente.Photos, "client", true)
	require.NoError(t, err)
}

func TestCreateExpiresOnlyOversizedUploads(t *testing.T) {
	c, db, _ := setupUploadLimitsTest(t, gib)
	collections := map[ente.App]int64{
		ente.Photos: insertUploadLimitsCollection(t, db, ente.Photos),
		ente.Drive:  insertUploadLimitsCollection(t, db, ente.Drive),
	}
	for i, tt := range []struct {
		app          ente.App
		public       bool
		size         int64
		wantErr      error
		wantExpired  bool
		internalUser bool
	}{
		{app: ente.Photos, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{app: ente.Photos, size: InternalUserMaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true, internalUser: true},
		{app: ente.Drive, size: DriveMaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{app: ente.Drive, size: 15 * gib, wantErr: ente.ErrFileTooLarge, wantExpired: true, internalUser: true},
		{app: ente.Photos, size: 2 * gib, wantErr: ente.ErrStorageLimitExceeded},
		{app: ente.Photos, size: 15 * gib, wantErr: ente.ErrStorageLimitExceeded, internalUser: true},
		{app: ente.Drive, size: DriveMaxFileSize, wantErr: ente.ErrStorageLimitExceeded},
		{app: ente.Drive, public: true, size: DrivePublicMaxFileSize, wantErr: ente.ErrStorageLimitExceeded},
		{app: ente.Drive, public: true, size: DrivePublicMaxFileSize + 1, wantErr: ente.ErrFileTooLarge},
		{app: ente.Photos, public: true, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge},
	} {
		setInternalUser(t, c, tt.internalUser)
		fileKey := uploadLimitsKey(fmt.Sprintf("create-file-%d", i), tt.size)
		thumbKey := uploadLimitsKey(fmt.Sprintf("create-thumb-%d", i), 10)
		expiry := stageUploadLimitsObjects(t, db, fileKey, thumbKey)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)

		before := time.Microseconds()
		_, err := c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, collections[tt.app], fileKey, thumbKey), "", tt.app, tt.public)
		require.ErrorIs(t, err, tt.wantErr, "%+v", tt)
		if tt.wantExpired {
			require.LessOrEqual(t, tempObjectExpiry(t, db, fileKey), time.Microseconds(), "%+v", tt)
			require.GreaterOrEqual(t, tempObjectExpiry(t, db, fileKey), before, "%+v", tt)
		} else {
			require.Equal(t, expiry, tempObjectExpiry(t, db, fileKey), "%+v", tt)
		}
		require.Equal(t, expiry, tempObjectExpiry(t, db, thumbKey), "%+v", tt)
	}
}

func TestUpdateSizeLimitFollowsStoredFileApp(t *testing.T) {
	c, db, _ := setupUploadLimitsTest(t, gib)
	fileIDs := make(map[string]int64)
	for _, stored := range []string{"", "photos", "locker", "drive"} {
		var fileID int64
		require.NoError(t, db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, NULLIF($2, '')::app, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, uploadLimitsUserID, stored).Scan(&fileID))
		_, err := db.Exec(`INSERT INTO object_keys(file_id, o_type, object_key, size, datacenters) VALUES
			($1, 'file', $2, 100, ARRAY['b2-eu-cen']::s3region[]), ($1, 'thumbnail', $3, 10, ARRAY['b2-eu-cen']::s3region[])`,
			fileID, uploadLimitsKey(fmt.Sprintf("existing-file-%d", fileID), 100), uploadLimitsKey(fmt.Sprintf("existing-thumb-%d", fileID), 10))
		require.NoError(t, err)
		fileIDs[stored] = fileID
	}

	for i, tt := range []struct {
		stored       string
		header       ente.App
		size         int64
		internalUser bool
		wantErr      error
		wantExpired  bool
	}{
		{stored: "photos", header: ente.Drive, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "", header: ente.Drive, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "locker", header: ente.Drive, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "photos", header: ente.Drive, size: 15 * gib, internalUser: true, wantErr: ente.ErrStorageLimitExceeded},
		{stored: "", header: ente.Drive, size: 15 * gib, internalUser: true, wantErr: ente.ErrStorageLimitExceeded},
		{stored: "drive", header: ente.Drive, size: DriveMaxFileSize, wantErr: ente.ErrStorageLimitExceeded},
		{stored: "drive", header: ente.Drive, size: DriveMaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "drive", header: ente.Photos, size: 15 * gib, internalUser: true, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "photos", header: ente.Photos, size: MaxFileSize + 1, wantErr: ente.ErrFileTooLarge, wantExpired: true},
		{stored: "locker", header: ente.Photos, size: 15 * gib, internalUser: true, wantErr: ente.ErrStorageLimitExceeded},
		{stored: "photos", header: ente.Locker, size: 15 * gib, internalUser: true, wantErr: ente.ErrFileTooLarge, wantExpired: true},
	} {
		setInternalUser(t, c, tt.internalUser)
		fileKey := uploadLimitsKey(fmt.Sprintf("update-file-%d", i), tt.size)
		thumbKey := uploadLimitsKey(fmt.Sprintf("update-thumb-%d", i), 10)
		expiry := stageUploadLimitsObjects(t, db, fileKey, thumbKey)
		file := objectCleanupTestFile(uploadLimitsUserID, 0, fileKey, thumbKey)
		file.ID = fileIDs[tt.stored]
		file.UpdationTime = time.Microseconds()

		_, err := c.Update(t.Context(), uploadLimitsUserID, file, tt.header)
		require.ErrorIs(t, err, tt.wantErr, "%+v", tt)
		if tt.wantExpired {
			require.LessOrEqual(t, tempObjectExpiry(t, db, fileKey), time.Microseconds(), "%+v", tt)
		} else {
			require.Equal(t, expiry, tempObjectExpiry(t, db, fileKey), "%+v", tt)
		}
		require.Equal(t, expiry, tempObjectExpiry(t, db, thumbKey), "%+v", tt)
	}
}
