package file_copy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/auth"
	"github.com/ente/museum/pkg/utils/network"
	enteTime "github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

const (
	maxCopyRequestIDLength = 64
	maxConcurrentCopyJobs  = 2
	maxUnfinishedCopyJobs  = 10
	maxCopyJobAttempts     = 5
	maxCopyJobRetryBackoff = 30 * time.Minute
	copyJobRetention       = 7 * 24 * time.Hour
	copyJobWriteTimeout    = 30 * time.Second
)

var (
	copyJobLease             = 2 * time.Minute
	copyJobHeartbeatInterval = 30 * time.Second
	copyJobPollInterval      = 5 * time.Second
	copyJobRetentionInterval = time.Hour
	copyJobRetryBackoff      = time.Minute

	errLeaseLost         = errors.New("copy job lease lost")
	internalCopyJobError = ente.CopyJobError{Code: ente.InternalError, Message: "copy failed"}
)

type copyJobItem struct {
	FileID             int64  `json:"fileID"`
	EncryptedKey       string `json:"encryptedKey"`
	KeyDecryptionNonce string `json:"keyDecryptionNonce"`
	FileObjectKey      string `json:"fileObjectKey"`
	ThumbObjectKey     string `json:"thumbObjectKey"`
	FileSize           int64  `json:"fileSize"`
	ThumbSize          int64  `json:"thumbSize"`
}

type jobWorker struct {
	wake   chan struct{}
	slots  chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// The client gets the synchronous Drive copy's errors before the job is queued.
func (fc *FileCopyController) EnqueueCopy(c *gin.Context, req ente.CopyFileSyncRequest) (*ente.CopyJobResponse, error) {
	userID := auth.GetUserID(c.Request.Header)
	if req.RequestID == "" || len(req.RequestID) > maxCopyRequestIDLength {
		return nil, ente.NewBadRequestWithMessage(fmt.Sprintf("requestID must have 1 to %d characters", maxCopyRequestIDLength))
	}
	if len(req.CollectionFileItems) == 0 {
		return nil, ente.NewBadRequestWithMessage("files can't be empty")
	}
	ctx := c.Request.Context()
	jobID, found, err := fc.JobRepo.GetIDByRequestID(ctx, userID, req.RequestID)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	if found {
		return &ente.CopyJobResponse{JobID: jobID}, nil
	}
	_, objects, err := fc.checkCopy(c, userID, ente.Drive, req)
	if err != nil {
		return nil, err
	}
	if err := fc.checkDriveFileSizes(ctx, userID, objects); err != nil {
		return nil, err
	}
	fileCopyList, err := fc.buildCopyList(req, objects, newObjectKeys(userID, len(objects)))
	if err != nil {
		return nil, err
	}
	items, err := json.Marshal(newCopyJobItems(fileCopyList))
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	job := repo.FileCopyJob{
		UserID:          userID,
		RequestID:       req.RequestID,
		SrcCollectionID: req.SrcCollectionID,
		DstCollectionID: req.DstCollection,
		Items:           items,
	}
	objectsToReserve := fc.driveTempObjects(userID, network.GetClientInfo(c), fileCopyList)
	err = fc.FileController.ReserveDriveUploadsWith(ctx, userID, objectsToReserve, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		jobID, err = fc.JobRepo.InsertTx(ctx, tx, job, maxUnfinishedCopyJobs)
		return err
	})
	if err != nil {
		// A concurrent retry may have queued it first; its reservation can
		// make this one fail the quota check.
		if existingID, found, lookupErr := fc.JobRepo.GetIDByRequestID(ctx, userID, req.RequestID); lookupErr == nil && found {
			return &ente.CopyJobResponse{JobID: existingID}, nil
		}
		return nil, stacktrace.Propagate(err, "")
	}
	fc.wakeJobWorker()
	return &ente.CopyJobResponse{JobID: jobID}, nil
}

func newCopyJobItems(fileCopyList []fileCopyInternal) []copyJobItem {
	items := make([]copyJobItem, 0, len(fileCopyList))
	for _, fileCopy := range fileCopyList {
		items = append(items, copyJobItem{
			FileID:             fileCopy.SourceFile.ID,
			EncryptedKey:       fileCopy.EncryptedFileKey,
			KeyDecryptionNonce: fileCopy.EncryptedFileKeyNonce,
			FileObjectKey:      fileCopy.FileCopyReq.DestObjectKey,
			ThumbObjectKey:     fileCopy.ThumbCopyReq.DestObjectKey,
			FileSize:           fileCopy.FileCopyReq.SourceS3Object.FileSize,
			ThumbSize:          fileCopy.ThumbCopyReq.SourceS3Object.FileSize,
		})
	}
	return items
}

func (fc *FileCopyController) GetCopyJob(ctx context.Context, userID int64, jobID int64) (*ente.CopyJobStatusResponse, error) {
	job, err := fc.JobRepo.Get(ctx, jobID, userID)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	resp := &ente.CopyJobStatusResponse{JobID: jobID, Status: job.Status}
	if len(job.Result) > 0 {
		if err := json.Unmarshal(job.Result, &resp.OldToNewFileIDMap); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
	}
	if len(job.Error) > 0 {
		resp.Error = &ente.CopyJobError{}
		if err := json.Unmarshal(job.Error, resp.Error); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
	}
	return resp, nil
}

func (fc *FileCopyController) StartJobWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	w := &jobWorker{wake: make(chan struct{}, 1), slots: make(chan struct{}, maxConcurrentCopyJobs), cancel: cancel}
	fc.worker = w
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		fc.runJobWorker(ctx, w)
	}()
}

// Interrupted jobs go back to the queue; those still stopping after the
// timeout resume elsewhere once their lease expires.
func (fc *FileCopyController) StopJobWorker(timeout time.Duration) {
	w := fc.worker
	if w == nil {
		return
	}
	w.cancel()
	stopped := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(timeout):
	}
}

func (fc *FileCopyController) wakeJobWorker() {
	if w := fc.worker; w != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (fc *FileCopyController) runJobWorker(ctx context.Context, w *jobWorker) {
	ticker := time.NewTicker(copyJobPollInterval)
	defer ticker.Stop()
	var lastRetention time.Time
	for {
		if time.Since(lastRetention) >= copyJobRetentionInterval {
			fc.deleteFinishedJobs(ctx)
			lastRetention = time.Now()
		}
		fc.claimJobs(ctx, w)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

func (fc *FileCopyController) claimJobs(ctx context.Context, w *jobWorker) {
	for ctx.Err() == nil {
		select {
		case w.slots <- struct{}{}:
		default:
			return
		}
		job, err := fc.JobRepo.Claim(ctx, uuid.NewString(), copyJobLease)
		if err != nil || job == nil {
			<-w.slots
			if err != nil && ctx.Err() == nil {
				logrus.WithError(err).Error("Failed to claim a copy job")
			}
			return
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			fc.processJob(ctx, *job)
			<-w.slots
			fc.wakeJobWorker()
		}()
	}
}

func (fc *FileCopyController) deleteFinishedJobs(ctx context.Context) {
	deleted, err := fc.JobRepo.DeleteFinishedBefore(ctx, time.Now().Add(-copyJobRetention).UnixMicro())
	if err != nil {
		if ctx.Err() == nil {
			logrus.WithError(err).Error("Failed to delete finished copy jobs")
		}
		return
	}
	if deleted > 0 {
		logrus.WithField("count", deleted).Info("Deleted finished copy jobs")
	}
}

func (fc *FileCopyController) processJob(workerCtx context.Context, job repo.FileCopyJob) {
	logger := logrus.WithFields(logrus.Fields{"copy_job_id": job.ID, "user_id": job.UserID, "attempt": job.Attempts})
	ctx, cancel := context.WithCancelCause(workerCtx)
	defer cancel(nil)
	stopLease := fc.keepLease(ctx, cancel, job, logger)
	items, result, started, err := fc.runCopyJobRecovering(ctx, job)
	stopLease()

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), copyJobWriteTimeout)
	defer cancelWrite()
	var owned bool
	var writeErr error
	switch {
	case errors.Is(context.Cause(ctx), errLeaseLost):
		logger.Warn("Stopped a copy job whose lease was lost")
		return
	case err == nil:
		owned, writeErr = fc.completeJob(writeCtx, job, result)
	case workerCtx.Err() != nil:
		owned, writeErr = fc.JobRepo.Yield(writeCtx, job.ID, job.LeaseToken)
	default:
		jobErr, retryable := classifyCopyJobError(err)
		entry := logger.WithError(err).WithField("code", jobErr.Code)
		if retryable && job.Attempts < maxCopyJobAttempts {
			entry.Warn("Copy job failed, retrying later")
			owned, writeErr = fc.JobRepo.Retry(writeCtx, job.ID, job.LeaseToken, retryBackoff(job.Attempts))
			break
		}
		if retryable {
			jobErr = ente.CopyJobError{Code: ente.ErrCopyAttemptsExceeded.Code, Message: ente.ErrCopyAttemptsExceeded.Message}
		}
		if jobErr.Code == ente.InternalError || jobErr.Code == ente.CopyAttemptsExceeded {
			entry.Error("Copy job failed")
		} else {
			entry.Warn("Copy job failed")
		}
		owned, writeErr = fc.failJob(writeCtx, job, items, result, started, jobErr)
	}
	if writeErr != nil {
		logger.WithError(writeErr).Error("Failed to record the outcome of a copy job")
	} else if !owned {
		logger.Warn("Copy job was claimed by another worker before its outcome was recorded")
	}
}

func retryBackoff(attempt int) time.Duration {
	return min(copyJobRetryBackoff<<max(attempt-1, 0), maxCopyJobRetryBackoff)
}

// A panicking job fails instead of taking down museum for every app.
func (fc *FileCopyController) runCopyJobRecovering(ctx context.Context, job repo.FileCopyJob) (items []copyJobItem, result map[int64]int64, started bool, err error) {
	result = map[int64]int64{}
	defer func() {
		if r := recover(); r != nil {
			started = true
			err = stacktrace.Propagate(errCopyPanicked, "%v\n%s", r, debug.Stack())
		}
	}()
	if err := json.Unmarshal(job.Items, &items); err != nil {
		return items, result, false, stacktrace.Propagate(err, "")
	}
	result, started, err = fc.runCopyJob(ctx, job, items)
	return items, result, started, err
}

// The lease is measured from before the claim, so it never ends later here
// than in the DB, and a heartbeat stuck on the DB can't outlive it.
func (fc *FileCopyController) keepLease(ctx context.Context, cancel context.CancelCauseFunc, job repo.FileCopyJob, logger *logrus.Entry) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(copyJobHeartbeatInterval)
		defer ticker.Stop()
		leaseEnd := job.LeaseStart.Add(copyJobLease)
		expiry := time.NewTimer(time.Until(leaseEnd))
		defer expiry.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-expiry.C:
				cancel(errLeaseLost)
				return
			case <-ticker.C:
			}
			start := time.Now()
			callCtx, cancelCall := context.WithDeadline(ctx, leaseEnd)
			owned, err := fc.JobRepo.ExtendLease(callCtx, job.ID, job.LeaseToken, copyJobLease)
			cancelCall()
			switch {
			case err == nil && owned:
				leaseEnd = start.Add(copyJobLease)
				expiry.Reset(time.Until(leaseEnd))
			case err == nil || !time.Now().Before(leaseEnd):
				cancel(errLeaseLost)
				return
			default:
				logger.WithError(err).Warn("Failed to extend the lease of a copy job")
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// started reports whether this claim may have started a copy on the provider.
func (fc *FileCopyController) runCopyJob(ctx context.Context, job repo.FileCopyJob, items []copyJobItem) (result map[int64]int64, started bool, err error) {
	result = make(map[int64]int64, len(items))
	fileKeys := make([]string, 0, len(items))
	for _, item := range items {
		fileKeys = append(fileKeys, item.FileObjectKey)
	}
	committed, err := fc.JobRepo.GetCommittedFileIDsByObjectKey(ctx, job.UserID, fileKeys)
	if err != nil {
		return result, false, stacktrace.Propagate(err, "")
	}
	for _, item := range items {
		if fileID, ok := committed[item.FileObjectKey]; ok {
			result[item.FileID] = fileID
		}
	}
	if len(result) == len(items) {
		return result, false, nil
	}
	if job.Attempts > maxCopyJobAttempts {
		return result, false, stacktrace.Propagate(ente.ErrCopyAttemptsExceeded, "")
	}
	fileCopyList, err := fc.loadCopyJob(ctx, job, items)
	if err != nil {
		return result, false, err
	}
	pending := make([]fileCopyInternal, 0, len(fileCopyList))
	for _, fileCopy := range fileCopyList {
		if _, ok := result[fileCopy.SourceFile.ID]; !ok {
			pending = append(pending, fileCopy)
		}
	}
	if err := fc.prepareReservedObjects(ctx, job, destObjectKeys(pending)); err != nil {
		return result, false, err
	}
	info := controller.RequestInfo{RequestID: fmt.Sprintf("copy-job-%d", job.ID)}
	create := func(ctx context.Context, file ente.File) (ente.File, error) {
		ctx, cancel := context.WithTimeout(ctx, driveMetadataTimeout)
		defer cancel()
		return fc.FileController.CreateWithContext(ctx, job.UserID, file, info, ente.Drive)
	}
	copied, err := fc.runDriveCopies(ctx, job.UserID, pending, create)
	maps.Copy(result, copied)
	return result, true, err
}

// Access is checked again: the sharer may have revoked it since the enqueue.
func (fc *FileCopyController) loadCopyJob(ctx context.Context, job repo.FileCopyJob, items []copyJobItem) ([]fileCopyInternal, error) {
	req := ente.CopyFileSyncRequest{SrcCollectionID: job.SrcCollectionID, DstCollection: job.DstCollectionID}
	itemsByFileID := make(map[int64]copyJobItem, len(items))
	for _, item := range items {
		itemsByFileID[item.FileID] = item
		req.CollectionFileItems = append(req.CollectionFileItems, ente.CollectionFileItem{
			ID: item.FileID, EncryptedKey: item.EncryptedKey, KeyDecryptionNonce: item.KeyDecryptionNonce,
		})
	}
	_, objects, complete, err := fc.loadCopySources(ctx, job.UserID, ente.Drive, req)
	switch {
	case errors.Is(err, repo.ErrFileNotInCollection) || (err == nil && !complete):
		return nil, stacktrace.Propagate(ente.ErrNotFoundError.NewErr("a source file is no longer available"), "%v", err)
	case errors.Is(err, sql.ErrNoRows) || errors.Is(err, ente.ErrNotFound) || errors.Is(err, ente.ErrPermissionDenied):
		return nil, stacktrace.Propagate(ente.NewPermissionDeniedError("access to a collection of the copy was lost"), "%v", err)
	case err != nil:
		return nil, err
	}
	destKeys := make([]string, len(objects))
	for i, obj := range objects {
		item := itemsByFileID[obj.FileID]
		destKey, expectedSize := item.ThumbObjectKey, item.ThumbSize
		if obj.Type == ente.FILE {
			destKey, expectedSize = item.FileObjectKey, item.FileSize
		}
		destKeys[i] = destKey
		if obj.FileSize != expectedSize {
			return nil, stacktrace.Propagate(ente.ErrCopySourceChanged, "%s has %d bytes, expected %d", obj.ObjectKey, obj.FileSize, expectedSize)
		}
	}
	if err := fc.checkDriveFileSizes(ctx, job.UserID, objects); err != nil {
		return nil, err
	}
	return fc.buildCopyList(req, objects, destKeys)
}

// Never copies without a reservation. A resumed job reuses its rows: uploads
// an earlier claim started, recorded or not, are aborted first, so none is
// left on the provider.
func (fc *FileCopyController) prepareReservedObjects(ctx context.Context, job repo.FileCopyJob, keys []string) error {
	cleanupRepo := fc.FileController.ObjectCleanupRepo
	live, err := cleanupRepo.GetLiveTempObjects(ctx, job.UserID, keys, enteTime.Microseconds())
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	for _, key := range keys {
		if _, ok := live[key]; !ok {
			return stacktrace.Propagate(ente.ErrCopyReservationLost, "no live reservation for %s", key)
		}
	}
	if job.Claims == 1 {
		return nil
	}
	for _, key := range keys {
		row := live[key]
		if err := fc.abortEarlierUploads(ctx, row); err != nil {
			return stacktrace.Propagate(err, "")
		}
		if row.UploadID == "" {
			continue
		}
		err := cleanupRepo.ResetTempObjectUpload(ctx, key, row.UploadID, enteTime.Microseconds())
		if errors.Is(err, ente.ErrUploadGone) {
			return stacktrace.Propagate(ente.ErrCopyReservationLost, "%v", err)
		}
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	return nil
}

func (fc *FileCopyController) abortEarlierUploads(ctx context.Context, row ente.TempObject) error {
	ctx, cancel := context.WithTimeout(ctx, driveMetadataTimeout)
	defer cancel()
	cleanupCtrl := fc.FileController.ObjectCleanupCtrl
	if row.UploadID != "" {
		if err := cleanupCtrl.AbortMultipartUploadWithContext(ctx, row.ObjectKey, row.UploadID, row.BucketId); err != nil {
			return err
		}
	}
	if row.IsMultipart || row.PartLength != nil {
		return cleanupCtrl.AbortMultipartUploadsForKey(ctx, row.ObjectKey, row.BucketId)
	}
	return nil
}

func (fc *FileCopyController) completeJob(ctx context.Context, job repo.FileCopyJob, result map[int64]int64) (bool, error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	return fc.JobRepo.Complete(ctx, job.ID, job.LeaseToken, resultJSON)
}

// Rows expire at once only if no claim can have reached the provider;
// otherwise they wait like a failed synchronous copy, since a copy may still
// finish there.
func (fc *FileCopyController) failJob(ctx context.Context, job repo.FileCopyJob, items []copyJobItem, result map[int64]int64,
	started bool, jobErr ente.CopyJobError) (bool, error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	errJSON, err := json.Marshal(jobErr)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	keys := make([]string, 0, 2*len(items))
	for _, item := range items {
		keys = append(keys, item.FileObjectKey, item.ThumbObjectKey)
	}
	expiry := time.Now()
	if started || job.Claims > 1 {
		expiry = expiry.Add(failedDriveCopyCleanupDelay)
	}
	return fc.JobRepo.Fail(ctx, job, resultJSON, errJSON, keys, expiry.UnixMicro())
}

// Server-side failures are retried; typed client-facing ones are final.
func classifyCopyJobError(err error) (jobErr ente.CopyJobError, retryable bool) {
	if errors.Is(err, errCopyPanicked) {
		return internalCopyJobError, false
	}
	var apiErr *ente.ApiError
	if errors.As(err, &apiErr) {
		if apiErr.Code == ente.InternalError {
			return internalCopyJobError, true
		}
		return ente.CopyJobError{Code: apiErr.Code, Message: apiErr.Message},
			apiErr.HttpStatusCode >= 500 && apiErr.Code != ente.CopyAttemptsExceeded
	}
	for _, known := range []struct {
		err  error
		code ente.ErrorCode
	}{
		{ente.ErrPermissionDenied, ente.PermissionDenied},
		{ente.ErrNotFound, ente.NotFoundError},
		{sql.ErrNoRows, ente.NotFoundError},
		{ente.ErrStorageLimitExceeded, ente.StorageLimitExceeded},
		{ente.ErrNoActiveSubscription, ente.NoActiveSubscription},
		{ente.ErrFileTooLarge, ente.FileTooLarge},
		{ente.ErrBadRequest, ente.BadRequest},
		{ente.ErrInvalidApp, ente.BadRequest},
	} {
		if errors.Is(err, known.err) {
			return ente.CopyJobError{Code: known.code, Message: known.err.Error()}, false
		}
	}
	return internalCopyJobError, true
}
