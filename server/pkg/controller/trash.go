package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type TrashController struct {
	TrashRepo               *repo.TrashRepository
	FileRepo                *repo.FileRepository
	CollectionRepo          *repo.CollectionRepository
	QueueRepo               *repo.QueueRepository
	TaskLockRepo            *repo.TaskLockRepository
	HostName                string
	dropFileMetadataRunning bool
	collectionTrashRunning  bool
	emptyTrashRunning       bool
	deleteAgedTrashRunning  bool

	driveCollectionTrashRunning atomic.Bool
	emptyDriveTrashRunning      atomic.Bool
	driveTrashBudgetOverride    gTime.Duration
}

const (
	driveCollectionTrashLock          = "CollectionTrashDrive"
	defaultDriveCollectionTrashBudget = 50 * gTime.Second
)

var (
	driveTrashLease         = 5 * gTime.Minute
	driveTrashLockHeartbeat = gTime.Minute
	// An item that takes longer goes to the back of the queue, so one large
	// delete can't hold up everyone else's.
	driveTrashItemSlice = 20 * gTime.Second
	// Bounds a batch stuck on a row lock. Progress is committed per batch, so
	// the item resumes on a later run.
	driveTrashBatchTimeout = 2 * gTime.Minute
)

type driveTrashOutcome int

const (
	driveTrashDone driveTrashOutcome = iota
	driveTrashYielded
	driveTrashFailed
)

func (t *TrashController) GetDiff(userID int64, sinceTime int64, app ente.App) ([]ente.Trash, bool, error) {
	trashFilesDiff, hasMore, err := t.getDiff(userID, sinceTime, repo.TrashDiffLimit, app)
	if err != nil {
		return nil, false, err
	}
	for _, trashFile := range trashFilesDiff {
		if trashFile.IsDeleted {
			trashFile.File.MagicMetadata = nil
			trashFile.File.PubicMagicMetadata = nil
			trashFile.File.Metadata = ente.FileAttributes{}
			trashFile.File.Info = nil
		}
	}
	return trashFilesDiff, hasMore, err
}

// Never split a version across pages. Results may be smaller or larger than
// limit so every row with the boundary timestamp stays together.
func (t *TrashController) getDiff(userID int64, sinceTime int64, limit int, app ente.App) ([]ente.Trash, bool, error) {
	diffLimitPlusOne, err := t.TrashRepo.GetDiff(userID, sinceTime, limit+1, app)
	if err != nil {
		return nil, false, stacktrace.Propagate(err, "")
	}
	if len(diffLimitPlusOne) <= limit {
		return diffLimitPlusOne, false, nil
	}
	lastFileVersion := diffLimitPlusOne[limit].UpdatedAt
	filteredDiffs := t.removeFilesWithVersion(diffLimitPlusOne, lastFileVersion)
	if len(filteredDiffs) > 0 {
		return filteredDiffs, true, nil
	}
	diff, err := t.TrashRepo.GetFilesWithVersion(userID, lastFileVersion, app)
	if err != nil {
		return nil, false, stacktrace.Propagate(err, "")
	}
	return diff, true, nil
}

func (t *TrashController) Delete(ctx context.Context, request ente.DeleteTrashFilesRequest) error {
	err := t.TrashRepo.Delete(ctx, request.OwnerID, request.FileIDs)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	return nil
}

func (t *TrashController) EmptyTrash(ctx context.Context, userID int64, req ente.EmptyTrashRequest, app ente.App) error {
	err := t.TrashRepo.EmptyTrash(ctx, userID, req.LastUpdatedAt, app)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if app == ente.Drive {
		defer t.processEmptyDriveTrashRequests()
	} else {
		defer t.processEmptyTrashRequests()
	}
	return nil
}

func (t *TrashController) CleanupTrashedCollections() {
	ctxLogger := log.WithFields(log.Fields{
		"flow": "trash_collection",
		"id":   uuid.New().String(),
	})
	item_processed_count := 0
	if t.collectionTrashRunning {
		ctxLogger.Info("Already moving collection to trash, skipping cron")
		return
	}
	t.collectionTrashRunning = true
	defer func() {
		ctxLogger.WithField("items_processed", item_processed_count).Info("cron run finished")
		t.collectionTrashRunning = false
	}()

	itemsV3, err2 := t.QueueRepo.GetItemsReadyForDeletion(repo.TrashCollectionQueueV3, 100)
	if err2 != nil {
		log.Error("Could not fetch from collection trash queue", err2)
		return
	}
	item_processed_count += len(itemsV3)
	for _, item := range itemsV3 {
		t.trashCollection(item, repo.TrashCollectionQueueV3, ctxLogger)
	}
}

// Unlike the Photos/Locker queue, this one isn't capped at 100 items per run:
// a large folder tree would otherwise take hours to reach the trash.
func (t *TrashController) CleanupTrashedDriveCollections() {
	ctxLogger := log.WithFields(log.Fields{
		"flow": "trash_drive_collection",
		"id":   uuid.New().String(),
	})
	if !t.driveCollectionTrashRunning.CompareAndSwap(false, true) {
		ctxLogger.Info("Already moving Drive collections to trash, skipping cron")
		return
	}
	defer t.driveCollectionTrashRunning.Store(false)
	// Every Drive collection is trashed under this lock, so one owner's items
	// never run concurrently.
	leaseEnd := gTime.Now().Add(driveTrashLease)
	lockStatus, err := t.TaskLockRepo.AcquireLock(driveCollectionTrashLock, leaseEnd.UnixMicro(), t.HostName)
	if err != nil || !lockStatus {
		if err != nil {
			ctxLogger.WithError(err).Error("error while acquiring lock")
		}
		return
	}
	defer func() {
		if releaseErr := t.TaskLockRepo.ReleaseLockBy(driveCollectionTrashLock, t.HostName); releaseErr != nil {
			ctxLogger.WithError(releaseErr).Error("Error while releasing lock")
		}
	}()
	ctx, stopHeartbeat := t.keepTaskLock(context.Background(), driveCollectionTrashLock, leaseEnd, ctxLogger)
	defer stopHeartbeat()
	outcomes := make(map[driveTrashOutcome]int)
	defer func() {
		if len(outcomes) > 0 {
			ctxLogger.WithFields(log.Fields{
				"items_processed": outcomes[driveTrashDone],
				"items_yielded":   outcomes[driveTrashYielded],
				"items_failed":    outcomes[driveTrashFailed],
			}).Info("cron run finished")
		}
	}()
	deadline := gTime.Now().Add(t.driveCollectionTrashBudget())
	cursor := repo.QueueItem{CreatedAt: math.MinInt64}
	failed := make(map[int64]bool)
	for {
		items, err := t.QueueRepo.GetItemsReadyForDeletionAfter(ctx, repo.TrashCollectionDriveQueue, cursor, 100)
		if err != nil {
			if ctx.Err() == nil {
				ctxLogger.WithError(err).Error("Could not fetch from Drive collection trash queue")
			}
			return
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			cursor = item
			if failed[item.Id] {
				continue
			}
			yieldAt := gTime.Now().Add(driveTrashItemSlice)
			if yieldAt.After(deadline) {
				yieldAt = deadline
			}
			outcome := t.trashQueuedDriveCollection(ctx, item, yieldAt, ctxLogger)
			outcomes[outcome]++
			if outcome == driveTrashFailed && ctx.Err() == nil {
				// Otherwise an item that keeps failing, e.g. on a row lock,
				// would be the first one of every run.
				failed[item.Id] = true
				if err := t.QueueRepo.MoveToBack(ctx, repo.TrashCollectionDriveQueue, item.Item); err != nil {
					ctxLogger.WithError(err).WithField("collection_id", item.Item).Error("failed to move item to the back of the queue")
				}
			}
			if gTime.Now().After(deadline) {
				return
			}
		}
		if len(items) == 0 {
			return
		}
	}
}

// The returned context is cancelled when the lock is lost, or when it may
// expire because extending it keeps failing.
func (t *TrashController) keepTaskLock(ctx context.Context, lockName string, leaseEnd gTime.Time, logger *log.Entry) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	logger = logger.WithField("lock", lockName)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := gTime.NewTicker(driveTrashLockHeartbeat)
		defer ticker.Stop()
		// Stop a heartbeat early, so a batch still running can roll back
		// before another host takes the lock.
		stopAt := func() gTime.Duration { return gTime.Until(leaseEnd) - driveTrashLockHeartbeat }
		expiry := gTime.NewTimer(stopAt())
		defer expiry.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-expiry.C:
				logger.Error("couldn't extend lock, stopping")
				cancel()
				return
			case <-ticker.C:
			}
			newLeaseEnd := gTime.Now().Add(driveTrashLease)
			callCtx, cancelCall := context.WithDeadline(ctx, leaseEnd.Add(-driveTrashLockHeartbeat))
			held, err := t.TaskLockRepo.ExtendLockContext(callCtx, lockName, newLeaseEnd.UnixMicro(), t.HostName)
			cancelCall()
			switch {
			case err != nil:
				logger.WithError(err).Warn("failed to extend lock")
			case !held:
				logger.Error("lock lost, stopping")
				cancel()
				return
			default:
				leaseEnd = newLeaseEnd
				expiry.Reset(stopAt())
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-stopped
	}
}

func (t *TrashController) driveCollectionTrashBudget() gTime.Duration {
	if t.driveTrashBudgetOverride > 0 {
		return t.driveTrashBudgetOverride
	}
	if seconds := viper.GetInt("jobs.drive-collection-trash.budget-seconds"); seconds > 0 {
		return gTime.Duration(seconds) * gTime.Second
	}
	return defaultDriveCollectionTrashBudget
}

func (t *TrashController) ProcessEmptyTrashRequests() {
	t.processEmptyTrashRequests()
	t.processEmptyDriveTrashRequests()
}

func (t *TrashController) processEmptyTrashRequests() {
	if t.emptyTrashRunning {
		log.Info("Already processing empty trash requests, skipping cron")
		return
	}
	t.emptyTrashRunning = true
	defer func() {
		t.emptyTrashRunning = false
	}()
	items, err := t.QueueRepo.GetItemsReadyForDeletion(repo.TrashEmptyQueue, 100)
	if err != nil {
		log.Error("Could not fetch from emptyTrashQueue queue", err)
	} else {
		for _, item := range items {
			t.emptyTrash(item, ente.Photos, repo.TrashEmptyQueue)
		}
	}

	itemsLocker, err2 := t.QueueRepo.GetItemsReadyForDeletion(repo.TrashEmptyLockerQueue, 100)
	if err2 != nil {
		log.Error("Could not fetch from emptyTrashLockerQueue queue", err2)
		return
	}
	for _, item := range itemsLocker {
		t.emptyTrash(item, ente.Locker, repo.TrashEmptyLockerQueue)
	}
}

func (t *TrashController) processEmptyDriveTrashRequests() {
	if !t.emptyDriveTrashRunning.CompareAndSwap(false, true) {
		log.Info("Already processing Drive empty trash requests, skipping")
		return
	}
	defer t.emptyDriveTrashRunning.Store(false)
	items, err := t.QueueRepo.GetItemsReadyForDeletion(repo.TrashEmptyDriveQueue, 100)
	if err != nil {
		log.Error("Could not fetch from emptyTrashDriveQueue queue", err)
		return
	}
	for _, item := range items {
		t.emptyTrash(item, ente.Drive, repo.TrashEmptyDriveQueue)
	}
}

func (t *TrashController) DeleteAgedTrashedFiles() {
	if t.deleteAgedTrashRunning {
		log.Info("Already deleting older trashed files, skipping cron")
		return
	}
	t.deleteAgedTrashRunning = true
	defer func() {
		t.deleteAgedTrashRunning = false
	}()

	lockName := "DeleteAgedTrashedFiles"
	lockStatus, err := t.TaskLockRepo.AcquireLock(lockName, time.MicrosecondsAfterHours(1), t.HostName)
	if err != nil || !lockStatus {
		log.Error("Unable to acquire lock to DeleteAgedTrashedFiles")
		return
	}
	defer func() {
		releaseErr := t.TaskLockRepo.ReleaseLock(lockName)
		if releaseErr != nil {
			log.WithError(releaseErr).Error("Error while releasing aged trash lock")
		}
	}()

	userIDToFileMap, err := t.TrashRepo.GetUserIDToFileIDsMapForDeletion()
	if err != nil {
		log.Error("Could not fetch trashed files for deletion", err)
		return
	}

	for userID, fileIDs := range userIDToFileMap {
		ctxLogger := log.WithFields(log.Fields{
			"user_id": userID,
			"fileIds": fileIDs,
		})
		ctxLogger.Info("start deleting old files from trash")
		err = t.TrashRepo.Delete(context.Background(), userID, fileIDs)
		if err != nil {
			ctxLogger.WithError(err).Error("failed to delete file from trash")
			continue
		}
		ctxLogger.Info("successfully deleted old files from trash")
	}
}

// trashedFiles must be sorted by increasing UpdatedAt.
func (t *TrashController) removeFilesWithVersion(trashedFiles []ente.Trash, version int64) []ente.Trash {
	var i = len(trashedFiles) - 1
	for ; i >= 0; i-- {
		if trashedFiles[i].UpdatedAt != version {
			break
		}
	}
	return trashedFiles[0 : i+1]
}

func (t *TrashController) trashCollection(item repo.QueueItem, queueName string, logger *log.Entry) {
	cID, _ := strconv.ParseInt(item.Item, 10, 64)
	collection, err := t.CollectionRepo.Get(cID)
	if err != nil {
		log.Error("Could not fetch collection "+item.Item, err)
		return
	}
	ctxLogger := logger.WithFields(log.Fields{
		"collection_id": cID,
		"user_id":       collection.Owner.ID,
		"queue":         queueName,
		"flow":          "trash_collection",
	})
	if collection.App == string(ente.Drive) {
		// Only the Drive cron trashes Drive collections.
		if err := t.QueueRepo.MoveItem(context.Background(), queueName, repo.TrashCollectionDriveQueue, item.Item); err != nil {
			ctxLogger.WithError(err).Error("failed to move Drive collection to its queue")
			return
		}
		ctxLogger.Info("moved Drive collection to its queue")
		return
	}
	// File exclusivity spans collections, so lock the user rather than one
	// collection.
	lockName := fmt.Sprintf("CollectionTrash:%d", collection.Owner.ID)
	lockStatus, err := t.TaskLockRepo.AcquireLock(lockName, time.MicrosecondsAfterHours(1), t.HostName)
	if err != nil || !lockStatus {
		if err == nil {
			ctxLogger.Error("lock is already taken for deleting collection")
		} else {
			ctxLogger.WithError(err).Error("critical: error while acquiring lock")
		}
		return
	}
	defer func() {
		releaseErr := t.TaskLockRepo.ReleaseLock(lockName)
		if releaseErr != nil {
			ctxLogger.WithError(releaseErr).Error("Error while releasing lock")
		}
	}()
	ctxLogger.Info("start trashing collection")
	err = t.CollectionRepo.TrashV3(context.Background(), cID)
	if err != nil {
		ctxLogger.WithError(err).Error("failed to trash collection")
		return
	}
	err = t.QueueRepo.DeleteItem(queueName, item.Item)
	if err != nil {
		ctxLogger.WithError(err).Error("failed to delete item from queue")
		return
	}
}

func (t *TrashController) trashQueuedDriveCollection(ctx context.Context, item repo.QueueItem, yieldAt gTime.Time, logger *log.Entry) driveTrashOutcome {
	queueName := repo.TrashCollectionDriveQueue
	cID, _ := strconv.ParseInt(item.Item, 10, 64)
	collection, err := t.CollectionRepo.Get(cID)
	if errors.Is(err, sql.ErrNoRows) {
		logger.WithField("collection_id", item.Item).Warn("queued collection doesn't exist, dropping it from the queue")
		if err := t.QueueRepo.DeleteItem(queueName, item.Item); err != nil {
			logger.WithError(err).Error("failed to delete item from queue")
		}
		return driveTrashFailed
	}
	if err != nil {
		logger.WithError(err).WithField("collection_id", item.Item).Error("Could not fetch collection")
		return driveTrashFailed
	}
	ctxLogger := logger.WithFields(log.Fields{
		"collection_id": collection.ID,
		"user_id":       collection.Owner.ID,
		"queue":         queueName,
		"flow":          "trash_collection",
	})
	if collection.App != string(ente.Drive) {
		ctxLogger.Error("queued collection isn't a Drive collection, moving it to the V3 queue")
		if err := t.QueueRepo.MoveItem(ctx, queueName, repo.TrashCollectionQueueV3, item.Item); err != nil {
			ctxLogger.WithError(err).Error("failed to move item to the V3 queue")
		}
		return driveTrashFailed
	}
	if !collection.IsDeleted {
		ctxLogger.Error("queued Drive collection isn't deleted, dropping it from the queue")
		if err := t.QueueRepo.DeleteItem(queueName, item.Item); err != nil {
			ctxLogger.WithError(err).Error("failed to delete item from queue")
		}
		return driveTrashFailed
	}
	ctxLogger.Debug("start trashing collection")
	finished, err := t.CollectionRepo.TrashDriveCollection(ctx, collection.ID, collection.Owner.ID, yieldAt, driveTrashBatchTimeout)
	switch {
	case err != nil && ctx.Err() != nil:
		ctxLogger.WithError(err).Info("stopped trashing collection")
		return driveTrashFailed
	case errors.Is(err, repo.ErrDriveTrashBatchTimeout):
		ctxLogger.WithError(err).Error("timed out trashing collection")
		return driveTrashFailed
	case err != nil:
		ctxLogger.WithError(err).Error("failed to trash collection")
		return driveTrashFailed
	case !finished:
		ctxLogger.Info("collection not trashed yet, moving it to the back of the queue")
		if err := t.QueueRepo.MoveToBack(ctx, queueName, item.Item); err != nil {
			ctxLogger.WithError(err).Error("failed to move item to the back of the queue")
		}
		return driveTrashYielded
	}
	if err := t.QueueRepo.DeleteItem(queueName, item.Item); err != nil {
		ctxLogger.WithError(err).Error("failed to delete item from queue")
		return driveTrashFailed
	}
	return driveTrashDone
}

func (t *TrashController) emptyTrash(item repo.QueueItem, app ente.App, queueName string) {
	lockName := fmt.Sprintf("EmptyTrash:%s", item.Item)
	if app == ente.Drive {
		// Account deletion queues the same item for every app, and Drive's
		// queue drains concurrently with the others.
		lockName = fmt.Sprintf("EmptyTrash:drive:%s", item.Item)
	}
	lockStatus, err := t.TaskLockRepo.AcquireLock(lockName, time.MicrosecondsAfterHours(1), t.HostName)
	split := strings.Split(item.Item, repo.EmptyTrashQueueItemSeparator)
	userID, _ := strconv.ParseInt(split[0], 10, 64)
	lastUpdateAt, _ := strconv.ParseInt(split[1], 10, 64)
	ctxLogger := log.WithFields(log.Fields{
		"user_id":       userID,
		"lastUpdatedAt": lastUpdateAt,
		"flow":          "empty_trash",
		"app":           app,
	})

	if err != nil || !lockStatus {
		if err == nil {
			// todo: error only when lock is help for more than X durat
			ctxLogger.Error("lock is already taken for emptying trash")
		} else {
			ctxLogger.WithError(err).Error("critical: error while acquiring lock")
		}
		return
	}
	defer func() {
		releaseErr := t.TaskLockRepo.ReleaseLock(lockName)
		if releaseErr != nil {
			log.WithError(releaseErr).Error("Error while releasing lock")
		}
	}()

	ctxLogger.Info("Start emptying trash")
	fileIDs, err := t.TrashRepo.GetFilesIDsForDeletion(userID, lastUpdateAt, app)
	if err != nil {
		ctxLogger.WithError(err).Error("Failed to fetch fileIDs")
		return
	}
	ctx := context.Background()
	size := len(fileIDs)
	limit := repo.TrashBatchSize
	for lb := 0; lb < size; lb += limit {
		ub := min(lb+limit, size)
		batch := fileIDs[lb:ub]
		err = t.TrashRepo.Delete(ctx, userID, batch)
		if err != nil {
			ctxLogger.WithField("batchIDs", batch).WithError(err).Error("Failed while deleting batch")
			return
		}
	}
	err = t.QueueRepo.DeleteItem(queueName, item.Item)
	if err != nil {
		log.Error("Error while removing item from queue "+item.Item, err)
		return
	}
	ctxLogger.Info("Finished emptying trash")
}
