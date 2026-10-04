package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ente/stacktrace"
	"github.com/lib/pq"
)

// Runs fn in a transaction that takes the advisory lock first. fn must use
// only tx: a second pooled connection taken while waiters hold the rest of
// the pool would never come.
func InAdvisoryLockTx(ctx context.Context, db *sql.DB, namespace string, id int64, timeout time.Duration, timeoutErr error, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	if err := lockAdvisoryXact(ctx, tx, namespace, id, timeout, timeoutErr); err != nil {
		return stacktrace.Propagate(err, "")
	}
	if err := fn(tx); err != nil {
		return stacktrace.Propagate(err, "")
	}
	return stacktrace.Propagate(tx.Commit(), "")
}

// lock_timeout backs up the caller's context deadline: a waiter holds a pooled
// connection until it gets the lock. The key is hashtextextended('<namespace>:<id>'),
// which older binaries also compute, so don't change its format.
func lockAdvisoryXact(ctx context.Context, tx *sql.Tx, namespace string, id int64, timeout time.Duration, timeoutErr error) error {
	// 0 would mean no timeout.
	timeoutMillis := max(timeout.Milliseconds(), 1)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, timeoutMillis)); err != nil {
		return stacktrace.Propagate(err, "")
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::bigint, 0))`, namespace, id)
	if isLockTimeout(err) {
		return stacktrace.Propagate(timeoutErr, "%v", err)
	}
	return stacktrace.Propagate(err, "")
}

func isLockTimeout(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}
