package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	"github.com/lib/pq"
	"github.com/sirupsen/logrus"
)

const driveTrashTreeLockTimeout = 30 * gTime.Second

var ErrDriveTrashBatchTimeout = errors.New("timed out trashing a batch of a Drive collection")

// Like TrashV3, except that an owner file that is still in another live
// collection of the owner is only removed from this one. Collections deleted
// together are all marked deleted before any of them gets here, so they
// don't keep each other's files alive.
//
// Returns false if it stopped at yieldAt. Each batch commits, so the next call
// resumes where this one stopped.
func (repo *CollectionRepository) TrashDriveCollection(ctx context.Context, collectionID int64, ownerID int64, yieldAt gTime.Time, batchTimeout gTime.Duration) (bool, error) {
	log := logrus.WithFields(logrus.Fields{
		"deleting_collection": collectionID,
	})
	err := withBatchTimeout(ctx, batchTimeout, func(ctx context.Context) error {
		return repo.reRootLiveChildren(ctx, collectionID, ownerID, log)
	})
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	fileIDs, err := repo.GetCollectionFileIDs(collectionID, ownerID)
	if err != nil {
		return false, stacktrace.Propagate(err, "failed to get fileIDs")
	}
	log.WithField("file_count", len(fileIDs)).Debug("Fetched fileIDs")
	batchSize := 2000
	for i := 0; i < len(fileIDs); i += batchSize {
		if i > 0 && gTime.Now().After(yieldAt) {
			return false, nil
		}
		batch := fileIDs[i:min(i+batchSize, len(fileIDs))]
		err := withBatchTimeout(ctx, batchTimeout, func(ctx context.Context) error {
			if err := repo.FileRepo.VerifyFileOwner(ctx, batch, ownerID, log); err != nil {
				return stacktrace.Propagate(err, "")
			}
			return stacktrace.Propagate(repo.trashOrUnlinkDriveFiles(ctx, collectionID, ownerID, batch), "failed to trash files")
		})
		if err != nil {
			return false, stacktrace.Propagate(err, "")
		}
	}
	count, err := repo.GetCollectionsFilesCount(collectionID)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	if count != 0 {
		removedFiles, removeErr := repo.removeAllFilesAddedByOthers(collectionID, ownerID)
		if removeErr != nil {
			return false, stacktrace.Propagate(removeErr, "")
		}
		if count != removedFiles {
			return false, fmt.Errorf("investigate: collection %d still has %d files which are not deleted", collectionID, count-removedFiles)
		}
		log.WithField("file_count", count).
			WithField("removed_files", removedFiles).
			Debug("All files are removed from the collection")
	}
	return true, nil
}

func withBatchTimeout(ctx context.Context, timeout gTime.Duration, fn func(ctx context.Context) error) error {
	batchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := fn(batchCtx)
	if err != nil && ctx.Err() == nil && errors.Is(batchCtx.Err(), context.DeadlineExceeded) {
		return stacktrace.Propagate(ErrDriveTrashBatchTimeout, "%v", err)
	}
	return err
}

// Nothing can add a child to a deleted folder, so the unlocked check can't
// miss one.
func (repo *CollectionRepository) reRootLiveChildren(ctx context.Context, collectionID, ownerID int64, log *logrus.Entry) error {
	hasChildren, err := hasLiveChildren(ctx, repo.DB, ownerID, collectionID)
	if err != nil || !hasChildren {
		return stacktrace.Propagate(err, "")
	}
	return repo.InCollectionTreeTx(ctx, ownerID, driveTrashTreeLockTimeout, func(tx *sql.Tx) error {
		count, err := repo.ReRootLiveChildrenTx(ctx, tx, ownerID, collectionID, time.Microseconds())
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		log.WithField("count", count).Warn("Moved live subfolders of a deleted folder to the root")
		return nil
	})
}

// Drive only: the row lock orders this against ScheduleDeletesTx, so a file
// can't land in a folder after the trash worker has listed its files. A move
// touches both folders in one statement, as for other apps, rather than
// locking them in request order.
func touchLiveDriveCollection(ctx context.Context, tx *sql.Tx, updationTime int64, liveID int64, otherIDs ...int64) error {
	rows, err := tx.QueryContext(ctx, `UPDATE collections SET updation_time = $1
		WHERE (collection_id = $2 AND is_deleted = FALSE) OR collection_id = ANY($3)
		RETURNING collection_id = $2`, updationTime, liveID, pq.Array(otherIDs))
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	touched := false
	for rows.Next() {
		var isLiveID bool
		if err := rows.Scan(&isLiveID); err != nil {
			return stacktrace.Propagate(err, "")
		}
		touched = touched || isLiveID
	}
	if err := rows.Err(); err != nil {
		return stacktrace.Propagate(err, "")
	}
	if !touched {
		return stacktrace.Propagate(ente.ErrCollectionDeleted, "collection %d", liveID)
	}
	return nil
}

func (repo *CollectionRepository) trashOrUnlinkDriveFiles(ctx context.Context, collectionID, ownerID int64, fileIDs []int64) error {
	tx, err := repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	if err := lockFiles(ctx, tx, ownerID, fileIDs); err != nil {
		return stacktrace.Propagate(err, "")
	}
	rows, err := tx.QueryContext(ctx, `SELECT cf.file_id, EXISTS (
			SELECT 1 FROM collection_files other
			JOIN collections c ON c.collection_id = other.collection_id
			WHERE other.file_id = cf.file_id AND other.collection_id <> cf.collection_id
				AND other.is_deleted = FALSE AND c.owner_id = $3 AND c.is_deleted = FALSE)
		FROM collection_files cf
		JOIN files f ON f.file_id = cf.file_id AND f.owner_id = $3
		WHERE cf.collection_id = $1 AND cf.file_id = ANY($2) AND cf.is_deleted = FALSE`,
		collectionID, pq.Array(fileIDs), ownerID)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	unlink := make([]int64, 0)
	trashIDs := make([]int64, 0)
	trash := make([]ente.TrashItemRequest, 0)
	for rows.Next() {
		var fileID int64
		var inAnotherLiveCollection bool
		if err := rows.Scan(&fileID, &inAnotherLiveCollection); err != nil {
			return stacktrace.Propagate(err, "")
		}
		if inAnotherLiveCollection {
			unlink = append(unlink, fileID)
		} else {
			trashIDs = append(trashIDs, fileID)
			trash = append(trash, ente.TrashItemRequest{FileID: fileID, CollectionID: collectionID})
		}
	}
	if err := rows.Err(); err != nil {
		return stacktrace.Propagate(err, "")
	}
	if len(unlink) > 0 {
		updationTime := time.Microseconds()
		if _, err := tx.ExecContext(ctx, `UPDATE collection_files SET is_deleted = TRUE, updation_time = $1
			WHERE collection_id = $2 AND file_id = ANY($3)`, updationTime, collectionID, pq.Array(unlink)); err != nil {
			return stacktrace.Propagate(err, "")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collections SET updation_time = $1 WHERE collection_id = $2`,
			updationTime, collectionID); err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	var accessTokens []string
	if len(trash) > 0 {
		accessTokens, err = repo.TrashRepo.trashLockedFiles(ctx, tx, ownerID, trashIDs, ente.TrashRequest{OwnerID: ownerID, TrashItems: trash})
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	if err := tx.Commit(); err != nil {
		return stacktrace.Propagate(err, "")
	}
	repo.TrashRepo.FileLinkRepo.Cache.Invalidate(accessTokens...)
	return nil
}
