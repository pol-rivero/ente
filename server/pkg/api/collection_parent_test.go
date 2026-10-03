package api

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/ente/cast"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller/access"
	"github.com/ente/museum/pkg/controller/collections"
	publicCtrl "github.com/ente/museum/pkg/controller/public"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/ente/museum/pkg/utils/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	parentTestOwnerID  = int64(1)
	parentTestShareeID = int64(2)
)

type collectionResponseFixture struct {
	db             *sql.DB
	router         *gin.Engine
	collectionRepo *repo.CollectionRepository
	linkCtrl       *publicCtrl.CollectionLinkController
}

func setupCollectionResponseFixture(t *testing.T) *collectionResponseFixture {
	t.Helper()
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: parentTestOwnerID, Email: "owner@example.com", CreationTime: 1})
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: parentTestShareeID, Email: "sharee@example.com", CreationTime: 1})
	_, err := db.Exec(`INSERT INTO key_attributes(user_id, kek_salt, encrypted_key, key_decryption_nonce,
		public_key, encrypted_secret_key, secret_key_decryption_nonce, mem_limit, ops_limit)
		VALUES ($1, 'salt', 'key', 'nonce', 'public', 'secret', 'secret-nonce', 1, 1)`, parentTestOwnerID)
	require.NoError(t, err)

	collectionRepo := &repo.CollectionRepository{
		DB:                  db,
		CollectionLinkRepo:  public.NewCollectionLinkRepository(db, ""),
		SecretEncryptionKey: testutil.SecretEncryptionKey(),
	}
	collectionCtrl := &collections.CollectionController{
		AccessCtrl:     access.NewAccessController(collectionRepo, nil),
		CollectionRepo: collectionRepo,
		UserRepo:       &repo.UserRepository{DB: db},
	}
	h := &CollectionHandler{Controller: collectionCtrl}
	castHandler := &CastHandler{CollectionCtrl: collectionCtrl}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/collections", h.Create)
	router.GET("/collections/v2", h.GetV2)
	router.GET("/collections/v3", h.GetWithLimit)
	router.GET("/collections/:collectionID", h.GetCollectionByID)
	router.GET("/cast/:collectionID", func(c *gin.Context) {
		var id int64
		_, _ = fmt.Sscan(c.Param("collectionID"), &id)
		c.Set(auth.CastContext, cast.AuthContext{CollectionID: id})
		castHandler.GetCollection(c)
	})
	return &collectionResponseFixture{
		db:             db,
		router:         router,
		collectionRepo: collectionRepo,
		linkCtrl:       &publicCtrl.CollectionLinkController{CollectionRepo: collectionRepo},
	}
}

func (f *collectionResponseFixture) do(method, path string, userID int64, clientPackage, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-User-ID", fmt.Sprint(userID))
	req.Header.Set("X-Client-Package", clientPackage)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, req)
	return recorder
}

func (f *collectionResponseFixture) request(t *testing.T, method, path string, userID int64, clientPackage, body string) []byte {
	t.Helper()
	recorder := f.do(method, path, userID, clientPackage, body)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	return recorder.Body.Bytes()
}

func (f *collectionResponseFixture) insertLegacyCollection(t *testing.T, app ente.App, collectionType string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name,
		encrypted_name, name_decryption_nonce, type, attributes, updation_time, magic_metadata, pub_magic_metadata, app)
		VALUES ($1, 'owner-key', 'owner-nonce', '', 'enc-name', 'name-nonce', $2, '{"version":1}', 1,
			'{"version":1,"count":1,"data":"private","header":"private-header"}',
			'{"version":1,"count":1,"data":"public","header":"public-header"}', $3)
		RETURNING collection_id`, parentTestOwnerID, collectionType, app).Scan(&id))
	return id
}

func (f *collectionResponseFixture) shareAndLink(t *testing.T, collectionID int64, token string) {
	t.Helper()
	require.NoError(t, f.collectionRepo.Share(collectionID, parentTestOwnerID, parentTestShareeID, "share-key", ente.COLLABORATOR, 5))
	_, err := f.db.Exec(`INSERT INTO public_collection_tokens (collection_id, access_token, valid_till, device_limit)
		VALUES ($1, $2, 0, 0)`, collectionID, token)
	require.NoError(t, err)
}

func (f *collectionResponseFixture) publicCollectionJSON(t *testing.T, collectionID int64) string {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set(auth.PublicAccessKey, ente.PublicAccessContext{CollectionID: collectionID})
	collection, err := f.linkCtrl.GetPublicCollection(ctx, false)
	require.NoError(t, err)
	encoded, err := json.Marshal(gin.H{"collection": collection})
	require.NoError(t, err)
	return string(encoded)
}

func syncedCollectionsByID(t *testing.T, body []byte, keys ...string) map[int64]string {
	t.Helper()
	var envelope map[string][]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &envelope))
	result := make(map[int64]string)
	for _, key := range keys {
		for _, raw := range envelope[key] {
			var id struct {
				ID int64 `json:"id"`
			}
			require.NoError(t, json.Unmarshal(raw, &id))
			result[id.ID] = key + ":" + string(raw)
		}
	}
	return result
}

func (f *collectionResponseFixture) responses(t *testing.T, id int64, clientPackage string) map[string]string {
	t.Helper()
	get := func(path string, userID int64) []byte {
		return f.request(t, http.MethodGet, path, userID, clientPackage, "")
	}
	single := fmt.Sprintf("/collections/%d", id)
	return map[string]string{
		"owner":    string(get(single, parentTestOwnerID)),
		"sharee":   string(get(single, parentTestShareeID)),
		"ownerV2":  syncedCollectionsByID(t, get("/collections/v2?sinceTime=0", parentTestOwnerID), "collections")[id],
		"shareeV2": syncedCollectionsByID(t, get("/collections/v2?sinceTime=0", parentTestShareeID), "collections")[id],
		"ownerV3":  syncedCollectionsByID(t, get("/collections/v3?sinceTime=0", parentTestOwnerID), "owned", "shared")[id],
		"shareeV3": syncedCollectionsByID(t, get("/collections/v3?sinceTime=0", parentTestShareeID), "owned", "shared")[id],
		"public":   f.publicCollectionJSON(t, id),
		"cast":     string(get(fmt.Sprintf("/cast/%d", id), 0)),
	}
}

func (f *collectionResponseFixture) createDriveFolder(t *testing.T, parentID *int64) int64 {
	t.Helper()
	collection := ente.Collection{
		Owner:               ente.CollectionUser{ID: parentTestOwnerID},
		EncryptedKey:        "owner-key",
		KeyDecryptionNonce:  "owner-nonce",
		EncryptedName:       "enc-name",
		NameDecryptionNonce: "name-nonce",
		Type:                "folder",
		UpdationTime:        1,
		App:                 string(ente.Drive),
	}
	if parentID != nil {
		key, nonce := "parent-key", "parent-nonce"
		collection.ParentID, collection.ParentEncryptedKey, collection.ParentKeyNonce = parentID, &key, &nonce
	}
	created, err := f.collectionRepo.Create(collection)
	require.NoError(t, err)
	return created.ID
}

// The golden file was recorded with the server as it was before migration 154.
func TestExistingCollectionResponsesAreUnchanged(t *testing.T) {
	f := setupCollectionResponseFixture(t)
	photosID := f.insertLegacyCollection(t, ente.Photos, "album")
	lockerID := f.insertLegacyCollection(t, ente.Locker, "folder")
	f.shareAndLink(t, photosID, "PHOTOSLINK")
	f.shareAndLink(t, lockerID, "LOCKERLINK")
	_, err := f.db.Exec(`UPDATE collections SET updation_time = 10`)
	require.NoError(t, err)

	lines := make([]string, 0)
	for _, tt := range []struct {
		app           ente.App
		clientPackage string
		id            int64
	}{
		{ente.Photos, "io.ente.photos", photosID},
		{ente.Locker, "io.ente.locker", lockerID},
	} {
		for name, value := range f.responses(t, tt.id, tt.clientPackage) {
			lines = append(lines, fmt.Sprintf("%s %s: %s", tt.app, name, value))
		}
	}
	updationTime := regexp.MustCompile(`"updationTime":\d+`)
	for _, tt := range []struct {
		app           ente.App
		clientPackage string
		createdType   string
	}{
		{ente.Photos, "io.ente.photos", "album"},
		{ente.Locker, "io.ente.locker", "folder"},
	} {
		for i, collectionType := range []string{tt.createdType, "favorites", "favorites", "uncategorized", "uncategorized"} {
			body := fmt.Sprintf(`{"encryptedKey":%q,"keyDecryptionNonce":%q,"encryptedName":"name","nameDecryptionNonce":"nonce","type":%q}`,
				base64.StdEncoding.EncodeToString(make([]byte, 48)), base64.StdEncoding.EncodeToString(make([]byte, 24)), collectionType)
			response := f.request(t, http.MethodPost, "/collections", parentTestOwnerID, tt.clientPackage, body)
			lines = append(lines, fmt.Sprintf("%s create %d %s: %s", tt.app, i, collectionType,
				updationTime.ReplaceAll(response, []byte(`"updationTime":0`))))
		}
	}
	sort.Strings(lines)
	golden, err := os.ReadFile("pkg/api/testdata/existing_collection_responses.golden")
	require.NoError(t, err)
	want := strings.ReplaceAll(strings.TrimSpace(string(golden)), "https://share.ente.com/c/LOCKERLINK",
		f.collectionRepo.CollectionLinkRepo.GetAlbumUrl(ente.Locker, "LOCKERLINK"))
	require.Equal(t, want, strings.Join(lines, "\n"))
}

func TestParentFieldsAreOwnerOnly(t *testing.T) {
	f := setupCollectionResponseFixture(t)
	rootID := f.createDriveFolder(t, nil)
	childID := f.createDriveFolder(t, &rootID)
	f.shareAndLink(t, childID, "DRIVELINK")

	ownerFields := fmt.Sprintf(`"parentID":%d,"parentEncryptedKey":"parent-key","parentKeyNonce":"parent-nonce"`, rootID)
	for name, body := range f.responses(t, childID, "io.ente.drive") {
		require.NotEmpty(t, body, name)
		if strings.HasPrefix(name, "owner") {
			require.Contains(t, body, ownerFields, name)
		} else {
			require.NotContains(t, body, "parent", name)
		}
	}
	rootOwnerResponse := string(f.request(t, http.MethodGet, fmt.Sprintf("/collections/%d", rootID), parentTestOwnerID, "io.ente.drive", ""))
	require.NotContains(t, rootOwnerResponse, "parent")
}

func createBody(collectionType, parentFields string) string {
	return fmt.Sprintf(`{"encryptedKey":%q,"keyDecryptionNonce":%q,"encryptedName":"name","nameDecryptionNonce":"nonce","type":%q%s}`,
		base64.StdEncoding.EncodeToString(make([]byte, 48)), base64.StdEncoding.EncodeToString(make([]byte, 24)), collectionType, parentFields)
}

func TestNonDriveCreateIgnoresClientParentFields(t *testing.T) {
	f := setupCollectionResponseFixture(t)
	rootID := f.createDriveFolder(t, nil)
	normalize := regexp.MustCompile(`"(id|updationTime)":\d+`)
	for _, clientPackage := range []string{"io.ente.photos", "io.ente.locker"} {
		for _, collectionType := range []string{"album", "folder"} {
			plain := f.request(t, http.MethodPost, "/collections", parentTestOwnerID, clientPackage, createBody(collectionType, ""))
			withParent := f.request(t, http.MethodPost, "/collections", parentTestOwnerID, clientPackage, createBody(collectionType,
				fmt.Sprintf(`,"parentID":%d,"parentEncryptedKey":"parent-key","parentKeyNonce":"parent-nonce"`, rootID)))
			require.Equal(t, string(normalize.ReplaceAll(plain, nil)), string(normalize.ReplaceAll(withParent, nil)))
		}
	}
	var withParentColumns int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections
		WHERE parent_id IS NOT NULL OR parent_encrypted_key IS NOT NULL OR parent_key_nonce IS NOT NULL`).Scan(&withParentColumns))
	require.Zero(t, withParentColumns)
}

func TestDriveCreateAndMoveWithParent(t *testing.T) {
	f := setupCollectionResponseFixture(t)
	f.router.POST("/collections/move-collection", (&CollectionHandler{Controller: &collections.CollectionController{
		CollectionRepo: f.collectionRepo,
	}}).MoveCollection)
	rootID, otherRootID := f.createDriveFolder(t, nil), f.createDriveFolder(t, nil)
	parentKey, parentNonce := base64.StdEncoding.EncodeToString(make([]byte, 48)), base64.StdEncoding.EncodeToString(make([]byte, 24))
	parentFields := fmt.Sprintf(`"parentEncryptedKey":%q,"parentKeyNonce":%q`, parentKey, parentNonce)

	response := f.request(t, http.MethodPost, "/collections", parentTestOwnerID, "io.ente.drive",
		createBody("folder", fmt.Sprintf(`,"parentID":%d,%s`, rootID, parentFields)))
	require.Contains(t, string(response), fmt.Sprintf(`"parentID":%d,%s`, rootID, parentFields))
	var created struct {
		Collection ente.Collection `json:"collection"`
	}
	require.NoError(t, json.Unmarshal(response, &created))
	childID := created.Collection.ID
	f.shareAndLink(t, childID, "DRIVELINK")

	recorder := f.do(http.MethodPost, "/collections", parentTestOwnerID, "io.ente.drive",
		createBody("folder", fmt.Sprintf(`,"parentID":%d,%s`, int64(1_000_000), parentFields)))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"INVALID_PARENT"`)

	var sinceTime int64
	require.NoError(t, f.db.QueryRow(`SELECT max(updation_time) FROM collections`).Scan(&sinceTime))
	recorder = f.do(http.MethodPost, "/collections/move-collection", parentTestOwnerID, "io.ente.drive",
		fmt.Sprintf(`{"collectionID":%d,"newParentID":%d,%s}`, childID, otherRootID, parentFields))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Empty(t, recorder.Body.String())

	ownerDiff := syncedCollectionsByID(t, f.request(t, http.MethodGet, fmt.Sprintf("/collections/v2?sinceTime=%d", sinceTime),
		parentTestOwnerID, "io.ente.drive", ""), "collections")
	require.Len(t, ownerDiff, 1)
	require.Contains(t, ownerDiff[childID], fmt.Sprintf(`"parentID":%d,%s`, otherRootID, parentFields))
	for name, body := range f.responses(t, childID, "io.ente.drive") {
		if !strings.HasPrefix(name, "owner") {
			require.NotContains(t, body, "parent", name)
		}
	}

	recorder = f.do(http.MethodPost, "/collections/move-collection", parentTestOwnerID, "io.ente.drive",
		fmt.Sprintf(`{"collectionID":%d,"newParentID":%d,%s}`, otherRootID, childID, parentFields))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"COLLECTION_CYCLE"`)

	recorder = f.do(http.MethodPost, "/collections/move-collection", parentTestOwnerID, "io.ente.drive",
		fmt.Sprintf(`{"collectionID":%d}`, childID))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"BAD_REQUEST"`)
	var storedParent int64
	require.NoError(t, f.db.QueryRow(`SELECT parent_id FROM collections WHERE collection_id = $1`, childID).Scan(&storedParent))
	require.Equal(t, otherRootID, storedParent)

	recorder = f.do(http.MethodPost, "/collections/move-collection", parentTestOwnerID, "io.ente.drive",
		fmt.Sprintf(`{"collectionID":%d,"newParentID":null}`, childID))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	ownerView := string(f.request(t, http.MethodGet, fmt.Sprintf("/collections/%d", childID), parentTestOwnerID, "io.ente.drive", ""))
	require.NotContains(t, ownerView, "parent")
}
