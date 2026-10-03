package controller

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/pkg/repo"
	timeUtil "github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	log "github.com/sirupsen/logrus"
)

const (
	orphanSweepInterval = 6 * time.Hour
	orphanSweepTimeout  = time.Hour
	orphanSweepLock     = "replication_orphan_upload_sweep"
	abandonedRowsBatch  = 1000
)

func (c *ReplicationController3) startOrphanSweeper() {
	go func() {
		delay := 5*time.Minute + rand.N(25*time.Minute)
		for {
			time.Sleep(delay)
			if c.stopping.Load() {
				return
			}
			c.sweepOrphanUploads(context.Background())
			delay = orphanSweepInterval
		}
	}()
}

func (c *ReplicationController3) sweepOrphanUploads(ctx context.Context) {
	logger := log.WithField("task", "replication-orphan-sweep")
	if !c.LockController.TryLock(orphanSweepLock, timeUtil.MicrosecondsAfterHours(2)) {
		logger.Info("Skipping sweep, another instance is running it")
		return
	}
	defer c.LockController.ReleaseLock(orphanSweepLock)
	ctx, cancel := context.WithTimeout(ctx, orphanSweepTimeout)
	defer cancel()
	cutoff := time.Now().Add(-c.stream.orphanUploadAge)
	for _, dest := range []*UploadDestination{c.wasabiDest, c.scwDest} {
		if *dest.Bucket == "" {
			continue
		}
		destLogger := logger.WithField("destination", dest.Label)
		aborted, err := c.sweepDestination(ctx, dest, cutoff, destLogger)
		if err != nil {
			destLogger.WithError(err).Warn("Orphaned replication upload sweep failed")
		}
		if aborted > 0 {
			destLogger.Infof("Aborted %d orphaned replication uploads", aborted)
		}
	}
	c.dropAbandonedUploadRows(ctx, cutoff, logger)
}

func (c *ReplicationController3) sweepDestination(ctx context.Context, dest *UploadDestination, cutoff time.Time, logger *log.Entry) (int, error) {
	type orphan struct {
		key, uploadID string
		stored        *repo.ReplicationUpload
	}
	var orphans []orphan
	err := forEachMultipartUpload(ctx, dest.Client, dest.Bucket, nil, func(upload *s3.MultipartUpload) error {
		key, uploadID := aws.StringValue(upload.Key), aws.StringValue(upload.UploadId)
		if upload.Initiated == nil || upload.Initiated.After(cutoff) || key == "" || uploadID == "" {
			return nil
		}
		isOrphan, stored, err := c.isOrphanUpload(ctx, key, uploadID)
		if err != nil {
			return err
		}
		if isOrphan {
			orphans = append(orphans, orphan{key, uploadID, stored})
		}
		return nil
	})
	aborted := 0
	for _, o := range orphans {
		if !c.abortUpload(ctx, dest, o.key, o.uploadID, logger) {
			continue
		}
		if o.stored != nil {
			c.deleteUploadRow(ctx, *o.stored, logger)
		}
		aborted++
	}
	return aborted, err
}

// Rows are matched by upload ID alone: both destinations may share a bucket.
// Uploads without a row are only ours if the object takes the streaming path;
// legacy uploads for other objects are never touched.
func (c *ReplicationController3) isOrphanUpload(ctx context.Context, objectKey string, uploadID string) (bool, *repo.ReplicationUpload, error) {
	stored, err := c.ReplicationUploadsRepo.GetByUploadID(ctx, objectKey, uploadID)
	if err != nil {
		return false, nil, stacktrace.Propagate(err, "")
	}
	if stored != nil {
		copies, err := c.ObjectCopiesRepo.Get(ctx, objectKey)
		if err != nil {
			return false, nil, stacktrace.Propagate(err, "")
		}
		return !c.needsReplica(copies, c.destination(stored.DestDC)), stored, nil
	}
	size, app, found, err := c.ObjectRepo.GetObjectSizeAndApp(ctx, objectKey)
	if err != nil {
		return false, nil, stacktrace.Propagate(err, "")
	}
	return found && c.isStreamingEligible(size, app), nil, nil
}

func (c *ReplicationController3) dropAbandonedUploadRows(ctx context.Context, cutoff time.Time, logger *log.Entry) {
	uploads, err := c.ReplicationUploadsRepo.ListAbandoned(ctx, cutoff.UnixMicro(), abandonedRowsBatch)
	if err != nil {
		logger.WithError(err).Warn("Failed to list abandoned replication uploads")
		return
	}
	for _, u := range uploads {
		if dest := c.destination(u.DestDC); dest != nil {
			c.abortUpload(ctx, dest, u.ObjectKey, u.UploadID, logger)
		}
		c.deleteUploadRow(ctx, u, logger)
	}
	if len(uploads) > 0 {
		logger.Infof("Dropped %d abandoned replication uploads", len(uploads))
	}
}
