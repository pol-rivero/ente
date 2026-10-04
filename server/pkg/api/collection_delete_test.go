package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/repo"
	castRepo "github.com/ente/museum/pkg/repo/cast"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestDeleteCollectionV4Responses(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	reset := func() {
		testutil.ResetTables(t, db)
		_, err := db.Exec(`DELETE FROM queue WHERE queue_name = $1`, repo.TrashCollectionDriveQueue)
		require.NoError(t, err)
	}
	reset()
	t.Cleanup(reset)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "owner@example.com", CreationTime: 1})
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "other@example.com", CreationTime: 1})
	linkRepo := public.NewCollectionLinkRepository(db, "")
	linkRepo.Cache = public.NewLinkCache(time.Minute, time.Minute)
	collectionRepo := &repo.CollectionRepository{DB: db, CollectionLinkRepo: linkRepo, QueueRepo: &repo.QueueRepository{DB: db}}
	h := &CollectionHandler{Controller: &collections.CollectionController{
		CollectionRepo: collectionRepo,
		CastRepo:       &castRepo.Repository{DB: db},
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/collections/v4/:collectionID", h.TrashV4)
	insert := func(ownerID int64, app ente.App, collectionType string, parentID any) int64 {
		var id int64
		var key any
		if parentID != nil {
			key = "key"
		}
		require.NoError(t, db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes,
				updation_time, app, parent_id, parent_encrypted_key, parent_key_nonce)
			VALUES ($1, 'key', 'nonce', 'name', $2, '{}', 1, $3, $4, $5, $5) RETURNING collection_id`,
			ownerID, collectionType, app, parentID, key).Scan(&id))
		return id
	}
	addFile := func(collectionID int64) {
		var fileID int64
		require.NoError(t, db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES (1, 'drive', 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`).Scan(&fileID))
		_, err := db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
			VALUES ($1, $2, 'key', 'nonce', 1, 1, 1)`, collectionID, fileID)
		require.NoError(t, err)
	}
	root := insert(1, ente.Drive, "folder", nil)
	child := insert(1, ente.Drive, "folder", root)
	addFile(root)
	photos := insert(1, ente.Photos, "folder", nil)
	uncategorized := insert(1, ente.Drive, "uncategorized", nil)
	othersFolder := insert(2, ente.Drive, "folder", nil)
	request := func(path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req.Header.Set("X-Auth-User-ID", "1")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		var body map[string]any
		if recorder.Body.Len() > 0 {
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		}
		return recorder.Code, body
	}
	requireError := func(path string, status int, code ente.ErrorCode) {
		t.Helper()
		gotStatus, body := request(path)
		require.Equal(t, status, gotStatus, path)
		require.Equal(t, string(code), body["code"], path)
	}
	path := func(id int64, keepFiles, recursive bool) string {
		return fmt.Sprintf("/collections/v4/%d?keepFiles=%t&recursive=%t", id, keepFiles, recursive)
	}

	for _, invalid := range []string{
		"/collections/v4/abc?keepFiles=false&recursive=true",
		fmt.Sprintf("/collections/v4/%d?keepFiles=false", root),
		fmt.Sprintf("/collections/v4/%d?recursive=true", root),
		fmt.Sprintf("/collections/v4/%d?keepFiles=no&recursive=true", root),
	} {
		requireError(invalid, http.StatusBadRequest, ente.BadRequest)
	}
	requireError(path(1_000_000, false, true), http.StatusNotFound, ente.NotFoundError)
	requireError(path(othersFolder, false, true), http.StatusNotFound, ente.NotFoundError)
	requireError(path(photos, false, true), http.StatusBadRequest, ente.InvalidCollection)
	requireError(path(uncategorized, false, false), http.StatusBadRequest, ente.InvalidCollection)

	testutil.SetViper(t, "collections.max-recursive-delete", 1)
	requireError(path(root, true, true), http.StatusBadRequest, ente.SubtreeTooLarge)
	requireError(path(root, true, false), http.StatusConflict, ente.CollectionNotEmpty)
	requireError(path(root, false, false), http.StatusConflict, ente.HasChildren)

	testutil.SetViper(t, "collections.recursive-delete-timeout-seconds", 1)
	holder, err := db.Begin()
	require.NoError(t, err)
	_, err = holder.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('ctree:' || $1::bigint, 0))`, 1)
	require.NoError(t, err)
	requireError(path(root, false, true), http.StatusServiceUnavailable, ente.CollectionTreeBusy)
	require.NoError(t, holder.Rollback())

	viper.Set("collections.max-recursive-delete", 2)
	for range 2 {
		code, body := request(path(root, false, true))
		require.Equal(t, http.StatusOK, code)
		require.Empty(t, body)
	}
	code, _ := request(path(child, true, false))
	require.Equal(t, http.StatusOK, code)
}
