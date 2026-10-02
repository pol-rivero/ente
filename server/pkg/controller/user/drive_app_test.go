package user

import (
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	enteCache "github.com/ente/museum/ente/cache"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/controller/family"
	"github.com/ente/museum/pkg/controller/public"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	authenticatorRepo "github.com/ente/museum/pkg/repo/authenticator"
	castRepo "github.com/ente/museum/pkg/repo/cast"
	"github.com/ente/museum/pkg/repo/passkey"
	publicRepo "github.com/ente/museum/pkg/repo/public"
	storagebonusrepo "github.com/ente/museum/pkg/repo/storagebonus"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

func TestShouldEnforceStorageWarningDeletionLoginBlock(t *testing.T) {
	for app, want := range map[ente.App]bool{
		ente.Photos: true,
		ente.Locker: true,
		ente.Drive:  true,
		ente.Auth:   false,
	} {
		if got := shouldEnforceStorageWarningDeletionLoginBlock(app); got != want {
			t.Errorf("shouldEnforceStorageWarningDeletionLoginBlock(%s) = %t, want %t", app, got, want)
		}
	}
}

func TestResetUserAccessRevokesDriveTokens(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 7101, Email: "drive-reset@example.com", CreationTime: 1})
	userAuthRepo := &repo.UserAuthRepository{DB: db}
	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Drive, ente.Auth} {
		if err := userAuthRepo.AddToken(userID, app, "reset-token-"+string(app), "127.0.0.1", "test"); err != nil {
			t.Fatal(err)
		}
	}
	userRepo := &repo.UserRepository{DB: db, SecretEncryptionKey: testutil.SecretEncryptionKey(), HashingKey: testutil.HashingKey()}
	collectionRepo := &repo.CollectionRepository{DB: db, CollectionLinkRepo: publicRepo.NewCollectionLinkRepository(db, "")}
	controller := &UserController{
		UserAuthRepo: userAuthRepo,
		Cache:        cache.New(time.Minute, time.Minute),
		CollectionCtrl: &collections.CollectionController{
			CollectionRepo: collectionRepo,
			CastRepo:       &castRepo.Repository{DB: db},
			CollectionLinkCtrl: &public.CollectionLinkController{
				CollectionLinkRepo: collectionRepo.CollectionLinkRepo,
				FileLinkRepo:       publicRepo.NewFileLinkRepo(db),
			},
		},
		FamilyController: &family.Controller{UserRepo: userRepo, FamilyRepo: &repo.FamilyRepository{DB: db}},
	}

	if err := controller.ResetUserAccess(t.Context(), userID, logrus.WithField("test", t.Name())); err != nil {
		t.Fatalf("ResetUserAccess() error = %v", err)
	}
	for app, wantActive := range map[ente.App]bool{ente.Photos: false, ente.Locker: false, ente.Drive: false, ente.Auth: true} {
		sessions, err := userAuthRepo.GetActiveSessions(userID, app)
		if err != nil {
			t.Fatal(err)
		}
		if (len(sessions) > 0) != wantActive {
			t.Fatalf("%s active sessions = %d, want active %t", app, len(sessions), wantActive)
		}
	}
}

func TestDriveUserDetailsMatchPhotos(t *testing.T) {
	controller, db, ctx := setupLockerUsageControllerTest(t)
	userID := int64(7201)
	insertLockerUsageTestUser(t, db, userID, nil)
	testutil.InsertUsage(t, db, userID, 300)
	testutil.InsertSubscription(t, db, testutil.SubscriptionFixture{
		UserID: userID, Storage: 1 << 30, ProductID: ente.FreePlanProductID, ExpiryTime: time.Now().Add(time.Hour).UnixMicro(),
	})
	insertLockerUsageTestLockerFile(t, db, userID, 100)
	insertDriveUserTestFiles(t, db, userID, ente.Photos, 1)
	insertDriveUserTestFiles(t, db, userID, ente.Drive, 2)
	addDriveUserTestDeps(controller, db)
	ctx.Request = httptest.NewRequest("GET", "/users/details/v2", nil)

	for _, tt := range []struct {
		app           ente.App
		wantUsage     int64
		wantFileCount int64
	}{
		{ente.Photos, 200, 1},
		{ente.Drive, 200, 2},
		{ente.Locker, 200, 1},
		{ente.Auth, 300, 0},
	} {
		details, err := controller.GetDetailsV2(ctx, userID, true, tt.app)
		if err != nil {
			t.Fatalf("GetDetailsV2(%s) error = %v", tt.app, err)
		}
		if details.Usage != tt.wantUsage || details.FileCount == nil || *details.FileCount != tt.wantFileCount {
			t.Fatalf("GetDetailsV2(%s) usage/fileCount = (%d, %v), want (%d, %d)", tt.app, details.Usage, details.FileCount, tt.wantUsage, tt.wantFileCount)
		}
		if (details.LockerFamilyUsage != nil) != (tt.app == ente.Locker) {
			t.Fatalf("GetDetailsV2(%s) lockerFamilyUsage = %+v", tt.app, details.LockerFamilyUsage)
		}
	}
}

func TestAccountDeletionSummaryIncludesDriveFiles(t *testing.T) {
	controller, db, ctx := setupLockerUsageControllerTest(t)
	userID := int64(7301)
	insertLockerUsageTestUser(t, db, userID, nil)
	testutil.InsertUsage(t, db, userID, 0)
	insertLockerUsageTestLockerFile(t, db, userID, 100)
	insertDriveUserTestFiles(t, db, userID, ente.Photos, 2)
	insertDriveUserTestFiles(t, db, userID, ente.Drive, 3)
	addDriveUserTestDeps(controller, db)

	summary, err := controller.GetAccountDeletionSummary(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PhotosAndVideosCount != 2 || summary.LockerRecordsCount != 1 || summary.DriveFilesCount != 3 {
		t.Fatalf("summary = %+v, want photos 2, locker 1, drive 3", summary)
	}
}

func addDriveUserTestDeps(controller *UserController, db *sql.DB) {
	trashRepo := &repo.TrashRepository{DB: db}
	controller.UserAuthRepo = &repo.UserAuthRepository{DB: db}
	controller.PasskeyRepo = &passkey.Repository{DB: db}
	controller.AuthenticatorRepo = &authenticatorRepo.Repository{DB: db}
	controller.UserCacheController = &usercache.Controller{
		FileRepo:       &repo.FileRepository{DB: db},
		UsageRepo:      controller.UsageRepo,
		TrashRepo:      trashRepo,
		StoreBonusRepo: &storagebonusrepo.Repository{DB: db},
		UserCache:      enteCache.NewUserCache(),
	}
}

func insertDriveUserTestFiles(t *testing.T, db *sql.DB, ownerID int64, app ente.App, count int) {
	t.Helper()
	var collectionID int64
	if err := db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
		VALUES ($1, 'key', 'nonce', 'name', 'album', '{}', 1, $2) RETURNING collection_id`, ownerID, app).Scan(&collectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH inserted AS (
			INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
				metadata_decryption_header, encrypted_metadata, updation_time)
			SELECT $1, $2, 'header', 'header', 'header', 'metadata', 1 FROM generate_series(1, $3)
			RETURNING file_id
		)
		INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		SELECT $4, file_id, 'key', 'nonce', 1, $1, $1 FROM inserted`, ownerID, app, count, collectionID); err != nil {
		t.Fatal(err)
	}
}

func TestAccountDeletionSummaryToleratesDriveCountFailure(t *testing.T) {
	controller, db, ctx := setupLockerUsageControllerTest(t)
	userID := int64(7401)
	insertLockerUsageTestUser(t, db, userID, nil)
	testutil.InsertUsage(t, db, userID, 0)
	if _, err := db.Exec(`UPDATE usage SET photos_file_count = 4, locker_file_count = 5 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	addDriveUserTestDeps(controller, db)
	// Photos and Locker are served from stored counts; only the Drive live count hits this closed DB.
	closedDB, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatal(err)
	}
	closedDB.Close()
	controller.UserCacheController.FileRepo = &repo.FileRepository{DB: closedDB}

	summary, err := controller.GetAccountDeletionSummary(ctx, userID)
	if err != nil {
		t.Fatalf("GetAccountDeletionSummary() error = %v", err)
	}
	if summary.PhotosAndVideosCount != 4 || summary.LockerRecordsCount != 5 || summary.DriveFilesCount != 0 {
		t.Fatalf("summary = %+v, want photos 4, locker 5, drive 0", summary)
	}
}
