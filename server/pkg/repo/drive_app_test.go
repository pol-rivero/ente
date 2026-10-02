package repo

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/repo/public"
)

func TestDriveAppMigrations(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)

	var enumHasDrive bool
	if err := db.QueryRow(`SELECT 'drive' = ANY(enum_range(NULL::app)::text[])`).Scan(&enumHasDrive); err != nil {
		t.Fatal(err)
	}
	if !enumHasDrive {
		t.Fatal("app enum is missing drive")
	}
	var definition string
	var validated bool
	if err := db.QueryRow(`SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint
		WHERE conname = 'files_app_supported' AND conrelid = 'files'::regclass`).Scan(&definition, &validated); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"photos", "locker", "drive"} {
		if !strings.Contains(definition, "'"+app+"'") {
			t.Fatalf("files_app_supported = %s, missing %s", definition, app)
		}
	}
	if validated {
		t.Fatal("files_app_supported should still be NOT VALID")
	}

	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Drive} {
		if _, err := db.Exec(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1)`, userID, app); err != nil {
			t.Fatalf("insert %s file: %v", app, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, 'auth', 'header', 'header', 'header', 'metadata', 1)`, userID); err == nil {
		t.Fatal("files_app_supported accepted an auth file")
	}
	collectionID := insertDriveTestCollection(t, db, userID, ente.Drive)
	var app string
	if err := db.QueryRow(`SELECT app FROM collections WHERE collection_id = $1`, collectionID).Scan(&app); err != nil || app != "drive" {
		t.Fatalf("collection app = (%q, %v), want drive", app, err)
	}
}

func TestFileCountDeltaIgnoresDrive(t *testing.T) {
	for _, tt := range []struct {
		app                    ente.App
		wantPhotos, wantLocker int64
		wantOK                 bool
	}{
		{ente.Photos, 3, 0, true},
		{ente.Locker, 0, 3, true},
		{ente.Drive, 0, 0, true},
		{ente.Auth, 0, 0, false},
	} {
		photos, locker, ok := fileCountDelta(tt.app, 3)
		if photos != tt.wantPhotos || locker != tt.wantLocker || ok != tt.wantOK {
			t.Errorf("fileCountDelta(%s) = (%d, %d, %t), want (%d, %d, %t)", tt.app, photos, locker, ok, tt.wantPhotos, tt.wantLocker, tt.wantOK)
		}
	}
}

func TestTrashAndRestoreDriveFileKeepsStoredFileCounts(t *testing.T) {
	repository, db, userID := setupCollectionMembershipTest(t)
	setReadyFileCounts(t, db, userID, 2, 3)
	driveCollectionID := insertDriveTestCollection(t, db, userID, ente.Drive)
	fileID := insertDriveTestFile(t, db, userID, ente.Drive)
	linkObjectTestFileToCollection(t, db, driveCollectionID, fileID, userID)
	repository.TrashRepo.FileLinkRepo = public.NewFileLinkRepo(db)

	if err := repository.TrashRepo.TrashFiles(t.Context(), userID, ente.TrashRequest{
		TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: driveCollectionID}},
	}); err != nil {
		t.Fatalf("TrashFiles() error = %v", err)
	}
	assertReadyFileCounts(t, db, userID, 2, 3, 0)

	if err := repository.RestoreFiles(t.Context(), userID, driveCollectionID,
		[]ente.CollectionFileItem{collectionMembershipTestItem(fileID)}); err != nil {
		t.Fatalf("RestoreFiles() error = %v", err)
	}
	assertReadyFileCounts(t, db, userID, 2, 3, 0)
}

func TestTrashStillInvalidatesCrossAppDriveMemberships(t *testing.T) {
	repository, db, userID := setupCollectionMembershipTest(t)
	setReadyFileCounts(t, db, userID, 1, 0)
	photosCollectionID := insertDriveTestCollection(t, db, userID, ente.Photos)
	driveCollectionID := insertDriveTestCollection(t, db, userID, ente.Drive)
	fileID := insertObjectTestFile(t, db, userID)
	linkObjectTestFileToCollection(t, db, photosCollectionID, fileID, userID)
	linkObjectTestFileToCollection(t, db, driveCollectionID, fileID, userID)
	repository.TrashRepo.FileLinkRepo = public.NewFileLinkRepo(db)

	if err := repository.TrashRepo.TrashFiles(t.Context(), userID, ente.TrashRequest{
		TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: photosCollectionID}},
	}); err != nil {
		t.Fatalf("TrashFiles() error = %v", err)
	}
	photos, locker, version := readFileCountState(t, db, userID)
	if photos.Valid || locker.Valid || version != 1 {
		t.Fatalf("file count state = (%v, %v, %d), want (NULL, NULL, 1)", photos, locker, version)
	}
}

func TestInitializeFileCountsExcludesDriveFiles(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Drive, ente.Drive} {
		collectionID := insertDriveTestCollection(t, db, userID, app)
		linkObjectTestFileToCollection(t, db, collectionID, insertDriveTestFile(t, db, userID, app), userID)
	}

	initialized, err := (&UsageRepository{DB: db}).InitializeFileCounts(t.Context(), userID)
	if err != nil || !initialized {
		t.Fatalf("InitializeFileCounts() = (%t, %v), want true", initialized, err)
	}
	assertReadyFileCounts(t, db, userID, 1, 1, 0)
}

func TestInitializeFileCountsRejectsCrossAppDriveMemberships(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	fileID := insertObjectTestFile(t, db, userID)
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		linkObjectTestFileToCollection(t, db, insertDriveTestCollection(t, db, userID, app), fileID, userID)
	}

	initialized, err := (&UsageRepository{DB: db}).InitializeFileCounts(t.Context(), userID)
	if initialized || !errors.Is(err, ErrFileCountIneligible) {
		t.Fatalf("InitializeFileCounts() = (%t, %v), want ineligible", initialized, err)
	}
}

func TestEmptyTrashUsesPerAppQueue(t *testing.T) {
	queueRepo, db := setupQueueRepositoryTest(t)
	trashRepo := &TrashRepository{DB: db, QueueRepo: queueRepo}
	wantQueues := map[ente.App]string{
		ente.Photos: TrashEmptyQueue,
		ente.Locker: TrashEmptyLockerQueue,
		ente.Drive:  TrashEmptyDriveQueue,
	}
	for app, queueName := range wantQueues {
		if err := trashRepo.EmptyTrash(t.Context(), 1, 10, app); err != nil {
			t.Fatalf("EmptyTrash(%s) error = %v", app, err)
		}
		items, err := queueRepo.GetItemsReadyForDeletion(queueName, 10)
		if err != nil {
			t.Fatalf("GetItemsReadyForDeletion(%s) error = %v", queueName, err)
		}
		if len(items) != 1 || items[0].Item != "1"+EmptyTrashQueueItemSeparator+"10" {
			t.Fatalf("%s queue items = %+v, want one item for %s", queueName, items, app)
		}
		if _, err := db.Exec(`DELETE FROM queue WHERE queue_name = $1`, queueName); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGetDistinctFileAppsTreatsMissingAppAsPhotos(t *testing.T) {
	_, db, userID := setupCollectionMembershipTest(t)
	legacyFileID := insertObjectTestFile(t, db, userID)
	photosFileID := insertDriveTestFile(t, db, userID, ente.Photos)
	driveFileID := insertDriveTestFile(t, db, userID, ente.Drive)
	repository := &FileRepository{DB: db}

	for _, tt := range []struct {
		fileIDs []int64
		want    []ente.App
	}{
		{[]int64{legacyFileID}, []ente.App{ente.Photos}},
		{[]int64{legacyFileID, photosFileID}, []ente.App{ente.Photos}},
		{[]int64{driveFileID}, []ente.App{ente.Drive}},
		{[]int64{legacyFileID, driveFileID}, []ente.App{ente.Drive, ente.Photos}},
	} {
		apps, err := repository.GetDistinctFileApps(t.Context(), tt.fileIDs)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(apps)
		if !slices.Equal(apps, tt.want) {
			t.Fatalf("GetDistinctFileApps(%v) = %v, want %v", tt.fileIDs, apps, tt.want)
		}
	}
}

func insertDriveTestCollection(t *testing.T, db *sql.DB, ownerID int64, app ente.App) int64 {
	t.Helper()
	collectionID := insertObjectTestCollection(t, db, ownerID)
	if _, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, app, collectionID); err != nil {
		t.Fatal(err)
	}
	return collectionID
}

func insertDriveTestFile(t *testing.T, db *sql.DB, ownerID int64, app ente.App) int64 {
	t.Helper()
	fileID := insertObjectTestFile(t, db, ownerID)
	if _, err := db.Exec(`UPDATE files SET app = $1 WHERE file_id = $2`, app, fileID); err != nil {
		t.Fatal(err)
	}
	return fileID
}
