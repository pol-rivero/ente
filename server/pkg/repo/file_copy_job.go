package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/stacktrace"
	"github.com/lib/pq"
)

var ErrFileCopyJobExists = errors.New("a copy job with this request ID already exists")

type FileCopyJobRepository struct {
	DB                *sql.DB
	ObjectCleanupRepo *ObjectCleanupRepository
}

type FileCopyJob struct {
	ID              int64
	UserID          int64
	RequestID       string
	App             ente.App
	SrcCollectionID int64
	DstCollectionID int64
	Items           json.RawMessage
	Status          ente.CopyJobStatus
	Result          json.RawMessage
	Error           json.RawMessage
	Claims          int
	Attempts        int
	LeaseToken      string
	// Local time taken before the claim, so the lease never ends later here than in the DB.
	LeaseStart time.Time
}

func (r *FileCopyJobRepository) InsertTx(ctx context.Context, tx *sql.Tx, job FileCopyJob) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO file_copy_jobs (user_id, request_id, app, src_collection_id, dst_collection_id, items)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, request_id) DO NOTHING
		RETURNING id`,
		job.UserID, job.RequestID, job.App, job.SrcCollectionID, job.DstCollectionID, []byte(job.Items)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, stacktrace.Propagate(ErrFileCopyJobExists, "")
	}
	return id, stacktrace.Propagate(err, "")
}

func (r *FileCopyJobRepository) GetIDByRequestID(ctx context.Context, userID int64, requestID string) (int64, bool, error) {
	var id int64
	err := r.DB.QueryRowContext(ctx, `SELECT id FROM file_copy_jobs WHERE user_id = $1 AND request_id = $2`,
		userID, requestID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, stacktrace.Propagate(err, "")
	}
	return id, true, nil
}

func (r *FileCopyJobRepository) Get(ctx context.Context, id int64, userID int64) (FileCopyJob, error) {
	job := FileCopyJob{ID: id, UserID: userID}
	var result, jobErr []byte
	err := r.DB.QueryRowContext(ctx, `SELECT status, result, error FROM file_copy_jobs WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&job.Status, &result, &jobErr)
	if errors.Is(err, sql.ErrNoRows) {
		return job, stacktrace.Propagate(ente.ErrNotFoundError.NewErr("copy job not found"), "")
	}
	if err != nil {
		return job, stacktrace.Propagate(err, "")
	}
	job.Result, job.Error = result, jobErr
	return job, nil
}

// Lease times use the DB clock, so clock skew between pods can't steal a live
// lease. A user with a job under a live lease waits, so one backlog can't
// take every worker.
func (r *FileCopyJobRepository) Claim(ctx context.Context, leaseToken string, lease time.Duration) (*FileCopyJob, error) {
	job := FileCopyJob{Status: ente.CopyJobRunning, LeaseToken: leaseToken, LeaseStart: time.Now()}
	var items []byte
	err := r.DB.QueryRowContext(ctx, `
		UPDATE file_copy_jobs
		SET status = 'running', claims = claims + 1, attempts = attempts + 1, lease_token = $1,
		    lease_until = now_utc_micro_seconds() + $2
		WHERE id = (
			SELECT j.id FROM file_copy_jobs j
			WHERE j.status IN ('pending', 'running') AND COALESCE(j.lease_until, 0) < now_utc_micro_seconds()
			  AND NOT EXISTS (
				SELECT 1 FROM file_copy_jobs r
				WHERE r.user_id = j.user_id AND r.status = 'running' AND r.lease_until >= now_utc_micro_seconds())
			ORDER BY j.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, user_id, request_id, app, src_collection_id, dst_collection_id, items, claims, attempts`,
		leaseToken, lease.Microseconds()).
		Scan(&job.ID, &job.UserID, &job.RequestID, &job.App, &job.SrcCollectionID, &job.DstCollectionID, &items,
			&job.Claims, &job.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	job.Items = items
	return &job, nil
}

func (r *FileCopyJobRepository) ExtendLease(ctx context.Context, id int64, leaseToken string, lease time.Duration) (bool, error) {
	return affectsOne(r.DB.ExecContext(ctx, `
		UPDATE file_copy_jobs SET lease_until = now_utc_micro_seconds() + $3
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, leaseToken, lease.Microseconds()))
}

func (r *FileCopyJobRepository) Complete(ctx context.Context, id int64, leaseToken string, result json.RawMessage) (bool, error) {
	return affectsOne(r.DB.ExecContext(ctx, `
		UPDATE file_copy_jobs SET status = 'completed', result = $3, lease_token = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, leaseToken, []byte(result)))
}

// A graceful shutdown doesn't count as an attempt.
func (r *FileCopyJobRepository) Yield(ctx context.Context, id int64, leaseToken string) (bool, error) {
	return affectsOne(r.DB.ExecContext(ctx, `
		UPDATE file_copy_jobs
		SET status = 'pending', attempts = GREATEST(attempts - 1, 0), lease_token = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, leaseToken))
}

func (r *FileCopyJobRepository) Retry(ctx context.Context, id int64, leaseToken string, backoff time.Duration) (bool, error) {
	return affectsOne(r.DB.ExecContext(ctx, `
		UPDATE file_copy_jobs
		SET status = 'pending', lease_token = NULL, lease_until = now_utc_micro_seconds() + $3
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, leaseToken, backoff.Microseconds()))
}

// The job's remaining reservations are released only if this worker still owns it.
func (r *FileCopyJobRepository) Fail(ctx context.Context, job FileCopyJob, result json.RawMessage, jobErr json.RawMessage,
	objectKeys []string, releaseExpiry int64) (bool, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	owned, err := affectsOne(tx.ExecContext(ctx, `
		UPDATE file_copy_jobs SET status = 'failed', result = $3, error = $4, lease_token = NULL, lease_until = NULL
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, job.ID, job.LeaseToken, []byte(result), []byte(jobErr)))
	if err != nil || !owned {
		return false, err
	}
	if err := r.ObjectCleanupRepo.ReleaseTempObjectsTx(ctx, tx, objectKeys, job.UserID, releaseExpiry); err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	return true, stacktrace.Propagate(tx.Commit(), "")
}

func (r *FileCopyJobRepository) DeleteFinishedBefore(ctx context.Context, before int64) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `
		DELETE FROM file_copy_jobs WHERE status IN ('completed', 'failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, stacktrace.Propagate(err, "")
	}
	count, err := res.RowsAffected()
	return count, stacktrace.Propagate(err, "")
}

func (r *FileCopyJobRepository) GetCommittedFileIDsByObjectKey(ctx context.Context, userID int64, objectKeys []string) (map[string]int64, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT k.object_key, k.file_id FROM object_keys k JOIN files f ON f.file_id = k.file_id
		WHERE k.object_key = ANY($1) AND f.owner_id = $2`, pq.Array(objectKeys), userID)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	fileIDs := make(map[string]int64)
	for rows.Next() {
		var key string
		var fileID int64
		if err := rows.Scan(&key, &fileID); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		fileIDs[key] = fileID
	}
	return fileIDs, stacktrace.Propagate(rows.Err(), "")
}

func affectsOne(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	count, err := res.RowsAffected()
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	return count == 1, nil
}
