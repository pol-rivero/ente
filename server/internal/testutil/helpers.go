package testutil

import (
	"database/sql"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func SetViper(t *testing.T, key string, value any) {
	t.Helper()
	previous := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() { viper.Set(key, previous) })
}

func RequireAPIError(t *testing.T, err error, status int, code ente.ErrorCode) {
	t.Helper()
	var apiErr *ente.ApiError
	require.ErrorAs(t, err, &apiErr, "%v", err)
	require.Equal(t, status, apiErr.HttpStatusCode, "%v", err)
	require.Equal(t, code, apiErr.Code, "%v", err)
}

// Holds the owner's collection tree lock until the returned tx ends.
func HoldCollectionTreeLock(t *testing.T, db *sql.DB, ownerID int64) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('ctree:' || $1::bigint, 0))`, ownerID)
	require.NoError(t, err)
	return tx
}

// Inserts a collection directly, bypassing the controller checks.
func InsertCollection(t *testing.T, db *sql.DB, ownerID int64, app ente.App, collectionType string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
		VALUES ($1, 'key', 'nonce', 'name', $2, '{}', 1, $3) RETURNING collection_id`, ownerID, collectionType, app).Scan(&id))
	return id
}
