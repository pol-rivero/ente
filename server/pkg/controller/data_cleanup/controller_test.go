package data_cleanup

import (
	"strings"
	"testing"

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

func TestEmptyTrashStageQueuesEveryStorageApp(t *testing.T) {
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
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 83, Email: "deleted@example.com", CreationTime: 1})
	if _, err := db.Exec(`INSERT INTO data_cleanup(user_id) VALUES($1)`, userID); err != nil {
		t.Fatalf("failed to insert cleanup row: %v", err)
	}
	queueRepo := &repo.QueueRepository{DB: db}
	controller := &DeleteUserCleanupController{
		Repo:      &cleanuprepo.Repository{DB: db},
		TrashRepo: &repo.TrashRepository{DB: db, QueueRepo: queueRepo},
	}

	if err := controller.emptyTrash(t.Context(), &cleanupentity.DataCleanup{UserID: userID, Stage: cleanupentity.Trash}); err != nil {
		t.Fatalf("emptyTrash() error = %v", err)
	}
	for _, queueName := range []string{repo.TrashEmptyQueue, repo.TrashEmptyLockerQueue, repo.TrashEmptyDriveQueue} {
		items, err := queueRepo.GetItemsReadyForDeletion(queueName, 10)
		if err != nil {
			t.Fatalf("GetItemsReadyForDeletion(%s) error = %v", queueName, err)
		}
		if len(items) != 1 || !strings.HasPrefix(items[0].Item, "83"+repo.EmptyTrashQueueItemSeparator) {
			t.Fatalf("%s items = %+v, want one item for user %d", queueName, items, userID)
		}
	}
}
