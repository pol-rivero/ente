package testutil

import (
	"database/sql"
	"testing"
	"time"
)

// Takes the quota lock the way UsageRepository.LockQuota does, until the
// returned transaction ends.
func HoldQuotaLock(t *testing.T, db *sql.DB, subscriptionAdminID int64) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('quota:' || $1::bigint, 0))`, subscriptionAdminID); err != nil {
		t.Fatalf("taking the quota lock: %v", err)
	}
	return tx
}

func WaitForAdvisoryLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("counting advisory lock waiters: %v", err)
		}
		if waiting == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d advisory lock waiters, want %d", waiting, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
