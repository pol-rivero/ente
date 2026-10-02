package controller

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/config"
	"github.com/ente/museum/pkg/utils/s3config"
	"github.com/spf13/viper"
)

func TestFileRegistrationStoresApp(t *testing.T) {
	for _, tt := range []struct {
		app                    ente.App
		wantPhotos, wantLocker int64
		wantVersion            int64
	}{
		{app: ente.Photos, wantPhotos: 3, wantLocker: 3, wantVersion: 1},
		{app: ente.Locker, wantPhotos: 2, wantLocker: 4, wantVersion: 1},
		// Drive files share the storage quota but aren't counted.
		{app: ente.Drive, wantPhotos: 2, wantLocker: 3, wantVersion: 0},
	} {
		t.Run(string(tt.app), func(t *testing.T) {
			_, fileRepo, db := setupObjectCleanupRaceTest(t, "http://127.0.0.1")
			ownerID := testutil.InsertUser(t, db, testutil.UserFixture{
				UserID:       1,
				Email:        "file-app-owner@ente.com",
				CreationTime: 1,
			})
			testutil.InsertUsage(t, db, ownerID, 0)
			if _, err := db.Exec(`UPDATE usage SET photos_file_count = 2, locker_file_count = 3 WHERE user_id = $1`, ownerID); err != nil {
				t.Fatal(err)
			}
			collectionID := insertObjectCleanupTestCollection(t, db, ownerID)
			if _, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, tt.app, collectionID); err != nil {
				t.Fatal(err)
			}
			const fileObjectKey = "1/file-app-file"
			const thumbnailObjectKey = "1/file-app-thumbnail"
			if _, err := db.Exec(`
				INSERT INTO temp_objects(object_key, expiration_time, bucket_id)
				VALUES ($1, 0, 'b2-eu-cen'), ($2, 0, 'b2-eu-cen')`, fileObjectKey, thumbnailObjectKey); err != nil {
				t.Fatalf("failed to stage objects: %v", err)
			}

			file, usage, err := fileRepo.Create(objectCleanupTestFile(
				ownerID, collectionID, fileObjectKey, thumbnailObjectKey,
			), 100, 10, 110, ownerID, tt.app)
			if err != nil {
				t.Fatal(err)
			}
			if usage != 110 {
				t.Fatalf("usage = %d, want 110", usage)
			}

			var app string
			if err := db.QueryRow(`SELECT app FROM files WHERE file_id = $1`, file.ID).Scan(&app); err != nil {
				t.Fatal(err)
			}
			if app != string(tt.app) {
				t.Fatalf("file app = %q, want %q", app, tt.app)
			}
			var photos, locker, version int64
			if err := db.QueryRow(`SELECT photos_file_count, locker_file_count, file_count_source_version
				FROM usage WHERE user_id = $1`, ownerID).Scan(&photos, &locker, &version); err != nil {
				t.Fatal(err)
			}
			if photos != tt.wantPhotos || locker != tt.wantLocker || version != tt.wantVersion {
				t.Fatalf("file counts = (%d, %d, %d), want (%d, %d, %d)",
					photos, locker, version, tt.wantPhotos, tt.wantLocker, tt.wantVersion)
			}
		})
	}
}

func TestFileRegistrationRollsBackWhenCleanupWins(t *testing.T) {
	cleanupDeleting := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var blockCleanup sync.Once
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			blockCleanup.Do(func() {
				close(cleanupDeleting)
				<-releaseCleanup
			})
			w.WriteHeader(http.StatusNoContent)
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s3Server.Close)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }

	cleanupController, fileRepo, db := setupObjectCleanupRaceTest(t, s3Server.URL)
	t.Cleanup(release)
	ownerID := testutil.InsertUser(t, db, testutil.UserFixture{
		UserID:       1,
		Email:        "cleanup-race-owner@ente.com",
		CreationTime: 1,
	})
	collectionID := insertObjectCleanupTestCollection(t, db, ownerID)
	const fileObjectKey = "1/cleanup-race-file"
	const thumbnailObjectKey = "1/cleanup-race-thumbnail"
	if _, err := db.Exec(`
		INSERT INTO temp_objects(object_key, expiration_time, bucket_id)
		VALUES ($1, 0, 'b2-eu-cen'), ($2, 0, 'b2-eu-cen')`, fileObjectKey, thumbnailObjectKey); err != nil {
		t.Fatalf("failed to stage objects: %v", err)
	}

	cleanupDone := make(chan int, 1)
	go func() { cleanupDone <- cleanupController.removeUnreportedObjects() }()
	select {
	case <-cleanupDeleting:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not reach object deletion")
	}

	registrationDone := make(chan error, 1)
	go func() {
		_, _, err := fileRepo.Create(objectCleanupTestFile(
			ownerID, collectionID, fileObjectKey, thumbnailObjectKey,
		), 100, 10, 110, ownerID, ente.Photos)
		registrationDone <- err
	}()
	waitForTempObjectDeleteLock(t, db)
	release()

	select {
	case count := <-cleanupDone:
		if count != 2 {
			t.Fatalf("cleanup removed %d objects, want 2", count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	select {
	case err := <-registrationDone:
		var apiErr *ente.ApiError
		if !errors.As(err, &apiErr) || apiErr.Message != "staged upload not found" {
			t.Fatalf("registration error = %v, want staged upload not found", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registration did not finish")
	}

	var files, objectKeys, collectionFiles, tempObjects int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM files WHERE owner_id = $1),
		(SELECT COUNT(*) FROM object_keys WHERE object_key IN ($2, $3)),
		(SELECT COUNT(*) FROM collection_files WHERE collection_id = $4),
		(SELECT COUNT(*) FROM temp_objects WHERE object_key IN ($2, $3))`,
		ownerID, fileObjectKey, thumbnailObjectKey, collectionID,
	).Scan(&files, &objectKeys, &collectionFiles, &tempObjects); err != nil {
		t.Fatalf("failed to read registration state: %v", err)
	}
	if files != 0 || objectKeys != 0 || collectionFiles != 0 || tempObjects != 0 {
		t.Fatalf("unexpected registration state: files=%d object_keys=%d collection_files=%d temp_objects=%d",
			files, objectKeys, collectionFiles, tempObjects)
	}
}

func waitForTempObjectDeleteLock(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%DELETE FROM temp_objects%'
		)`).Scan(&waiting)
		if err != nil {
			t.Fatalf("failed to inspect registration lock wait: %v", err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("registration did not wait for the temporary-object lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func setupObjectCleanupRaceTest(t *testing.T, endpoint string) (*ObjectCleanupController, *repo.FileRepository, *sql.DB) {
	t.Helper()
	testutil.WithServerRoot(t)
	viper.Reset()
	if err := config.ConfigureViper("local"); err != nil {
		t.Fatal(err)
	}
	viper.Set("s3.b2-eu-cen.key", "test-key")
	viper.Set("s3.b2-eu-cen.secret", "test-secret")
	viper.Set("s3.b2-eu-cen.endpoint", endpoint)
	viper.Set("s3.b2-eu-cen.region", "us-east-1")
	viper.Set("s3.b2-eu-cen.bucket", "test-bucket")
	viper.Set("s3.b2-eu-cen.disable_ssl", true)
	viper.Set("s3.use_path_style_urls", true)
	t.Cleanup(viper.Reset)

	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	s3Config := s3config.NewS3Config()
	objectRepo := &repo.ObjectRepository{DB: db}
	objectCleanupRepo := &repo.ObjectCleanupRepository{DB: db}
	return NewObjectCleanupController(objectCleanupRepo, objectRepo, s3Config), &repo.FileRepository{
		DB:                db,
		S3Config:          s3Config,
		QueueRepo:         &repo.QueueRepository{DB: db},
		ObjectRepo:        objectRepo,
		ObjectCleanupRepo: objectCleanupRepo,
		ObjectCopiesRepo:  &repo.ObjectCopiesRepository{DB: db},
	}, db
}

func insertObjectCleanupTestCollection(t *testing.T, db *sql.DB, ownerID int64) int64 {
	t.Helper()
	var collectionID int64
	err := db.QueryRow(`
		INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
		VALUES($1, 'encrypted-key', 'key-nonce', 'Cleanup race', 'album', '{}', 1, 'photos')
		RETURNING collection_id`, ownerID).Scan(&collectionID)
	if err != nil {
		t.Fatalf("failed to insert collection: %v", err)
	}
	return collectionID
}

func objectCleanupTestFile(ownerID, collectionID int64, fileObjectKey, thumbnailObjectKey string) ente.File {
	return ente.File{
		OwnerID:            ownerID,
		CollectionID:       collectionID,
		EncryptedKey:       "encrypted-key",
		KeyDecryptionNonce: "key-nonce",
		File: ente.FileAttributes{
			ObjectKey:        fileObjectKey,
			DecryptionHeader: "file-header",
		},
		Thumbnail: ente.FileAttributes{
			ObjectKey:        thumbnailObjectKey,
			DecryptionHeader: "thumbnail-header",
		},
		Metadata: ente.FileAttributes{
			EncryptedData:    "encrypted-metadata",
			DecryptionHeader: "metadata-header",
		},
		UpdationTime: 1,
		Info:         &ente.FileInfo{FileSize: 100, ThumbnailSize: 10},
	}
}
