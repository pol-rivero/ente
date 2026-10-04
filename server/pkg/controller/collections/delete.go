package collections

import (
	"context"
	"database/sql"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/stacktrace"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

const (
	defaultMaxRecursiveDelete     = 10_000
	defaultRecursiveDeleteTimeout = 30 * gTime.Second
)

func maxRecursiveDelete() int {
	if limit := viper.GetInt("collections.max-recursive-delete"); limit > 0 {
		return limit
	}
	return defaultMaxRecursiveDelete
}

// collectionTreeTimeout is too short for a large subtree. Keep this below the
// proxies' request timeouts.
func recursiveDeleteTimeout() gTime.Duration {
	if seconds := viper.GetInt("collections.recursive-delete-timeout-seconds"); seconds > 0 {
		return gTime.Duration(seconds) * gTime.Second
	}
	return defaultRecursiveDeleteTimeout
}

func (c *CollectionController) TrashV4(ctx context.Context, userID int64, cID int64, keepFiles bool, recursive bool) error {
	slots, timeout := collectionTreeSlots, collectionTreeTimeout
	if recursive {
		slots, timeout = recursiveDeleteSlots, recursiveDeleteTimeout()
	}
	var disabledLinks []string
	err := c.changeCollectionTreeWithin(ctx, userID, slots, timeout, func(ctx context.Context, tx *sql.Tx) error {
		node, err := c.CollectionRepo.GetTreeNodeTx(ctx, tx, cID)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		switch {
		case node == nil || node.OwnerID != userID:
			return stacktrace.Propagate(&ente.ErrNotFoundError, "collection %d", cID)
		case node.App != string(ente.Drive) || !node.AllowDelete():
			return stacktrace.Propagate(ente.ErrNotDeletableWithV4, "collection %d", cID)
		case node.IsDeleted:
			log.WithFields(log.Fields{
				"c_id":    cID,
				"user_id": userID,
			}).Warning("Collection is already deleted")
			return nil
		}
		ids, err := c.liveCollectionsToDelete(ctx, tx, userID, cID, keepFiles, recursive)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		links, err := c.CollectionRepo.CollectionLinkRepo.DisableSharingForCollectionsTx(ctx, tx, ids)
		if err != nil {
			return stacktrace.Propagate(err, "failed to disable public share urls")
		}
		if err := c.CastRepo.RevokeTokensForCollectionsTx(ctx, tx, ids); err != nil {
			return stacktrace.Propagate(err, "failed to revoke cast tokens")
		}
		if err := c.CollectionRepo.ScheduleDeletesTx(ctx, tx, ids, repo.TrashCollectionDriveQueue); err != nil {
			return stacktrace.Propagate(err, "")
		}
		disabledLinks = links
		return nil
	})
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	c.CollectionRepo.CollectionLinkRepo.Cache.Invalidate(disabledLinks...)
	return nil
}

func (c *CollectionController) liveCollectionsToDelete(ctx context.Context, tx *sql.Tx, ownerID, cID int64, keepFiles bool, recursive bool) ([]int64, error) {
	ids := []int64{cID}
	if recursive {
		limit := maxRecursiveDelete()
		subtree, err := c.CollectionRepo.GetLiveSubtreeIDsTx(ctx, tx, ownerID, cID, limit+1)
		if err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		if len(subtree) > limit {
			return nil, stacktrace.Propagate(ente.ErrSubtreeTooLarge, "collection %d has more than %d live folders", cID, limit)
		}
		ids = subtree
	}
	if keepFiles {
		// Locking the rows first makes a concurrent upload into the subtree
		// either visible here or rejected (it needs a live folder).
		if err := c.CollectionRepo.LockCollectionsTx(ctx, tx, ids); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		hasFiles, err := c.CollectionRepo.HasLiveFilesTx(ctx, tx, ids)
		if err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		if hasFiles {
			return nil, stacktrace.Propagate(&ente.ErrCollectionNotEmpty, "collection %d", cID)
		}
	}
	if !recursive {
		hasChildren, err := c.CollectionRepo.HasLiveChildrenTx(ctx, tx, ownerID, cID)
		if err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		if hasChildren {
			return nil, stacktrace.Propagate(ente.ErrHasChildren, "collection %d", cID)
		}
	}
	return ids, nil
}
