package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// lock_timeout backs up the caller's context deadline: a waiter holds a pooled
// connection until it gets the lock. The key is hashtextextended('<namespace>:<id>'),
// which older binaries also compute, so don't change its format.
func lockAdvisoryXact(ctx context.Context, tx *sql.Tx, namespace string, id int64, timeout time.Duration) error {
	// 0 would mean no timeout.
	timeoutMillis := max(timeout.Milliseconds(), 1)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, timeoutMillis)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::bigint, 0))`, namespace, id)
	return err
}

func isLockTimeout(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}
