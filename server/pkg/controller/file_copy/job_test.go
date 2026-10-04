package file_copy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil/fakes3"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func (f *copyFixture) enqueue(requestID string, fileIDs ...int64) (int64, error) {
	req := f.copyRequest(fileIDs...)
	req.RequestID = requestID
	resp, err := f.ctrl.EnqueueCopy(f.ginContext(f.t.Context(), ente.Drive), req)
	if err != nil {
		return 0, err
	}
	return resp.JobID, nil
}

func (f *copyFixture) mustEnqueue(fileIDs ...int64) int64 {
	f.t.Helper()
	jobID, err := f.enqueue("request-1", fileIDs...)
	require.NoError(f.t, err)
	return jobID
}

func (f *copyFixture) tryClaim() *repo.FileCopyJob {
	f.t.Helper()
	job, err := f.ctrl.JobRepo.Claim(f.t.Context(), uuid.NewString(), copyJobLease)
	require.NoError(f.t, err)
	return job
}

func (f *copyFixture) claim() repo.FileCopyJob {
	f.t.Helper()
	job := f.tryClaim()
	require.NotNil(f.t, job)
	return *job
}

func (f *copyFixture) runNextJob() {
	f.t.Helper()
	f.ctrl.processJob(f.t.Context(), f.claim())
}

func (f *copyFixture) jobStatus(jobID int64) *ente.CopyJobStatusResponse {
	f.t.Helper()
	resp, err := f.ctrl.GetCopyJob(f.t.Context(), actorID, jobID)
	require.NoError(f.t, err)
	return resp
}

func (f *copyFixture) requireJobError(jobID int64, code ente.ErrorCode) *ente.CopyJobStatusResponse {
	f.t.Helper()
	status := f.jobStatus(jobID)
	require.Equal(f.t, ente.CopyJobFailed, status.Status)
	require.NotNil(f.t, status.Error)
	require.Equal(f.t, code, status.Error.Code)
	return status
}

func (f *copyFixture) jobItem(jobID int64, fileID int64) copyJobItem {
	f.t.Helper()
	var raw []byte
	require.NoError(f.t, f.db.QueryRow(`SELECT items FROM file_copy_jobs WHERE id = $1`, jobID).Scan(&raw))
	var items []copyJobItem
	require.NoError(f.t, json.Unmarshal(raw, &items))
	for _, item := range items {
		if item.FileID == fileID {
			return item
		}
	}
	f.t.Fatalf("no item for file %d", fileID)
	return copyJobItem{}
}

func (f *copyFixture) jobCount() int {
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM file_copy_jobs`).Scan(&count))
	return count
}

func (f *copyFixture) jobCounters(jobID int64) (claims, attempts int) {
	require.NoError(f.t, f.db.QueryRow(`SELECT claims, attempts FROM file_copy_jobs WHERE id = $1`, jobID).Scan(&claims, &attempts))
	return claims, attempts
}

func (f *copyFixture) expireLease(jobID int64) {
	_, err := f.db.Exec(`UPDATE file_copy_jobs SET lease_until = now_utc_micro_seconds() - 1 WHERE id = $1`, jobID)
	require.NoError(f.t, err)
}

func (f *copyFixture) requireNoLiveReservation(wantRows int) {
	f.t.Helper()
	var live int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM temp_objects WHERE NOT reservation_released OR expiration_time > $1`,
		time.Microseconds()).Scan(&live))
	require.Zero(f.t, live)
	require.Equal(f.t, wantRows, f.tempRowCount())
}

// Leaves what a worker that died while copying the multipart file leaves: the
// other file committed and the multipart upload recorded but never aborted.
func (f *copyFixture) crashWhileCopyingParts() {
	f.t.Helper()
	stolen := make(chan struct{})
	var once sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		switch r.Op {
		case fakes3.OpPartCopy:
			once.Do(func() {
				defer close(stolen)
				for deadline := gotime.Now().Add(5 * gotime.Second); gotime.Now().Before(deadline); gotime.Sleep(10 * gotime.Millisecond) {
					var files int
					if err := f.db.QueryRow(`SELECT COUNT(*) FROM files WHERE owner_id = $1`, actorID).Scan(&files); err != nil || files == 1 {
						break
					}
				}
				if _, err := f.db.Exec(`UPDATE file_copy_jobs SET lease_token = NULL, lease_until = now_utc_micro_seconds() - 1`); err != nil {
					f.t.Error(err)
				}
			})
			<-r.Done
			return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
		case fakes3.OpAbort:
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
		}
		return nil
	})
	f.runNextJob()
	<-stolen
	f.fake.SetHook(nil)
}

func lowerHeartbeat(t *testing.T) {
	interval := copyJobHeartbeatInterval
	copyJobHeartbeatInterval = 10 * gotime.Millisecond
	t.Cleanup(func() { copyJobHeartbeatInterval = interval })
}

func TestAsyncCopyWithSameRequestIDCreatesOneJob(t *testing.T) {
	for _, storage := range []int64{100 * gib, gib} {
		t.Run(fmt.Sprint(storage), func(t *testing.T) {
			f := setupCopyTest(t, ente.Drive, storage)
			fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
			holder := holdQuotaLock(t, f.db, actorID)
			type result struct {
				jobID int64
				err   error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					jobID, err := f.enqueue("request-1", fileID)
					results <- result{jobID, err}
				}()
			}
			waitForAdvisoryLockWaiters(t, f.db, 2)
			require.NoError(t, holder.Rollback())
			first, second := <-results, <-results

			require.NoError(t, first.err)
			require.NoError(t, second.err)
			require.Equal(t, first.jobID, second.jobID)
			retried, err := f.enqueue("request-1", fileID)
			require.NoError(t, err)
			require.Equal(t, first.jobID, retried)
			require.Equal(t, 1, f.jobCount())
			require.Equal(t, 2, f.tempRowCount())
			require.Empty(t, f.fake.Requests())
			require.Equal(t, ente.CopyJobPending, f.jobStatus(retried).Status)
		})
	}
}

func TestAsyncCopyValidatesBeforeEnqueueing(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, gib)
	fileID := f.addSourceFile(ente.Drive, 600*mib, 1000)
	large := f.addSourceFile(ente.Drive, 600*mib, 1000)

	for _, requestID := range []string{"", strings.Repeat("r", maxCopyRequestIDLength+1)} {
		_, err := f.enqueue(requestID, fileID)
		requireAPIErrorCode(t, err, http.StatusBadRequest, ente.BadRequest)
	}
	_, err := f.enqueue("over-quota", fileID, large)
	status, _ := response(err)
	require.Equal(t, http.StatusUpgradeRequired, status)
	_, err = f.db.Exec(`UPDATE collection_shares SET is_deleted = TRUE`)
	require.NoError(t, err)
	_, err = f.enqueue("revoked", fileID)
	status, _ = response(err)
	require.Equal(t, http.StatusNotFound, status)

	require.Zero(t, f.jobCount())
	require.Zero(t, f.tempRowCount())
}

func TestAsyncCopyCapsUnfinishedJobsPerUser(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	var first int64
	for i := range maxUnfinishedCopyJobs {
		jobID, err := f.enqueue(fmt.Sprint("request-", i), fileID)
		require.NoError(t, err)
		if i == 0 {
			first = jobID
		}
	}
	_, err := f.db.Exec(`UPDATE file_copy_jobs SET status = 'running' WHERE id = $1`, first)
	require.NoError(t, err)

	_, err = f.enqueue("over-the-cap", fileID)
	requireAPIErrorCode(t, err, http.StatusTooManyRequests, ente.TooManyCopyJobs)
	require.Equal(t, maxUnfinishedCopyJobs, f.jobCount())
	require.Equal(t, 2*maxUnfinishedCopyJobs, f.tempRowCount())
	retried, err := f.enqueue("request-0", fileID)
	require.NoError(t, err)
	require.Equal(t, first, retried)

	_, err = f.db.Exec(`UPDATE file_copy_jobs SET status = 'completed' WHERE id = $1`, first)
	require.NoError(t, err)
	_, err = f.enqueue("over-the-cap", fileID)
	require.NoError(t, err)
}

func TestCopyJobCompletes(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	small := f.addSourceFile(ente.Drive, 3*mib, 1000)
	large := f.addSourceFile(ente.Drive, 22*mib, 1000)
	jobID := f.mustEnqueue(small, large)

	f.runNextJob()

	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Nil(t, status.Error)
	require.Len(t, status.OldToNewFileIDMap, 2)
	fileSize, _ := f.committedSizes(status.OldToNewFileIDMap[large])
	require.Equal(t, 22*mib, fileSize)
	require.Equal(t, 2, f.actorFileCount())
	require.Zero(t, f.tempRowCount())
	require.Empty(t, f.fake.Uploads())
}

func TestCopyJobWorkerRunsQueuedJobs(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	f.ctrl.StartJobWorker()
	t.Cleanup(func() { f.ctrl.StopJobWorker(5 * gotime.Second) })

	jobID := f.mustEnqueue(fileID)

	require.Eventually(t, func() bool {
		resp, err := f.ctrl.GetCopyJob(t.Context(), actorID, jobID)
		return err == nil && resp.Status == ente.CopyJobCompleted
	}, 10*gotime.Second, 20*gotime.Millisecond)
	f.ctrl.StopJobWorker(5 * gotime.Second)
	require.Equal(t, 1, f.actorFileCount())
}

func TestCopyJobResumesAfterCrash(t *testing.T) {
	lowerCopyThresholds(t)
	lowerHeartbeat(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	small := f.addSourceFile(ente.Drive, 3*mib, 1000)
	large := f.addSourceFile(ente.Drive, 22*mib, 1000)
	jobID := f.mustEnqueue(small, large)
	largeKey := f.jobItem(jobID, large).FileObjectKey
	f.fake.StartUpload(largeKey)

	f.crashWhileCopyingParts()

	require.Equal(t, ente.CopyJobRunning, f.jobStatus(jobID).Status)
	require.Equal(t, 1, f.actorFileCount())
	require.Len(t, f.fake.Uploads(), 2)
	row, err := f.tempRow(largeKey)
	require.NoError(t, err)
	require.True(t, row.uploadID.Valid)

	f.runNextJob()

	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Len(t, status.OldToNewFileIDMap, 2)
	require.Equal(t, 2, f.actorFileCount())
	require.Empty(t, f.fake.Uploads())
	require.Zero(t, f.tempRowCount())
}

func TestResumedCopyJobFailsWhenAccessIsRevoked(t *testing.T) {
	lowerCopyThresholds(t)
	lowerHeartbeat(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	small := f.addSourceFile(ente.Drive, 3*mib, 1000)
	large := f.addSourceFile(ente.Drive, 22*mib, 1000)
	jobID := f.mustEnqueue(small, large)
	f.crashWhileCopyingParts()
	_, err := f.db.Exec(`UPDATE collection_shares SET is_deleted = TRUE`)
	require.NoError(t, err)

	f.runNextJob()

	status := f.requireJobError(jobID, ente.PermissionDenied)
	require.Len(t, status.OldToNewFileIDMap, 1)
	require.Contains(t, status.OldToNewFileIDMap, small)
	require.Equal(t, 1, f.actorFileCount())
	f.requireReleasedForDelayedCleanup(2)
}

func TestCopyJobRetriesTransientErrors(t *testing.T) {
	backoff := copyJobRetryBackoff
	copyJobRetryBackoff = 0
	t.Cleanup(func() { copyJobRetryBackoff = backoff })
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	var once sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		var failure *fakes3.Failure
		if r.Op == fakes3.OpCopy {
			once.Do(func() { failure = &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"} })
		}
		return failure
	})

	f.runNextJob()

	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobPending, status.Status)
	require.Nil(t, status.Error)
	require.Equal(t, 2, f.tempRowCount())
	f.runNextJob()
	status = f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Len(t, status.OldToNewFileIDMap, 1)
	claims, attempts := f.jobCounters(jobID)
	require.Equal(t, 2, claims)
	require.Equal(t, 2, attempts)
}

func TestCopyJobFailsWhenItsSourceChanges(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change string
		code   ente.ErrorCode
	}{
		{"resized", `UPDATE object_keys SET size = size + 1 WHERE file_id = $1 AND o_type = 'file'`, ente.CopySourceChanged},
		{"removed from the collection", `UPDATE collection_files SET is_deleted = TRUE WHERE file_id = $1`, ente.NotFoundError},
		{"deleted", `DELETE FROM object_keys WHERE file_id = $1 AND o_type = 'thumbnail'`, ente.NotFoundError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setupCopyTest(t, ente.Drive, 100*gib)
			fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
			jobID := f.mustEnqueue(fileID)
			_, err := f.db.Exec(tt.change, fileID)
			require.NoError(t, err)

			f.runNextJob()

			f.requireJobError(jobID, tt.code)
			require.Empty(t, f.fake.Requests())
			f.requireNoLiveReservation(2)
		})
	}
}

func TestCopyJobFailsWithoutItsReservation(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	_, err := f.db.Exec(`UPDATE temp_objects SET reservation_released = TRUE`)
	require.NoError(t, err)

	f.runNextJob()

	f.requireJobError(jobID, ente.CopyReservationLost)
	require.Empty(t, f.fake.Requests())
	require.Zero(t, f.actorFileCount())
}

func TestCopyJobFailsWhenAccessIsRevoked(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	_, err := f.db.Exec(`UPDATE collection_shares SET is_deleted = TRUE`)
	require.NoError(t, err)

	f.runNextJob()

	status := f.requireJobError(jobID, ente.PermissionDenied)
	require.Empty(t, status.OldToNewFileIDMap)
	require.Empty(t, f.fake.Requests())
	require.Zero(t, f.actorFileCount())
	f.requireNoLiveReservation(2)
}

func TestCopyJobFailsAfterTooManyAttempts(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	_, err := f.db.Exec(`UPDATE file_copy_jobs SET attempts = $1, claims = $1`, maxCopyJobAttempts)
	require.NoError(t, err)

	f.runNextJob()

	f.requireJobError(jobID, ente.CopyAttemptsExceeded)
	f.requireReleasedForDelayedCleanup(2)
}

func TestCopyJobPastTheCapCompletesWhenEverythingIsCommitted(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	f.runNextJob()
	_, err := f.db.Exec(`UPDATE file_copy_jobs SET status = 'pending', result = NULL, attempts = $1`, maxCopyJobAttempts)
	require.NoError(t, err)

	f.runNextJob()

	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Len(t, status.OldToNewFileIDMap, 1)
	require.Equal(t, 1, f.actorFileCount())
}

func TestPanickingCopyJobFailsWithoutRetrying(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	f.ctrl.FileController = nil

	f.runNextJob()

	status := f.requireJobError(jobID, ente.InternalError)
	require.Equal(t, internalCopyJobError, *status.Error)
	f.requireReleasedForDelayedCleanup(2)
}

func TestStolenLeaseStopsTheOldWorker(t *testing.T) {
	lowerHeartbeat(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	old := f.claim()
	f.expireLease(jobID)
	current := f.claim()
	require.Equal(t, 2, current.Claims)

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stop := f.ctrl.keepLease(ctx, cancel, old, logrus.NewEntry(logrus.StandardLogger()))
	<-ctx.Done()
	stop()
	require.ErrorIs(t, context.Cause(ctx), errLeaseLost)
	owned, err := f.ctrl.completeJob(t.Context(), old, map[int64]int64{})
	require.NoError(t, err)
	require.False(t, owned)
	owned, err = f.ctrl.failJob(t.Context(), old, []copyJobItem{f.jobItem(jobID, fileID)}, nil, false, internalCopyJobError)
	require.NoError(t, err)
	require.False(t, owned)
	for _, write := range []func(context.Context, int64, string) (bool, error){
		f.ctrl.JobRepo.Yield,
		func(ctx context.Context, id int64, token string) (bool, error) {
			return f.ctrl.JobRepo.Retry(ctx, id, token, 0)
		},
	} {
		owned, err = write(t.Context(), old.ID, old.LeaseToken)
		require.NoError(t, err)
		require.False(t, owned)
	}

	require.Equal(t, ente.CopyJobRunning, f.jobStatus(jobID).Status)
	var live int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM temp_objects WHERE NOT reservation_released`).Scan(&live))
	require.Equal(t, 2, live)
	f.ctrl.processJob(t.Context(), current)
	require.Equal(t, ente.CopyJobCompleted, f.jobStatus(jobID).Status)
}

func TestLeaseEndsWhenItCantBeExtendedInTime(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	f.mustEnqueue(fileID)
	job := f.claim()
	job.LeaseStart = gotime.Now().Add(-copyJobLease)

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stop := f.ctrl.keepLease(ctx, cancel, job, logrus.NewEntry(logrus.StandardLogger()))
	<-ctx.Done()
	stop()

	require.ErrorIs(t, context.Cause(ctx), errLeaseLost)
}

func TestInterruptedCopyJobGoesBackToTheQueue(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)
	ctx, cancel := context.WithCancel(t.Context())
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		cancel()
		<-r.Done
		return nil
	})

	f.ctrl.processJob(ctx, f.claim())

	require.Equal(t, ente.CopyJobPending, f.jobStatus(jobID).Status)
	claims, attempts := f.jobCounters(jobID)
	require.Equal(t, 1, claims)
	require.Zero(t, attempts)
	f.fake.SetHook(nil)
	f.runNextJob()
	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Len(t, status.OldToNewFileIDMap, 1)
}

func TestCopyJobsOfAUserDontBlockOtherUsers(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	for i, userID := range []int64{actorID, actorID, sharerID} {
		_, err := f.db.Exec(`INSERT INTO file_copy_jobs (user_id, request_id, src_collection_id, dst_collection_id, items)
			VALUES ($1, $2, 1, 2, '[]')`, userID, fmt.Sprint(i))
		require.NoError(t, err)
	}

	first := f.claim()
	second := f.claim()

	require.Equal(t, actorID, first.UserID)
	require.Equal(t, sharerID, second.UserID)
	require.Nil(t, f.tryClaim())
	f.expireLease(first.ID)
	require.Equal(t, first.ID, f.claim().ID)
}

func TestCopyJobIsOnlyVisibleToItsOwner(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	fileID := f.addSourceFile(ente.Drive, 3*mib, 1000)
	jobID := f.mustEnqueue(fileID)

	for _, tt := range []struct{ userID, jobID int64 }{{sharerID, jobID}, {actorID, jobID + 1}} {
		_, err := f.ctrl.GetCopyJob(t.Context(), tt.userID, tt.jobID)
		status, body := response(err)
		require.Equal(t, http.StatusNotFound, status)
		require.Contains(t, body, string(ente.NotFoundError))
	}
}

func TestFinishedCopyJobsAreDeletedAfterRetention(t *testing.T) {
	f := setupCopyTest(t, ente.Drive, 100*gib)
	old := time.Microseconds() - copyJobRetention.Microseconds() - 1
	insert := func(requestID string, status ente.CopyJobStatus, updatedAt int64) {
		_, err := f.db.Exec(`INSERT INTO file_copy_jobs (user_id, request_id, src_collection_id, dst_collection_id, items, status, updated_at)
			VALUES ($1, $2, 1, 2, '[]', $3, $4)`, actorID, requestID, status, updatedAt)
		require.NoError(t, err)
	}
	for i, status := range []ente.CopyJobStatus{ente.CopyJobPending, ente.CopyJobRunning, ente.CopyJobCompleted, ente.CopyJobFailed} {
		insert(fmt.Sprint(i), status, old)
	}
	insert("recent", ente.CopyJobCompleted, time.Microseconds())

	f.ctrl.deleteFinishedJobs(t.Context())

	rows, err := f.db.Query(`SELECT request_id FROM file_copy_jobs ORDER BY request_id`)
	require.NoError(t, err)
	defer rows.Close()
	var remaining []string
	for rows.Next() {
		var requestID string
		require.NoError(t, rows.Scan(&requestID))
		remaining = append(remaining, requestID)
	}
	require.Equal(t, []string{"0", "1", "recent"}, remaining)
}

// The client lists its pending uploads to resume or abort them; a copy's
// reservations must not be among them.
func (f *copyFixture) requireNotTheClientsUploads(keys ...string) {
	f.t.Helper()
	ctx := context.Background()
	pending, err := f.ctrl.FileController.GetPendingDriveUploads(ctx, actorID, "")
	require.NoError(f.t, err)
	require.Empty(f.t, pending.Uploads)
	for _, key := range keys {
		before, err := f.tempRow(key)
		require.NoError(f.t, err)
		_, err = f.ctrl.FileController.ResumeMultipartUpload(ctx, actorID, key)
		requireAPIErrorCode(f.t, err, http.StatusNotFound, ente.NotFoundError)
		err = f.ctrl.FileController.AbortMultipartUpload(ctx, actorID, key)
		requireAPIErrorCode(f.t, err, http.StatusNotFound, ente.NotFoundError)
		after, err := f.tempRow(key)
		require.NoError(f.t, err)
		require.Equal(f.t, before, after, key)
	}
}

func TestCopyReservationsAreNotTheClientsUploads(t *testing.T) {
	lowerCopyThresholds(t)
	f := setupCopyTest(t, ente.Drive, 100*gib)
	small := f.addSourceFile(ente.Drive, 3*mib, 1000)
	large := f.addSourceFile(ente.Drive, 22*mib, 1000)
	jobID := f.mustEnqueue(small, large)
	var keys []string
	for _, fileID := range []int64{small, large} {
		item := f.jobItem(jobID, fileID)
		keys = append(keys, item.FileObjectKey, item.ThumbObjectKey)
	}
	largeKey := f.jobItem(jobID, large).FileObjectKey
	f.requireNotTheClientsUploads(keys...)

	var once sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy && r.Key == largeKey {
			once.Do(func() {
				row, err := f.tempRow(largeKey)
				require.NoError(t, err)
				require.True(t, row.uploadID.Valid)
				f.requireNotTheClientsUploads(largeKey)
			})
		}
		return nil
	})
	f.runNextJob()
	f.fake.SetHook(nil)

	status := f.jobStatus(jobID)
	require.Equal(t, ente.CopyJobCompleted, status.Status)
	require.Len(t, status.OldToNewFileIDMap, 2)
	require.Equal(t, 2, f.actorFileCount())
	require.Empty(t, f.fake.Uploads())
	require.Empty(t, f.fake.RequestsOf(fakes3.OpAbort))

	syncSource := f.addSourceFile(ente.Drive, 22*mib, 1000)
	var syncOnce sync.Once
	f.fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy {
			syncOnce.Do(func() { f.requireNotTheClientsUploads(r.Key) })
		}
		return nil
	})
	resp, err := f.copy(ente.Drive, syncSource)
	require.NoError(t, err)
	require.Len(t, resp.OldToNewFileIDMap, 1)
	require.Zero(t, f.tempRowCount())
}
