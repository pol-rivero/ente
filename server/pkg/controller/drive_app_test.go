package controller

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/ente/cache"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	storagebonusrepo "github.com/ente/museum/pkg/repo/storagebonus"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
)

func TestDriveUploadURLUsesSharedQuota(t *testing.T) {
	cleanup, _, db := setupObjectCleanupRaceTest(t, "http://127.0.0.1")
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "drive-upload@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, userID, 0)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: userID, Storage: 1 << 30, ExpiryTime: gotime.Now().Add(gotime.Hour).UnixMicro(),
	})
	users := &repo.UserRepository{DB: db}
	usageRepo := &repo.UsageRepository{DB: db}
	controller := &FileController{
		S3Config: cleanup.S3Config, ObjectCleanupCtrl: cleanup,
		UsageCtrl: &UsageController{
			UserRepo: users, UsageRepo: usageRepo,
			BillingCtrl: &BillingController{UserRepo: users, BillingRepo: &repo.BillingRepository{DB: db}},
			UserCacheCtrl: &usercache.Controller{
				UsageRepo: usageRepo, StoreBonusRepo: &storagebonusrepo.Repository{DB: db}, UserCache: cache.NewUserCache(),
			},
			UploadResultCache: make(map[int64]bool),
		},
	}
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	request := ente.UploadURLRequest{ContentLength: 200 << 20, ContentMD5: checksum}

	upload, err := controller.GetUploadURLWithMetadata(t.Context(), userID, request, ente.Drive, "io.ente.drive/1.0")
	if err != nil {
		t.Fatal(err)
	}
	var app string
	if err := db.QueryRow(`SELECT app FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if app != string(ente.Drive) {
		t.Fatalf("temp object app = %q, want %q", app, ente.Drive)
	}

	if _, err := db.Exec(`UPDATE usage SET storage_consumed = $1 WHERE user_id = $2`, int64(1<<30), userID); err != nil {
		t.Fatal(err)
	}
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		_, err := controller.GetUploadURLWithMetadata(t.Context(), userID, request, app, "client")
		if !errors.Is(err, ente.ErrStorageLimitExceeded) {
			t.Fatalf("%s upload over quota error = %v, want %v", app, err, ente.ErrStorageLimitExceeded)
		}
	}
}

func TestEmptyTrashIsScopedToApp(t *testing.T) {
	controller, db := setupTrashDriveTest(t)
	const userID = int64(1)
	fileIDs := make(map[ente.App]int64)
	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Drive} {
		var collectionID, fileID int64
		if err := db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
			VALUES ($1, 'key', 'nonce', 'name', 'album', '{}', 1, $2) RETURNING collection_id`, userID, app).Scan(&collectionID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, userID, app).Scan(&fileID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
			VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, collectionID, fileID, userID); err != nil {
			t.Fatal(err)
		}
		if err := controller.TrashRepo.TrashFiles(t.Context(), userID, ente.TrashRequest{
			TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: collectionID}},
		}); err != nil {
			t.Fatal(err)
		}
		fileIDs[app] = fileID
	}

	for i, app := range []ente.App{ente.Drive, ente.Photos, ente.Locker} {
		if err := controller.EmptyTrash(t.Context(), userID, ente.EmptyTrashRequest{LastUpdatedAt: time.Microseconds()}, app); err != nil {
			t.Fatalf("EmptyTrash(%s) error = %v", app, err)
		}
		emptied := map[ente.App]bool{}
		for _, done := range []ente.App{ente.Drive, ente.Photos, ente.Locker}[:i+1] {
			emptied[done] = true
		}
		for fileApp, fileID := range fileIDs {
			var deleted bool
			if err := db.QueryRow(`SELECT is_deleted FROM trash WHERE file_id = $1`, fileID).Scan(&deleted); err != nil {
				t.Fatal(err)
			}
			if deleted != emptied[fileApp] {
				t.Fatalf("after emptying %s trash, %s trash deleted = %t, want %t", app, fileApp, deleted, emptied[fileApp])
			}
		}
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM queue WHERE queue_name IN ($1, $2, $3) AND is_deleted = FALSE`,
		repo.TrashEmptyQueue, repo.TrashEmptyLockerQueue, repo.TrashEmptyDriveQueue).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending empty-trash queue items = %d, want 0", pending)
	}
}

func setupTrashDriveTest(t *testing.T) (*TrashController, *sql.DB) {
	t.Helper()
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	resetTrashDriveTest(t, db)
	t.Cleanup(func() { resetTrashDriveTest(t, db) })
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "drive-trash@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, 1, 0)

	queueRepo := &repo.QueueRepository{DB: db}
	objectRepo := &repo.ObjectRepository{DB: db, QueueRepo: queueRepo}
	return &TrashController{
		TrashRepo: &repo.TrashRepository{
			DB:           db,
			ObjectRepo:   objectRepo,
			FileRepo:     &repo.FileRepository{DB: db, QueueRepo: queueRepo, ObjectRepo: objectRepo},
			QueueRepo:    queueRepo,
			FileLinkRepo: public.NewFileLinkRepo(db),
		},
		QueueRepo:    queueRepo,
		TaskLockRepo: &repo.TaskLockRepository{DB: db},
		HostName:     "drive-trash-test",
	}, db
}

func resetTrashDriveTest(t *testing.T, db *sql.DB) {
	t.Helper()
	testutil.ResetTables(t, db)
	if _, err := db.Exec(`TRUNCATE TABLE queue RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
}

func TestFileCreateRequiresMatchingDriveCollection(t *testing.T) {
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected storage request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3Server.Close)
	cleanup, fileRepo, db := setupObjectCleanupRaceTest(t, s3Server.URL)
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "drive-create@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, userID, 0)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: userID, Storage: 1 << 30, ExpiryTime: gotime.Now().Add(gotime.Hour).UnixMicro(),
	})
	users := &repo.UserRepository{DB: db}
	usageRepo := &repo.UsageRepository{DB: db}
	controller := &FileController{
		S3Config:       cleanup.S3Config,
		FileRepo:       fileRepo,
		CollectionRepo: &repo.CollectionRepository{DB: db, CollectionLinkRepo: public.NewCollectionLinkRepository(db, "")},
		UsageCtrl: &UsageController{
			UserRepo: users, UsageRepo: usageRepo,
			BillingCtrl: &BillingController{UserRepo: users, BillingRepo: &repo.BillingRepository{DB: db}},
			UserCacheCtrl: &usercache.Controller{
				UsageRepo: usageRepo, StoreBonusRepo: &storagebonusrepo.Repository{DB: db}, UserCache: cache.NewUserCache(),
			},
			UploadResultCache: make(map[int64]bool),
		},
	}
	collectionIDs := make(map[ente.App]int64)
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		collectionID := insertObjectCleanupTestCollection(t, db, userID)
		if _, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, app, collectionID); err != nil {
			t.Fatal(err)
		}
		collectionIDs[app] = collectionID
	}

	for i, tt := range []struct {
		header, collection ente.App
		wantInvalidApp     bool
	}{
		{header: ente.Drive, collection: ente.Drive},
		{header: ente.Drive, collection: ente.Photos, wantInvalidApp: true},
		{header: ente.Photos, collection: ente.Drive, wantInvalidApp: true},
		// Runs after the Drive upload, so it isn't the user's first upload (no email).
		{header: ente.Photos, collection: ente.Photos},
	} {
		fileKey, thumbKey := fmt.Sprintf("1/create-file-%d", i), fmt.Sprintf("1/create-thumb-%d", i)
		if _, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id)
			VALUES ($1, 0, 'b2-eu-cen'), ($2, 0, 'b2-eu-cen')`, fileKey, thumbKey); err != nil {
			t.Fatal(err)
		}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
		file, err := controller.Create(ctx, userID, objectCleanupTestFile(userID, collectionIDs[tt.collection], fileKey, thumbKey), "", tt.header)
		if tt.wantInvalidApp {
			if !errors.Is(err, ente.ErrInvalidApp) {
				t.Fatalf("%s header into %s collection: error = %v, want %v", tt.header, tt.collection, err, ente.ErrInvalidApp)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s header into %s collection: %v", tt.header, tt.collection, err)
		}
		var app string
		if err := db.QueryRow(`SELECT app FROM files WHERE file_id = $1`, file.ID).Scan(&app); err != nil {
			t.Fatal(err)
		}
		if app != string(tt.header) {
			t.Fatalf("file app = %q, want %q", app, tt.header)
		}
	}
}
