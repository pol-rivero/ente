package collections

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/controller/access"
	"github.com/ente/museum/pkg/repo"
	publicRepo "github.com/ente/museum/pkg/repo/public"
	"github.com/gin-gonic/gin"
)

// Collection fixtures: an "a" and a "b" collection per app. Files start in
// their app's "a" collection; "legacy" has no stored app (pre-migration 145).
type driveAppFixture struct {
	controller  *CollectionController
	db          *sql.DB
	ownerID     int64
	collections map[string]int64
	files       map[string]int64
}

func TestAddFilesKeepsDriveIsolated(t *testing.T) {
	for _, tt := range []struct {
		name, file, to string
		header         ente.App
		wantCrossApp   bool
	}{
		{name: "photos to photos", file: "photos", to: "photos-b", header: ente.Photos},
		{name: "legacy to photos", file: "legacy", to: "photos-b", header: ente.Photos},
		{name: "locker to photos stays allowed", file: "locker", to: "photos-b", header: ente.Photos},
		{name: "photos to locker stays allowed", file: "photos", to: "locker-b", header: ente.Locker},
		{name: "drive to drive", file: "drive", to: "drive-b", header: ente.Drive},
		{name: "drive to photos", file: "drive", to: "photos-b", header: ente.Photos, wantCrossApp: true},
		{name: "drive to locker", file: "drive", to: "locker-b", header: ente.Locker, wantCrossApp: true},
		{name: "photos to drive", file: "photos", to: "drive-b", header: ente.Drive, wantCrossApp: true},
		{name: "legacy to drive", file: "legacy", to: "drive-b", header: ente.Drive, wantCrossApp: true},
		{name: "locker to drive", file: "locker", to: "drive-b", header: ente.Drive, wantCrossApp: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setupDriveAppFixture(t)
			err := f.controller.AddFiles(f.context(tt.header), f.ownerID,
				[]ente.CollectionFileItem{driveAppTestItem(f.files[tt.file])}, f.collections[tt.to])
			f.requireOutcome(t, err, tt.wantCrossApp, tt.file, tt.to)
		})
	}

	t.Run("mixed batch into drive", func(t *testing.T) {
		f := setupDriveAppFixture(t)
		err := f.controller.AddFiles(f.context(ente.Drive), f.ownerID, []ente.CollectionFileItem{
			driveAppTestItem(f.files["drive"]), driveAppTestItem(f.files["photos"]),
		}, f.collections["drive-b"])
		requireCrossAppFileError(t, err)
		f.requireActiveMembership(t, "drive", "drive-b", false)
	})
}

func TestRestoreFilesKeepsDriveIsolated(t *testing.T) {
	for _, tt := range []struct {
		name, file, to string
		header         ente.App
		wantCrossApp   bool
	}{
		{name: "photos to photos", file: "photos", to: "photos-b", header: ente.Photos},
		{name: "legacy to photos", file: "legacy", to: "photos-b", header: ente.Photos},
		{name: "locker to photos stays allowed", file: "locker", to: "photos-b", header: ente.Photos},
		{name: "photos to locker with photos header stays allowed", file: "photos", to: "locker-b", header: ente.Photos},
		{name: "drive to drive", file: "drive", to: "drive-b", header: ente.Drive},
		{name: "drive to photos", file: "drive", to: "photos-b", header: ente.Photos, wantCrossApp: true},
		{name: "drive to photos with drive header", file: "drive", to: "photos-b", header: ente.Drive, wantCrossApp: true},
		{name: "drive to drive with photos header", file: "drive", to: "drive-b", header: ente.Photos, wantCrossApp: true},
		{name: "photos to drive", file: "photos", to: "drive-b", header: ente.Drive, wantCrossApp: true},
		{name: "legacy to drive", file: "legacy", to: "drive-b", header: ente.Drive, wantCrossApp: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setupDriveAppFixture(t)
			fileID := f.files[tt.file]
			if err := f.controller.CollectionRepo.TrashRepo.TrashFiles(t.Context(), f.ownerID, ente.TrashRequest{
				TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: f.collections[tt.file+"-a"]}},
			}); err != nil {
				t.Fatal(err)
			}
			err := f.controller.RestoreFiles(f.context(tt.header), f.ownerID, f.collections[tt.to],
				[]ente.CollectionFileItem{driveAppTestItem(fileID)})
			f.requireOutcome(t, err, tt.wantCrossApp, tt.file, tt.to)
		})
	}
}

func TestMoveFilesKeepsDriveIsolated(t *testing.T) {
	for _, tt := range []struct {
		name, file, from, to string
		wantCrossApp         bool
	}{
		{name: "photos within photos", file: "photos", from: "photos-a", to: "photos-b"},
		{name: "locker within locker", file: "locker", from: "locker-a", to: "locker-b"},
		{name: "drive within drive", file: "drive", from: "drive-a", to: "drive-b"},
		// The source collection isn't required to contain the files.
		{name: "drive within photos", file: "drive", from: "photos-a", to: "photos-b", wantCrossApp: true},
		{name: "photos within drive", file: "photos", from: "drive-a", to: "drive-b", wantCrossApp: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setupDriveAppFixture(t)
			err := f.controller.MoveFiles(f.context(ente.Photos), ente.MoveFilesRequest{
				FromCollectionID: f.collections[tt.from],
				ToCollectionID:   f.collections[tt.to],
				Files:            []ente.CollectionFileItem{driveAppTestItem(f.files[tt.file])},
			})
			f.requireOutcome(t, err, tt.wantCrossApp, tt.file, tt.to)
		})
	}
}

func setupDriveAppFixture(t *testing.T) *driveAppFixture {
	t.Helper()
	db, collectionRepo, ownerID, _ := setupCollectionShareTest(t)
	fileRepo := &repo.FileRepository{DB: db}
	collectionRepo.FileRepo = fileRepo
	collectionRepo.TrashRepo = &repo.TrashRepository{DB: db, FileLinkRepo: publicRepo.NewFileLinkRepo(db)}
	if _, err := db.Exec(`INSERT INTO usage(user_id, storage_consumed) VALUES ($1, 0)`, ownerID); err != nil {
		t.Fatal(err)
	}
	f := &driveAppFixture{
		controller: &CollectionController{
			AccessCtrl:     access.NewAccessController(collectionRepo, fileRepo),
			CollectionRepo: collectionRepo,
			FileRepo:       fileRepo,
		},
		db:          db,
		ownerID:     ownerID,
		collections: make(map[string]int64),
		files:       make(map[string]int64),
	}
	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Drive} {
		for _, suffix := range []string{"-a", "-b"} {
			var collectionID int64
			if err := db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
				VALUES ($1, 'key', 'nonce', 'name', 'album', '{}', 1, $2) RETURNING collection_id`, ownerID, app).Scan(&collectionID); err != nil {
				t.Fatal(err)
			}
			f.collections[string(app)+suffix] = collectionID
		}
	}
	for name, app := range map[string]any{"photos": ente.Photos, "legacy": nil, "locker": ente.Locker, "drive": ente.Drive} {
		var fileID int64
		if err := db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, ownerID, app).Scan(&fileID); err != nil {
			t.Fatal(err)
		}
		home := name + "-a"
		if name == "legacy" {
			home = "photos-a"
			f.collections["legacy-a"] = f.collections[home]
		}
		if _, err := db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
			VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, f.collections[home], fileID, ownerID); err != nil {
			t.Fatal(err)
		}
		f.files[name] = fileID
	}
	return f
}

func (f *driveAppFixture) context(app ente.App) *gin.Context {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/collections/add-files", nil)
	ctx.Request.Header.Set("X-Auth-User-ID", strconv.FormatInt(f.ownerID, 10))
	ctx.Request.Header.Set("X-Client-Package", "io.ente."+string(app))
	return ctx
}

func (f *driveAppFixture) requireOutcome(t *testing.T, err error, wantCrossApp bool, file, to string) {
	t.Helper()
	if wantCrossApp {
		requireCrossAppFileError(t, err)
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireActiveMembership(t, file, to, !wantCrossApp)
}

func (f *driveAppFixture) requireActiveMembership(t *testing.T, file, collection string, want bool) {
	t.Helper()
	var active bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM collection_files
		WHERE collection_id = $1 AND file_id = $2 AND is_deleted = FALSE)`,
		f.collections[collection], f.files[file]).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != want {
		t.Fatalf("%s file active in %s = %t, want %t", file, collection, active, want)
	}
}

func requireCrossAppFileError(t *testing.T, err error) {
	t.Helper()
	var apiErr *ente.ApiError
	if !errors.As(err, &apiErr) || apiErr.Code != ente.CrossAppFile || apiErr.HttpStatusCode != http.StatusBadRequest {
		t.Fatalf("error = %v, want %s (400)", err, ente.CrossAppFile)
	}
}

func driveAppTestItem(fileID int64) ente.CollectionFileItem {
	return ente.CollectionFileItem{
		ID:                 fileID,
		EncryptedKey:       b64OfLen(encryptedCollectionKeyLen),
		KeyDecryptionNonce: b64OfLen(secretboxNonceBytes),
	}
}
