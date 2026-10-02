package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/ente/cache"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller"
	publicCtrl "github.com/ente/museum/pkg/controller/public"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/ente/museum/pkg/repo/remotestore"
	storagebonusrepo "github.com/ente/museum/pkg/repo/storagebonus"
	"github.com/ente/museum/pkg/utils/auth"
	"github.com/ente/museum/pkg/utils/config"
	"github.com/ente/museum/pkg/utils/s3config"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestRestrictLegacyUploads(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	// Odd IDs created before the cutoff may still use the legacy routes.
	allowedID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "legacy-allowed@ente.com", CreationTime: 1})
	restrictedID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "legacy-restricted@ente.com", CreationTime: 1})
	h := &FileHandler{Controller: &controller.FileController{
		UserRepo: &repo.UserRepository{DB: db, SecretEncryptionKey: testutil.SecretEncryptionKey()},
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	router.GET("/files/upload-urls", h.RestrictLegacyUploads, ok)
	router.GET("/files/multipart-upload-urls", h.RestrictLegacyUploads, ok)
	const gone = `{"error":"This upload API is no longer supported. Please update your app."}`

	for _, path := range []string{"/files/upload-urls", "/files/multipart-upload-urls"} {
		for _, tt := range []struct {
			userID        int64
			clientPackage string
			wantCode      int
		}{
			{userID: allowedID, clientPackage: "io.ente.photos", wantCode: http.StatusOK},
			{userID: allowedID, clientPackage: "io.ente.locker", wantCode: http.StatusOK},
			{userID: allowedID, clientPackage: "", wantCode: http.StatusOK},
			{userID: restrictedID, clientPackage: "io.ente.photos", wantCode: http.StatusGone},
			{userID: restrictedID, clientPackage: "io.ente.locker", wantCode: http.StatusGone},
			{userID: allowedID, clientPackage: "io.ente.drive", wantCode: http.StatusGone},
			{userID: 99, clientPackage: "io.ente.drive.web", wantCode: http.StatusGone},
		} {
			req := httptest.NewRequest(http.MethodGet, path+"?count=1", nil)
			req.Header.Set("X-Auth-User-ID", strconv.FormatInt(tt.userID, 10))
			req.Header.Set("X-Client-Package", tt.clientPackage)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, tt.wantCode, recorder.Code, "%s %+v", path, tt)
			if tt.wantCode == http.StatusGone {
				require.JSONEq(t, gone, recorder.Body.String())
			}
		}
	}
}

func TestPublicUploadURLUsesCollectionApp(t *testing.T) {
	testutil.WithServerRoot(t)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, config.ConfigureViper("local"))
	viper.Set("s3.b2-eu-cen.key", "test-key")
	viper.Set("s3.b2-eu-cen.secret", "test-secret")
	viper.Set("s3.b2-eu-cen.endpoint", "http://127.0.0.1")
	viper.Set("s3.b2-eu-cen.region", "us-east-1")
	viper.Set("s3.b2-eu-cen.bucket", "test-bucket")
	viper.Set("s3.b2-eu-cen.disable_ssl", true)
	viper.Set("s3.use_path_style_urls", true)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	const ownerID = int64(1)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: ownerID, Email: "public-upload-owner@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, ownerID, 0)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: ownerID, Storage: 100 << 30, ExpiryTime: time.Now().Add(time.Hour).UnixMicro(),
	})
	collectionIDs := make(map[ente.App]int64)
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		var collectionID int64
		require.NoError(t, db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
			VALUES ($1, 'key', 'nonce', 'name', 'folder', '{}', 1, $2) RETURNING collection_id`, ownerID, app).Scan(&collectionID))
		_, err := db.Exec(`INSERT INTO public_collection_tokens(collection_id, access_token, enable_collect) VALUES ($1, $2, TRUE)`,
			collectionID, "collect-"+string(app))
		require.NoError(t, err)
		collectionIDs[app] = collectionID
	}

	s3Config := s3config.NewS3Config()
	users := &repo.UserRepository{DB: db}
	usageRepo := &repo.UsageRepository{DB: db}
	h := &PublicCollectionHandler{
		Controller: &publicCtrl.CollectionLinkController{
			CollectionRepo: &repo.CollectionRepository{DB: db, CollectionLinkRepo: public.NewCollectionLinkRepository(db, "")},
		},
		FileCtrl: &controller.FileController{
			S3Config:          s3Config,
			ObjectCleanupCtrl: controller.NewObjectCleanupController(&repo.ObjectCleanupRepository{DB: db}, &repo.ObjectRepository{DB: db}, s3Config),
			RemoteStoreRepo:   &remotestore.Repository{DB: db},
			UsageCtrl: &controller.UsageController{
				UserRepo: users, UsageRepo: usageRepo, FamilyRepo: &repo.FamilyRepository{DB: db},
				BillingCtrl: &controller.BillingController{UserRepo: users, BillingRepo: &repo.BillingRepository{DB: db}},
				UserCacheCtrl: &usercache.Controller{
					UsageRepo: usageRepo, StoreBonusRepo: &storagebonusrepo.Repository{DB: db}, UserCache: cache.NewUserCache(),
				},
				UploadResultCache: make(map[int64]bool),
			},
		},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/public-collection/upload-url", func(c *gin.Context) {
		collectionID, _ := strconv.ParseInt(c.GetHeader("X-Test-Collection-ID"), 10, 64)
		c.Set(auth.PublicAccessKey, ente.PublicAccessContext{CollectionID: collectionID})
	}, h.GetUploadURLV2)

	for _, tt := range []struct {
		collection    ente.App
		clientPackage string
		size          int64
		wantCode      int
		wantBody      string
		wantTempApp   ente.App
	}{
		{collection: ente.Drive, clientPackage: "io.ente.photos", size: 1 << 30, wantCode: http.StatusOK, wantTempApp: ente.Drive},
		{collection: ente.Drive, clientPackage: "io.ente.drive.web", size: 1 << 30, wantCode: http.StatusOK, wantTempApp: ente.Drive},
		{collection: ente.Drive, clientPackage: "io.ente.photos", size: 11 << 30, wantCode: http.StatusBadRequest,
			wantBody: `"message":"contentLength exceeds max file size 10737418240"`},
		{collection: ente.Photos, clientPackage: "io.ente.photos", size: 1 << 30, wantCode: http.StatusOK, wantTempApp: ente.Photos},
		{collection: ente.Photos, clientPackage: "io.ente.locker", size: 1 << 30, wantCode: http.StatusOK, wantTempApp: ente.Locker},
		{collection: ente.Photos, clientPackage: "io.ente.photos", size: 11 << 30, wantCode: http.StatusBadRequest, wantBody: `{}`},
		{collection: ente.Photos, clientPackage: "io.ente.drive", size: 1 << 30, wantCode: http.StatusOK, wantTempApp: ente.Photos},
		{collection: ente.Photos, clientPackage: "io.ente.drive", size: 6 << 30, wantCode: http.StatusOK, wantTempApp: ente.Photos},
		{collection: ente.Photos, clientPackage: "io.ente.drive", size: 11 << 30, wantCode: http.StatusBadRequest, wantBody: `{}`},
	} {
		body := fmt.Sprintf(`{"contentLength":%d,"contentMD5":"XUFAKrxLKna5cZ2REBfFkg=="}`, tt.size)
		req := httptest.NewRequest(http.MethodPost, "/public-collection/upload-url", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Client-Package", tt.clientPackage)
		req.Header.Set("X-Test-Collection-ID", strconv.FormatInt(collectionIDs[tt.collection], 10))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		require.Equal(t, tt.wantCode, recorder.Code, "%+v: %s", tt, recorder.Body.String())
		if tt.wantBody == `{}` {
			require.JSONEq(t, tt.wantBody, recorder.Body.String(), "%+v", tt)
		} else if tt.wantBody != "" {
			require.Contains(t, recorder.Body.String(), tt.wantBody, "%+v", tt)
		}
		if tt.wantTempApp != "" {
			var upload ente.UploadURL
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &upload))
			var app string
			var userID int64
			require.NoError(t, db.QueryRow(`SELECT app, user_id FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).Scan(&app, &userID))
			require.Equal(t, string(tt.wantTempApp), app, "%+v", tt)
			require.Equal(t, ownerID, userID)
		}
	}
}

func TestPublicUploadApp(t *testing.T) {
	for _, tt := range []struct {
		header, collection, want ente.App
	}{
		{header: ente.Photos, collection: ente.Photos, want: ente.Photos},
		{header: ente.Locker, collection: ente.Photos, want: ente.Locker},
		{header: ente.Photos, collection: ente.Locker, want: ente.Photos},
		{header: ente.Photos, collection: ente.Drive, want: ente.Drive},
		{header: ente.Drive, collection: ente.Drive, want: ente.Drive},
		{header: ente.Drive, collection: ente.Photos, want: ente.Photos},
		{header: ente.Drive, collection: ente.Locker, want: ente.Locker},
		{header: ente.Drive, collection: "", want: ente.Drive},
		{header: ente.Photos, collection: "", want: ente.Photos},
	} {
		require.Equal(t, tt.want, publicUploadApp(tt.header, ente.Collection{App: string(tt.collection)}), "%+v", tt)
	}
}
