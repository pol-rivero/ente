package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/controller/access"
	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDriveCollectionsAndTrashAreIsolatedByClientPackage(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	const userID = int64(1)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: userID, Email: "drive-isolation@ente.io", CreationTime: 1})
	testutil.InsertUsage(t, db, userID, 0)
	_, err := db.Exec(`INSERT INTO key_attributes(user_id, kek_salt, encrypted_key, key_decryption_nonce,
		public_key, encrypted_secret_key, secret_key_decryption_nonce, mem_limit, ops_limit)
		VALUES ($1, 'salt', 'key', 'nonce', 'public', 'secret', 'secret-nonce', 1, 1)`, userID)
	require.NoError(t, err)

	collectionRepo := &repo.CollectionRepository{
		DB:                  db,
		CollectionLinkRepo:  public.NewCollectionLinkRepository(db, ""),
		SecretEncryptionKey: testutil.SecretEncryptionKey(),
	}
	trashRepo := &repo.TrashRepository{DB: db, FileLinkRepo: public.NewFileLinkRepo(db)}
	collectionHandler := &CollectionHandler{Controller: &collections.CollectionController{
		CollectionRepo: collectionRepo,
		UserRepo:       &repo.UserRepository{DB: db},
	}}
	trashHandler := &TrashHandler{Controller: &controller.TrashController{TrashRepo: trashRepo}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/collections", collectionHandler.Create)
	router.GET("/collections/v2", collectionHandler.GetV2)
	router.GET("/trash/v2/diff", trashHandler.GetDiffV2)
	request := func(method, path, clientPackage, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-User-ID", fmt.Sprint(userID))
		req.Header.Set("X-Client-Package", clientPackage)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		return recorder
	}

	packages := map[ente.App]string{
		ente.Photos: "io.ente.photos",
		ente.Locker: "io.ente.locker",
		ente.Drive:  "io.ente.drive",
	}
	collectionIDs := make(map[ente.App]int64)
	for app, clientPackage := range packages {
		body := fmt.Sprintf(`{"encryptedKey":%q,"keyDecryptionNonce":%q,"encryptedName":"name","nameDecryptionNonce":"nonce","type":"folder"}`,
			base64.StdEncoding.EncodeToString(make([]byte, 48)), base64.StdEncoding.EncodeToString(make([]byte, 24)))
		var created struct {
			Collection ente.Collection `json:"collection"`
		}
		require.NoError(t, json.Unmarshal(request(http.MethodPost, "/collections", clientPackage, body).Body.Bytes(), &created))
		require.Equal(t, string(app), created.Collection.App)
		collectionIDs[app] = created.Collection.ID

		var fileID int64
		require.NoError(t, db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
			VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, userID, app).Scan(&fileID))
		_, err := db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
			VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, created.Collection.ID, fileID, userID)
		require.NoError(t, err)
		require.NoError(t, trashRepo.TrashFiles(t.Context(), userID, ente.TrashRequest{
			TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: created.Collection.ID}},
		}))
	}

	for app, clientPackage := range packages {
		var synced struct {
			Collections []ente.Collection `json:"collections"`
		}
		require.NoError(t, json.Unmarshal(request(http.MethodGet, "/collections/v2?sinceTime=0", clientPackage, "").Body.Bytes(), &synced))
		require.Len(t, synced.Collections, 1, "%s collections", app)
		require.Equal(t, collectionIDs[app], synced.Collections[0].ID, "%s collections", app)

		var trash struct {
			Diff []ente.Trash `json:"diff"`
		}
		require.NoError(t, json.Unmarshal(request(http.MethodGet, "/trash/v2/diff?sinceTime=0", clientPackage, "").Body.Bytes(), &trash))
		require.Len(t, trash.Diff, 1, "%s trash", app)
		require.Equal(t, collectionIDs[app], trash.Diff[0].File.CollectionID, "%s trash", app)
	}
}

func TestAddFilesRejectsCrossAppDriveFileWithBadRequest(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	const userID = int64(1)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: userID, Email: "drive-cross-app@ente.io", CreationTime: 1})
	testutil.InsertUsage(t, db, userID, 0)
	var collectionID, fileID int64
	require.NoError(t, db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app)
		VALUES ($1, 'key', 'nonce', 'name', 'album', '{}', 1, 'photos') RETURNING collection_id`, userID).Scan(&collectionID))
	require.NoError(t, db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, 'drive', 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, userID).Scan(&fileID))

	collectionRepo := &repo.CollectionRepository{DB: db, CollectionLinkRepo: public.NewCollectionLinkRepository(db, "")}
	fileRepo := &repo.FileRepository{DB: db}
	h := &CollectionHandler{Controller: &collections.CollectionController{
		AccessCtrl:     access.NewAccessController(collectionRepo, fileRepo),
		CollectionRepo: collectionRepo,
		FileRepo:       fileRepo,
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/collections/add-files", h.AddFiles)
	body := fmt.Sprintf(`{"collectionID":%d,"files":[{"id":%d,"encryptedKey":%q,"keyDecryptionNonce":%q}]}`, collectionID, fileID,
		base64.StdEncoding.EncodeToString(make([]byte, 48)), base64.StdEncoding.EncodeToString(make([]byte, 24)))
	req := httptest.NewRequest(http.MethodPost, "/collections/add-files", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-User-ID", fmt.Sprint(userID))
	req.Header.Set("X-Client-Package", "io.ente.photos")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"code":"CROSS_APP_FILE"`)
}
