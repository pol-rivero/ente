package collections

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	"github.com/lib/pq"
)

const (
	maxConcurrentTreeChanges         = 8
	maxConcurrentTreeChangesPerOwner = 2
	maxConcurrentRecursiveDeletes    = 2
	deadlockDetected                 = "40P01"
)

// Tree transactions hold a pooled connection while they wait for the tree
// lock. The process slots bound how many can wait at once, and the per-owner
// slots stop one user from taking all of them; waiting for either holds no
// connection. The timeout bounds both waits plus the transaction. Recursive
// deletes run much longer, so they get their own slots and don't hold up
// the quick changes.
var (
	collectionTreeSlots      = make(chan struct{}, maxConcurrentTreeChanges)
	recursiveDeleteSlots     = make(chan struct{}, maxConcurrentRecursiveDeletes)
	collectionTreeOwnerSlots = controller.NewKeyedSlots(maxConcurrentTreeChangesPerOwner)
	collectionTreeTimeout    = 5 * gTime.Second
)

func (c *CollectionController) changeCollectionTree(ctx context.Context, ownerID int64, change func(ctx context.Context, tx *sql.Tx) error) error {
	return c.changeCollectionTreeWithin(ctx, ownerID, collectionTreeSlots, collectionTreeTimeout, change)
}

func (c *CollectionController) changeCollectionTreeWithin(ctx context.Context, ownerID int64, slots chan struct{}, timeout gTime.Duration, change func(ctx context.Context, tx *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	releaseOwner, ok := collectionTreeOwnerSlots.Acquire(ctx, ownerID)
	if !ok {
		return stacktrace.Propagate(ente.ErrCollectionTreeBusy, "no free tree change slot for owner")
	}
	defer releaseOwner()
	release, ok := controller.AcquireSlot(ctx, slots)
	if !ok {
		return stacktrace.Propagate(ente.ErrCollectionTreeBusy, "no free tree change slot")
	}
	defer release()
	err := c.CollectionRepo.InCollectionTreeTx(ctx, ownerID, timeout, func(tx *sql.Tx) error {
		return change(ctx, tx)
	})
	var apiErr *ente.ApiError
	var pqErr *pq.Error
	if err != nil && !errors.As(err, &apiErr) &&
		(errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, repo.ErrCollectionTreeLockTimeout) ||
			// Row locks taken by concurrent file moves can deadlock with a delete.
			(errors.As(err, &pqErr) && pqErr.Code == deadlockDetected)) {
		return stacktrace.Propagate(ente.ErrCollectionTreeBusy, "%v", err)
	}
	return stacktrace.Propagate(err, "")
}

func validateParentFields(parentID *int64, parentEncryptedKey, parentKeyNonce *string) error {
	if parentID == nil {
		if parentEncryptedKey != nil || parentKeyNonce != nil {
			return ente.NewBadRequestWithMessage("parentEncryptedKey and parentKeyNonce are only allowed with a parent")
		}
		return nil
	}
	if parentEncryptedKey == nil || parentKeyNonce == nil {
		return ente.NewBadRequestWithMessage("parentEncryptedKey and parentKeyNonce are required with a parent")
	}
	if err := validateBase64DecodedLength(*parentEncryptedKey, encryptedCollectionKeyLen, "parentEncryptedKey"); err != nil {
		return ente.NewBadRequestWithMessage(err.Error())
	}
	if err := validateBase64DecodedLength(*parentKeyNonce, secretboxNonceBytes, "parentKeyNonce"); err != nil {
		return ente.NewBadRequestWithMessage(err.Error())
	}
	return nil
}

func (c *CollectionController) validParentAncestors(ctx context.Context, tx *sql.Tx, parentID int64, ownerID int64) ([]int64, error) {
	parent, err := c.CollectionRepo.GetTreeNodeTx(ctx, tx, parentID)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	if !parent.IsLiveDriveFolderOf(ownerID) {
		return nil, stacktrace.Propagate(ente.ErrInvalidParent, "parent %d", parentID)
	}
	ancestors, err := c.CollectionRepo.GetAncestorIDsTx(ctx, tx, parentID, ente.MaxCollectionDepth)
	return ancestors, stacktrace.Propagate(err, "")
}

func (c *CollectionController) createWithParent(ctx context.Context, collection ente.Collection) (ente.Collection, error) {
	if collection.Type != "folder" {
		return ente.Collection{}, stacktrace.Propagate(ente.ErrInvalidParent, "type %s can't have a parent", collection.Type)
	}
	var created ente.Collection
	err := c.changeCollectionTree(ctx, collection.Owner.ID, func(ctx context.Context, tx *sql.Tx) error {
		ancestors, err := c.validParentAncestors(ctx, tx, *collection.ParentID, collection.Owner.ID)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		if len(ancestors)+1 > ente.MaxCollectionDepth {
			return stacktrace.Propagate(ente.ErrMaxDepthExceeded, "")
		}
		collection.UpdationTime = time.Microseconds()
		created, err = c.CollectionRepo.CreateTx(ctx, tx, collection)
		return stacktrace.Propagate(err, "")
	})
	if err != nil {
		return ente.Collection{}, stacktrace.Propagate(err, "")
	}
	return created, nil
}

func (c *CollectionController) MoveCollection(ctx context.Context, actorUserID int64, req ente.MoveCollectionRequest) error {
	if !req.NewParentID.Present {
		return ente.NewBadRequestWithMessage("newParentID is required, use null to move to the root")
	}
	newParentID := req.NewParentID.Value
	if err := validateParentFields(newParentID, req.ParentEncryptedKey, req.ParentKeyNonce); err != nil {
		return stacktrace.Propagate(err, "")
	}
	return c.changeCollectionTree(ctx, actorUserID, func(ctx context.Context, tx *sql.Tx) error {
		moved, err := c.CollectionRepo.GetTreeNodeTx(ctx, tx, req.CollectionID)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		switch {
		case moved == nil || moved.OwnerID != actorUserID:
			return stacktrace.Propagate(&ente.ErrNotFoundError, "collection %d", req.CollectionID)
		case moved.IsDeleted:
			return stacktrace.Propagate(ente.ErrCollectionDeleted, "collection %d", req.CollectionID)
		case !moved.IsDriveFolder():
			return stacktrace.Propagate(ente.ErrInvalidCollection, "collection %d", req.CollectionID)
		}
		if newParentID != nil {
			ancestors, err := c.validParentAncestors(ctx, tx, *newParentID, actorUserID)
			if err != nil {
				return stacktrace.Propagate(err, "")
			}
			if slices.Contains(ancestors, req.CollectionID) {
				return stacktrace.Propagate(ente.ErrCollectionCycle, "")
			}
			height, err := c.CollectionRepo.GetSubtreeHeightTx(ctx, tx, req.CollectionID, ente.MaxCollectionDepth-len(ancestors)+1)
			if err != nil {
				return stacktrace.Propagate(err, "")
			}
			if len(ancestors)+height > ente.MaxCollectionDepth {
				return stacktrace.Propagate(ente.ErrMaxDepthExceeded, "")
			}
		}
		return stacktrace.Propagate(c.CollectionRepo.SetParentTx(ctx, tx, req.CollectionID, newParentID,
			req.ParentEncryptedKey, req.ParentKeyNonce, time.Microseconds()), "")
	})
}
