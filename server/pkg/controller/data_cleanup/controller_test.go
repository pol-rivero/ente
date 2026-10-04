package data_cleanup

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ente/museum/ente"
	cleanupentity "github.com/ente/museum/ente/data_cleanup"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo"
	cleanuprepo "github.com/ente/museum/pkg/repo/datacleanup"
)

func TestStartCleanupCancelsRecoveredAccount(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM data_cleanup`); err != nil {
			t.Errorf("failed to clear data_cleanup: %v", err)
		}
		testutil.ResetTables(t, db)
	})

	tests := []struct {
		name             string
		cleanupRowExists bool
	}{
		{name: "stale fetched row"},
		{name: "legacy cleanup row", cleanupRowExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM data_cleanup`); err != nil {
				t.Fatalf("failed to clear data_cleanup: %v", err)
			}
			testutil.ResetTables(t, db)

			userID := testutil.InsertUser(t, db, testutil.UserFixture{
				UserID:       82,
				Email:        "recovered@example.com",
				CreationTime: 1,
			})
			if tt.cleanupRowExists {
				if _, err := db.Exec(`INSERT INTO data_cleanup(user_id) VALUES($1)`, userID); err != nil {
					t.Fatalf("failed to insert cleanup row: %v", err)
				}
			}
			controller := &DeleteUserCleanupController{
				Repo: &cleanuprepo.Repository{DB: db},
				UserRepo: &repo.UserRepository{
					DB:                  db,
					SecretEncryptionKey: testutil.SecretEncryptionKey(),
				},
			}

			if err := controller.startCleanup(t.Context(), &cleanupentity.DataCleanup{
				UserID: userID,
				Stage:  cleanupentity.Scheduled,
			}); err != nil {
				t.Fatalf("startCleanup() error = %v", err)
			}
			var cleanupRows int
			if err := db.QueryRow(`SELECT count(*) FROM data_cleanup WHERE user_id = $1`, userID).Scan(&cleanupRows); err != nil {
				t.Fatalf("failed to count cleanup rows: %v", err)
			}
			if cleanupRows != 0 {
				t.Fatalf("cleanup row count = %d, want 0", cleanupRows)
			}
		})
	}
}

func TestEmptyTrashStageQueuesDriveOnlyForDriveUsers(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	reset := func() {
		if _, err := db.Exec(`DELETE FROM data_cleanup`); err != nil {
			t.Errorf("failed to clear data_cleanup: %v", err)
		}
		if _, err := db.Exec(`TRUNCATE TABLE queue RESTART IDENTITY`); err != nil {
			t.Errorf("failed to clear queue: %v", err)
		}
		testutil.ResetTables(t, db)
	}
	reset()
	t.Cleanup(reset)
	queueRepo := &repo.QueueRepository{DB: db}
	controller := &DeleteUserCleanupController{
		Repo:           &cleanuprepo.Repository{DB: db},
		TrashRepo:      &repo.TrashRepository{DB: db, QueueRepo: queueRepo},
		CollectionRepo: &repo.CollectionRepository{DB: db},
	}
	queued := func(userID int64) []string {
		t.Helper()
		var queues []string
		for _, queueName := range []string{repo.TrashEmptyQueue, repo.TrashEmptyLockerQueue, repo.TrashEmptyDriveQueue} {
			items, err := queueRepo.GetItemsReadyForDeletion(queueName, 10)
			if err != nil {
				t.Fatalf("GetItemsReadyForDeletion(%s) error = %v", queueName, err)
			}
			for _, item := range items {
				if strings.HasPrefix(item.Item, strconv.FormatInt(userID, 10)+repo.EmptyTrashQueueItemSeparator) {
					queues = append(queues, queueName)
				}
			}
		}
		return queues
	}

	for userID, hasDrive := range map[int64]bool{83: true, 84: false} {
		testutil.InsertUser(t, db, testutil.UserFixture{UserID: userID, Email: fmt.Sprintf("deleted-%d@example.com", userID), CreationTime: 1})
		if _, err := db.Exec(`INSERT INTO data_cleanup(user_id) VALUES($1)`, userID); err != nil {
			t.Fatalf("failed to insert cleanup row: %v", err)
		}
		if hasDrive {
			driveFolder := testutil.InsertCollection(t, db, userID, ente.Drive, "folder")
			if _, err := db.Exec(`UPDATE collections SET is_deleted = TRUE WHERE collection_id = $1`, driveFolder); err != nil {
				t.Fatal(err)
			}
		}
		testutil.InsertCollection(t, db, userID, ente.Photos, "album")
		if err := controller.emptyTrash(t.Context(), &cleanupentity.DataCleanup{UserID: userID, Stage: cleanupentity.Trash}); err != nil {
			t.Fatalf("emptyTrash() error = %v", err)
		}
	}
	if got := queued(83); len(got) != 3 {
		t.Fatalf("queues for a Drive user = %v, want all three", got)
	}
	if got := queued(84); len(got) != 2 || slices.Contains(got, repo.TrashEmptyDriveQueue) {
		t.Fatalf("queues for a non-Drive user = %v, want Photos and Locker", got)
	}
}

func TestDeleteCollectionsStageQueuesDriveCollectionsSeparately(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	reset := func() {
		if _, err := db.Exec(`DELETE FROM data_cleanup`); err != nil {
			t.Errorf("failed to clear data_cleanup: %v", err)
		}
		if _, err := db.Exec(`TRUNCATE TABLE queue RESTART IDENTITY`); err != nil {
			t.Errorf("failed to clear queue: %v", err)
		}
		testutil.ResetTables(t, db)
	}
	reset()
	t.Cleanup(reset)
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 84, Email: "deleted-drive@example.com", CreationTime: 1})
	if _, err := db.Exec(`INSERT INTO data_cleanup(user_id) VALUES($1)`, userID); err != nil {
		t.Fatalf("failed to insert cleanup row: %v", err)
	}
	collections := map[string]int64{}
	for _, name := range []string{"photos", "locker", "drive", "drive-child", "drive-deleted"} {
		app := strings.Split(name, "-")[0]
		var parentID, key any
		if name == "drive-child" {
			parentID, key = collections["drive"], "key"
		}
		var id int64
		if err := db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes,
				updation_time, app, parent_id, parent_encrypted_key, parent_key_nonce, is_deleted)
			VALUES ($1, 'key', 'nonce', 'name', 'folder', '{}', 1, $2, $3, $4, $4, $5) RETURNING collection_id`,
			userID, app, parentID, key, name == "drive-deleted").Scan(&id); err != nil {
			t.Fatalf("failed to insert %s collection: %v", name, err)
		}
		collections[name] = id
	}
	queueRepo := &repo.QueueRepository{DB: db}
	controller := &DeleteUserCleanupController{
		Repo:           &cleanuprepo.Repository{DB: db},
		CollectionRepo: &repo.CollectionRepository{DB: db, QueueRepo: queueRepo},
	}

	if err := controller.deleteCollections(t.Context(), &cleanupentity.DataCleanup{UserID: userID, Stage: cleanupentity.Collection}); err != nil {
		t.Fatalf("deleteCollections() error = %v", err)
	}
	want := map[string][]int64{
		repo.TrashCollectionQueueV3:    {collections["photos"], collections["locker"]},
		repo.TrashCollectionDriveQueue: {collections["drive"], collections["drive-child"]},
	}
	for queueName, ids := range want {
		items, err := queueRepo.GetItemsReadyForDeletion(queueName, 10)
		if err != nil {
			t.Fatalf("GetItemsReadyForDeletion(%s) error = %v", queueName, err)
		}
		got := map[string]bool{}
		for _, item := range items {
			got[item.Item] = true
		}
		if len(got) != len(ids) {
			t.Fatalf("%s items = %+v, want %v", queueName, items, ids)
		}
		for _, id := range ids {
			if !got[strconv.FormatInt(id, 10)] {
				t.Fatalf("%s items = %+v, want %v", queueName, items, ids)
			}
		}
	}
	var live int
	if err := db.QueryRow(`SELECT count(*) FROM collections WHERE owner_id = $1 AND NOT is_deleted`, userID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live collections = %d, want 0", live)
	}
}
