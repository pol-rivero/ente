package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/stacktrace"
)

var ErrCollectionTreeLockTimeout = errors.New("timed out waiting for the collection tree lock")

type CollectionTreeNode struct {
	OwnerID   int64
	App       string
	Type      string
	IsDeleted bool
}

func (n *CollectionTreeNode) IsDriveFolder() bool {
	return n.App == string(ente.Drive) && n.Type == "folder"
}

func (n *CollectionTreeNode) AllowDelete() bool {
	return (&ente.Collection{Type: n.Type}).AllowDelete()
}

func (n *CollectionTreeNode) IsLiveDriveFolderOf(ownerID int64) bool {
	return n != nil && n.OwnerID == ownerID && n.IsDriveFolder() && !n.IsDeleted
}

// Every structural change to an owner's folder tree runs in a transaction
// that takes this lock first.
func (repo *CollectionRepository) InCollectionTreeTx(ctx context.Context, ownerID int64, lockTimeout time.Duration, change func(tx *sql.Tx) error) error {
	return InAdvisoryLockTx(ctx, repo.DB, "ctree", ownerID, lockTimeout, ErrCollectionTreeLockTimeout, change)
}

// Returns nil if the collection doesn't exist.
func (repo *CollectionRepository) GetTreeNodeTx(ctx context.Context, tx *sql.Tx, collectionID int64) (*CollectionTreeNode, error) {
	var node CollectionTreeNode
	err := tx.QueryRowContext(ctx, `SELECT owner_id, app, type, is_deleted FROM collections WHERE collection_id = $1`,
		collectionID).Scan(&node.OwnerID, &node.App, &node.Type, &node.IsDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	return &node, nil
}

// Returns the collection's ID followed by its ancestors' IDs up to the root,
// at most limit of them.
func (repo *CollectionRepository) GetAncestorIDsTx(ctx context.Context, tx *sql.Tx, ownerID, collectionID int64, limit int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE anc(id, pid, d) AS (
			SELECT collection_id, parent_id, 1 FROM collections
			WHERE collection_id = $2 AND owner_id = $1 AND app = 'drive'
			UNION ALL
			SELECT c.collection_id, c.parent_id, a.d + 1
			FROM collections c JOIN anc a ON c.collection_id = a.pid
			WHERE a.d < $3 AND c.owner_id = $1 AND c.app = 'drive')
		SELECT id FROM anc ORDER BY d`, ownerID, collectionID, limit)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	return scanInt64s(rows)
}

// There's no index on parent_id: descendant walks read the owner's live Drive
// rows once (collections_owner_id_index) and hash join over them.
const ownerLiveDriveRows = `owned AS MATERIALIZED (
			SELECT collection_id, parent_id FROM collections
			WHERE owner_id = $1 AND app = 'drive' AND NOT is_deleted)`

// Counts the collection itself as 1 and skips deleted descendants. Returns at
// most limit.
func (repo *CollectionRepository) GetSubtreeHeightTx(ctx context.Context, tx *sql.Tx, ownerID, collectionID int64, limit int) (int, error) {
	var height sql.NullInt64
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE `+ownerLiveDriveRows+`,
		sub(id, d) AS (
			SELECT collection_id, 1 FROM owned WHERE collection_id = $2
			UNION ALL
			SELECT o.collection_id, s.d + 1
			FROM owned o JOIN sub s ON o.parent_id = s.id
			WHERE s.d < $3)
		SELECT max(d) FROM sub`, ownerID, collectionID, limit).Scan(&height)
	return int(height.Int64), stacktrace.Propagate(err, "")
}

func (repo *CollectionRepository) SetParentTx(ctx context.Context, tx *sql.Tx, collectionID int64, parentID *int64, parentEncryptedKey, parentKeyNonce *string, updationTime int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE collections
		SET parent_id = $2, parent_encrypted_key = $3, parent_key_nonce = $4, updation_time = $5
		WHERE collection_id = $1`, collectionID, parentID, parentEncryptedKey, parentKeyNonce, updationTime)
	return stacktrace.Propagate(err, "")
}

// Returns at most limit live collections of the subtree, the root included.
// Live folders below a deleted one aren't part of it: the trash worker
// re-roots them (ReRootLiveChildrenTx).
func (repo *CollectionRepository) GetLiveSubtreeIDsTx(ctx context.Context, tx *sql.Tx, ownerID, collectionID int64, limit int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE `+ownerLiveDriveRows+`,
		sub(id) AS (
			SELECT collection_id FROM owned WHERE collection_id = $2
			UNION
			SELECT o.collection_id
			FROM owned o JOIN sub s ON o.parent_id = s.id)
		SELECT id FROM sub LIMIT $3`, ownerID, collectionID, limit)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	return scanInt64s(rows)
}

func (repo *CollectionRepository) HasLiveChildrenTx(ctx context.Context, tx *sql.Tx, ownerID, parentID int64) (bool, error) {
	return hasLiveChildren(ctx, tx, ownerID, parentID)
}

func hasLiveChildren(ctx context.Context, db interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, ownerID, parentID int64) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM collections
		WHERE owner_id = $1 AND app = 'drive' AND parent_id = $2 AND NOT is_deleted)`, ownerID, parentID).Scan(&exists)
	return exists, stacktrace.Propagate(err, "")
}

// Older binaries could delete a folder without its children.
func (repo *CollectionRepository) ReRootLiveChildrenTx(ctx context.Context, tx *sql.Tx, ownerID, parentID int64, updationTime int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE collections
		SET parent_id = NULL, parent_encrypted_key = NULL, parent_key_nonce = NULL, updation_time = $3
		WHERE owner_id = $1 AND app = 'drive' AND parent_id = $2 AND NOT is_deleted`, ownerID, parentID, updationTime)
	if err != nil {
		return 0, stacktrace.Propagate(err, "")
	}
	count, err := result.RowsAffected()
	return count, stacktrace.Propagate(err, "")
}

func scanInt64s(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	values := make([]int64, 0)
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		values = append(values, value)
	}
	return values, stacktrace.Propagate(rows.Err(), "")
}
