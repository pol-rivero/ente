package repo

import (
	"testing"
	"time"

	"github.com/ente/museum/internal/testutil"
	"github.com/stretchr/testify/require"
)

// Without serialized claims, a claim made while another pod's claim of the
// same user's other job is uncommitted would run both jobs.
func TestCopyJobClaimWaitsForAnUncommittedClaim(t *testing.T) {
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	for _, requestID := range []string{"a", "b"} {
		_, err := db.Exec(`INSERT INTO file_copy_jobs (user_id, request_id, src_collection_id, dst_collection_id, items)
			VALUES (1, $1, 1, 2, '[]')`, requestID)
		require.NoError(t, err)
	}
	r := &FileCopyJobRepository{DB: db}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	first, err := r.claimTx(t.Context(), tx, "first", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)

	type claim struct {
		job *FileCopyJob
		err error
	}
	second := make(chan claim, 1)
	go func() {
		job, err := r.Claim(t.Context(), "second", time.Minute)
		second <- claim{job, err}
	}()
	require.Eventually(t, func() bool {
		var waiters int
		err := db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiters)
		return err == nil && waiters == 1
	}, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, tx.Commit())

	got := <-second
	require.NoError(t, got.err)
	require.Nil(t, got.job)
	var running int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM file_copy_jobs WHERE status = 'running'`).Scan(&running))
	require.Equal(t, 1, running)
}
