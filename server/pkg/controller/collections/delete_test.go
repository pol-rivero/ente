package collections

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/stacktrace"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func setupDeleteFixture(t *testing.T) *treeFixture {
	t.Helper()
	f := setupTreeFixture(t)
	gin.SetMode(gin.TestMode)
	clearQueue := func() {
		_, err := f.db.Exec(`DELETE FROM queue WHERE queue_name IN ($1, $2)`, repo.TrashCollectionQueueV3, repo.TrashCollectionDriveQueue)
		require.NoError(t, err)
	}
	clearQueue()
	t.Cleanup(clearQueue)
	return f
}

func setViper(t *testing.T, key string, value any) {
	t.Helper()
	previous := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() { viper.Set(key, previous) })
}

func (f *treeFixture) ginContext(userID int64) *gin.Context {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/collections/v3/1", nil)
	ctx.Request.Header.Set("X-Auth-User-ID", strconv.FormatInt(userID, 10))
	return ctx
}

func (f *treeFixture) deleteV3(id int64, keepFiles bool) error {
	return f.deleteV3As(treeTestOwnerID, id, keepFiles)
}

func (f *treeFixture) deleteV3As(userID, id int64, keepFiles bool) error {
	return f.ctrl.TrashV3(f.ginContext(userID), ente.TrashCollectionV3Request{CollectionID: id, KeepFiles: &keepFiles})
}

func (f *treeFixture) deleteV4(id int64, keepFiles, recursive bool) error {
	return f.deleteV4As(treeTestOwnerID, id, keepFiles, recursive)
}

func (f *treeFixture) deleteV4As(userID, id int64, keepFiles, recursive bool) error {
	return f.ctrl.TrashV4(context.Background(), userID, id, keepFiles, recursive)
}

func (f *treeFixture) insertFolder(ownerID int64, parentID *int64) int64 {
	f.t.Helper()
	var id int64
	var key *string
	if parentID != nil {
		key = treeKey(1)
	}
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes,
			updation_time, app, parent_id, parent_encrypted_key, parent_key_nonce)
		VALUES ($1, 'key', 'nonce', 'name', 'folder', '{}', 1, 'drive', $2, $3, $3) RETURNING collection_id`,
		ownerID, parentID, key).Scan(&id))
	return id
}

// Root, two children, each with two grandchildren.
func (f *treeFixture) threeLevelTree() []int64 {
	f.t.Helper()
	root := f.folder(nil)
	ids := []int64{root}
	for range 2 {
		child := f.folder(&root)
		ids = append(ids, child, f.folder(&child), f.folder(&child))
	}
	return ids
}

func (f *treeFixture) isDeleted(id int64) bool {
	f.t.Helper()
	var deleted bool
	require.NoError(f.t, f.db.QueryRow(`SELECT is_deleted FROM collections WHERE collection_id = $1`, id).Scan(&deleted))
	return deleted
}

func (f *treeFixture) queued(queueName string) []int64 {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT item FROM queue WHERE queue_name = $1 AND is_deleted = FALSE`, queueName)
	require.NoError(f.t, err)
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var item string
		require.NoError(f.t, rows.Scan(&item))
		id, err := strconv.ParseInt(item, 10, 64)
		require.NoError(f.t, err)
		ids = append(ids, id)
	}
	require.NoError(f.t, rows.Err())
	return ids
}

func (f *treeFixture) addFile(collectionID int64) int64 {
	return f.addFileOf(collectionID, ente.Drive)
}

func (f *treeFixture) addFileOf(collectionID int64, app ente.App) int64 {
	f.t.Helper()
	var fileID int64
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, treeTestOwnerID, app).Scan(&fileID))
	_, err := f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, collectionID, fileID, treeTestOwnerID)
	require.NoError(f.t, err)
	return fileID
}

func (f *treeFixture) insertUser(userID int64) int64 {
	f.t.Helper()
	return testutil.InsertUser(f.t, f.db, testutil.UserFixture{UserID: userID, Email: fmt.Sprintf("delete-%d@example.com", userID), CreationTime: 1})
}

func (f *treeFixture) addLinkAndCast(collectionID int64) string {
	f.t.Helper()
	token := fmt.Sprintf("token-%d", collectionID)
	_, err := f.db.Exec(`INSERT INTO public_collection_tokens(collection_id, access_token) VALUES ($1, $2)`, collectionID, token)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO casting(id, code, public_key, collection_id, cast_user, ip)
		SELECT $1, $2, 'key', collection_id, owner_id, 'ip' FROM collections WHERE collection_id = $3`,
		uuid.New(), fmt.Sprintf("C%d", collectionID), collectionID)
	require.NoError(f.t, err)
	f.ctrl.CollectionRepo.CollectionLinkRepo.Cache.Set("summary:"+token, "cached", gTime.Now())
	return token
}

func (f *treeFixture) requireRevoked(collectionID int64, token string, want bool) {
	f.t.Helper()
	var linkDisabled, castDeleted bool
	require.NoError(f.t, f.db.QueryRow(`SELECT is_disabled FROM public_collection_tokens WHERE access_token = $1`, token).Scan(&linkDisabled))
	require.NoError(f.t, f.db.QueryRow(`SELECT is_deleted FROM casting WHERE collection_id = $1`, collectionID).Scan(&castDeleted))
	_, cached := f.ctrl.CollectionRepo.CollectionLinkRepo.Cache.Get(token, "summary:"+token)
	require.Equal(f.t, want, linkDisabled, "link of %d", collectionID)
	require.Equal(f.t, want, castDeleted, "cast of %d", collectionID)
	require.Equal(f.t, !want, cached, "cached link of %d", collectionID)
}

func (f *treeFixture) requireUnchanged(ids []int64, tokens map[int64]string) {
	f.t.Helper()
	for _, id := range ids {
		require.False(f.t, f.isDeleted(id))
		if token, ok := tokens[id]; ok {
			f.requireRevoked(id, token, false)
		}
	}
	require.Empty(f.t, f.queued(repo.TrashCollectionDriveQueue))
}

func TestRecursiveDeleteRemovesTheTreeFromSyncInOneStep(t *testing.T) {
	f := setupDeleteFixture(t)
	other := f.folder(nil)
	tree := f.threeLevelTree()
	for _, id := range []int64{tree[1], tree[2]} {
		require.NoError(t, f.ctrl.CollectionRepo.Share(id, treeTestOwnerID, treeTestOtherID, "share-key", ente.VIEWER, 5))
	}
	since := f.stored(tree[len(tree)-1]).updationTime

	require.NoError(t, f.deleteV4(tree[0], false, true))

	owned, err := f.ctrl.CollectionRepo.GetCollectionsOwnedByUserV2(treeTestOwnerID, since, ente.Drive, nil)
	require.NoError(t, err)
	require.Len(t, owned, len(tree))
	for _, collection := range owned {
		require.Contains(t, tree, collection.ID)
		require.True(t, collection.IsDeleted)
		require.Equal(t, owned[0].UpdationTime, collection.UpdationTime)
	}
	shared, err := f.ctrl.CollectionRepo.GetCollectionsSharedWithUser(treeTestOtherID, since, ente.Drive, nil)
	require.NoError(t, err)
	require.Len(t, shared, 2)
	for _, collection := range shared {
		require.True(t, collection.IsDeleted)
	}
	require.ElementsMatch(t, tree, f.queued(repo.TrashCollectionDriveQueue))
	require.Empty(t, f.queued(repo.TrashCollectionQueueV3))
	require.False(t, f.isDeleted(other))

	require.NoError(t, f.deleteV4(tree[0], false, true))
	require.NoError(t, f.deleteV3(tree[3], false))
}

func TestV3DeleteOfDriveFolders(t *testing.T) {
	f := setupDeleteFixture(t)
	parent := f.folder(nil)
	child := f.folder(&parent)
	f.addFile(child)

	requireTreeAPIError(t, f.deleteV3(parent, true), http.StatusConflict, ente.HasChildren)
	requireTreeAPIError(t, f.deleteV3(parent, false), http.StatusConflict, ente.HasChildren)
	requireTreeAPIError(t, f.deleteV4(parent, false, false), http.StatusConflict, ente.HasChildren)
	requireTreeAPIError(t, f.deleteV3(child, true), http.StatusConflict, ente.CollectionNotEmpty)
	f.requireUnchanged([]int64{parent, child}, nil)

	require.NoError(t, f.deleteV3(child, false))
	require.True(t, f.isDeleted(child))
	require.NoError(t, f.deleteV3(parent, true))
	require.True(t, f.isDeleted(parent))
	require.ElementsMatch(t, []int64{child, parent}, f.queued(repo.TrashCollectionDriveQueue))
	require.Empty(t, f.queued(repo.TrashCollectionQueueV3))
	require.NoError(t, f.deleteV3(parent, true))

	uncategorized := f.insert(treeTestOwnerID, ente.Drive, "uncategorized")
	require.ErrorIs(t, f.deleteV3(uncategorized, false), ente.ErrBadRequest)
	require.ErrorIs(t, f.deleteV4(uncategorized, false, true), ente.ErrNotDeletableWithV4)
}

func TestV3DeleteOfPhotosAndLockerCollectionsIsUnchanged(t *testing.T) {
	f := setupDeleteFixture(t)
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		nonEmpty := f.insert(treeTestOwnerID, app, "folder")
		f.addFileOf(nonEmpty, app)
		requireTreeAPIError(t, f.deleteV3(nonEmpty, true), http.StatusConflict, ente.CollectionNotEmpty)

		album := f.insert(treeTestOwnerID, app, "album")
		require.NoError(t, f.ctrl.CollectionRepo.Share(album, treeTestOwnerID, treeTestOtherID, "share-key", ente.VIEWER, 5))
		token := f.addLinkAndCast(album)
		holder := holdCollectionTreeLock(t, f.db, treeTestOwnerID)
		require.NoError(t, f.deleteV3(album, true))
		require.NoError(t, holder.Rollback())
		require.True(t, f.isDeleted(album))
		f.requireRevoked(album, token, true)
		var shareDeleted bool
		require.NoError(t, f.db.QueryRow(`SELECT is_deleted FROM collection_shares WHERE collection_id = $1`, album).Scan(&shareDeleted))
		require.True(t, shareDeleted)
		require.Contains(t, f.queued(repo.TrashCollectionQueueV3), album)

		require.ErrorIs(t, f.deleteV4(f.insert(treeTestOwnerID, app, "folder"), false, false), ente.ErrNotDeletableWithV4)
	}
	require.Empty(t, f.queued(repo.TrashCollectionDriveQueue))
}

func TestDeleteRejectsInvalidRequests(t *testing.T) {
	f := setupDeleteFixture(t)
	folder := f.folder(nil)
	othersFolder := f.insert(treeTestOtherID, ente.Drive, "folder")
	requireTreeAPIError(t, f.deleteV4(othersFolder, false, true), http.StatusNotFound, ente.NotFoundError)
	require.ErrorIs(t, f.deleteV3(othersFolder, false), ente.ErrPermissionDenied)
	requireTreeAPIError(t, f.deleteV4(1_000_000, false, true), http.StatusNotFound, ente.NotFoundError)
	require.False(t, f.isDeleted(folder))
	require.False(t, f.isDeleted(othersFolder))
}

func TestRecursiveDeleteSubtreeLimit(t *testing.T) {
	f := setupDeleteFixture(t)
	tree := f.threeLevelTree()
	tokens := map[int64]string{}
	for _, id := range tree {
		tokens[id] = f.addLinkAndCast(id)
	}
	setViper(t, "collections.max-recursive-delete", len(tree)-1)
	requireTreeAPIError(t, f.deleteV4(tree[0], false, true), http.StatusBadRequest, ente.SubtreeTooLarge)
	f.requireUnchanged(tree, tokens)

	setViper(t, "collections.max-recursive-delete", len(tree))
	require.NoError(t, f.deleteV4(tree[0], false, true))
	for _, id := range tree {
		require.True(t, f.isDeleted(id))
		f.requireRevoked(id, tokens[id], true)
	}
}

func TestRecursiveDeleteKeepFilesNeedsAnEmptySubtree(t *testing.T) {
	f := setupDeleteFixture(t)
	tree := f.threeLevelTree()
	fileID := f.addFile(tree[len(tree)-1])
	requireTreeAPIError(t, f.deleteV4(tree[0], true, true), http.StatusConflict, ente.CollectionNotEmpty)
	f.requireUnchanged(tree, nil)

	_, err := f.db.Exec(`UPDATE collection_files SET is_deleted = TRUE WHERE file_id = $1`, fileID)
	require.NoError(t, err)
	require.NoError(t, f.deleteV4(tree[0], true, true))
	require.ElementsMatch(t, tree, f.queued(repo.TrashCollectionDriveQueue))
}

func TestDeletesDontReachLiveFoldersUnderDeletedOnes(t *testing.T) {
	f := setupDeleteFixture(t)
	for _, recursive := range []bool{false, true} {
		chain := f.chain(4)
		f.markDeleted(chain[1])

		require.NoError(t, f.deleteV4(chain[0], false, recursive))
		require.True(t, f.isDeleted(chain[0]))
		require.False(t, f.isDeleted(chain[2]))
		require.False(t, f.isDeleted(chain[3]))
		require.Contains(t, f.queued(repo.TrashCollectionDriveQueue), chain[0])
		require.NotContains(t, f.queued(repo.TrashCollectionDriveQueue), chain[2])
	}
}

func TestConcurrentV3DeleteAndCreateNeverLeaveALiveChildOfADeletedParent(t *testing.T) {
	f := setupDeleteFixture(t)
	for range 50 {
		parent := f.folder(nil)
		errs := runConcurrently(
			func() error {
				_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &parent))
				return err
			},
			func() error { return f.deleteV3(parent, true) },
		)
		if errs[1] == nil {
			requireTreeAPIError(t, errs[0], http.StatusBadRequest, ente.InvalidParent)
		} else {
			require.NoError(t, errs[0])
			requireTreeAPIError(t, errs[1], http.StatusConflict, ente.HasChildren)
		}
	}
	var orphans int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections c JOIN collections p ON p.collection_id = c.parent_id
		WHERE NOT c.is_deleted AND p.is_deleted`).Scan(&orphans))
	require.Zero(t, orphans)
}

func TestRecursiveDeleteGetsALongerBudget(t *testing.T) {
	f := setupDeleteFixture(t)
	tree := f.threeLevelTree()
	setCollectionTreeLimits(t, maxConcurrentTreeChanges, 300*gTime.Millisecond)
	setViper(t, "collections.recursive-delete-timeout-seconds", 5)
	holder := holdCollectionTreeLock(t, f.db, treeTestOwnerID)
	go func() {
		gTime.Sleep(gTime.Second)
		_ = holder.Rollback()
	}()
	errs := runConcurrently(
		func() error { return f.deleteV4(tree[0], false, true) },
		func() error { return f.deleteV3(tree[len(tree)-1], false) },
	)
	require.NoError(t, errs[0])
	requireTreeAPIError(t, errs[1], http.StatusServiceUnavailable, ente.CollectionTreeBusy)
	require.ElementsMatch(t, tree, f.queued(repo.TrashCollectionDriveQueue))
}

func TestConcurrentDeletesDontExhaustThePool(t *testing.T) {
	f := setupDeleteFixture(t)
	const poolSize = 4
	owners := make([]int64, 0, 2*poolSize)
	for i := range int64(2 * poolSize) {
		owners = append(owners, f.insertUser(100+i))
	}
	fns := make([]func() error, 0, 4*len(owners))
	for _, owner := range owners {
		for range 2 {
			root := f.insertFolder(owner, nil)
			child := f.insertFolder(owner, &root)
			f.insertFolder(owner, &child)
			f.addLinkAndCast(child)
			leaf := f.insertFolder(owner, nil)
			f.addLinkAndCast(leaf)
			fns = append(fns,
				func() error { return f.deleteV4As(owner, root, false, true) },
				func() error { return f.deleteV3As(owner, leaf, true) },
			)
		}
	}
	pool, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(poolSize)
	f.ctrl = newTreeTestController(pool)
	setCollectionTreeLimits(t, maxConcurrentTreeChanges, 10*gTime.Second)
	setViper(t, "collections.recursive-delete-timeout-seconds", 10)

	for _, err := range runConcurrently(fns...) {
		require.NoError(t, err)
	}
	var live int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections WHERE owner_id >= 100 AND NOT is_deleted`).Scan(&live))
	require.Zero(t, live)
}

func TestRecursiveDeleteOfTheMaximumSubtree(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	f := setupDeleteFixture(t)
	root := f.folder(nil)
	const fanOut = 100
	_, err := f.db.Exec(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app,
			parent_id, parent_encrypted_key, parent_key_nonce)
		SELECT $1, 'key', 'nonce', 'name', 'folder', '{}', 1, 'drive', $2, 'key', 'nonce' FROM generate_series(1, $3)`,
		treeTestOwnerID, root, fanOut-1)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO collections(owner_id, encrypted_key, key_decryption_nonce, name, type, attributes, updation_time, app,
			parent_id, parent_encrypted_key, parent_key_nonce)
		SELECT $1, 'key', 'nonce', 'name', 'folder', '{}', 1, 'drive', c.collection_id, 'key', 'nonce'
		FROM collections c, generate_series(1, $3)
		WHERE c.parent_id = $2`, treeTestOwnerID, root, fanOut)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO public_collection_tokens(collection_id, access_token)
		SELECT collection_id, 'token-' || collection_id FROM collections WHERE collection_id % 10 = 0`)
	require.NoError(t, err)
	_, err = f.db.Exec(`ANALYZE collections`)
	require.NoError(t, err)
	var total int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections WHERE NOT is_deleted`).Scan(&total))
	require.Equal(t, defaultMaxRecursiveDelete, total)

	start := gTime.Now()
	require.NoError(t, f.deleteV4(root, false, true))
	elapsed := gTime.Since(start)
	t.Logf("recursive delete of %d folders took %v", total, elapsed)
	require.Less(t, elapsed, defaultRecursiveDeleteTimeout/3)
	require.Len(t, f.queued(repo.TrashCollectionDriveQueue), total)
	var live, activeLinks int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections WHERE NOT is_deleted`).Scan(&live))
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM public_collection_tokens WHERE NOT is_disabled`).Scan(&activeLinks))
	require.Zero(t, live)
	require.Zero(t, activeLinks)
	require.True(t, slices.Contains(f.queued(repo.TrashCollectionDriveQueue), root))
}

func (f *treeFixture) link(fileID, collectionID int64) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		VALUES ($1, $2, 'key', 'nonce', 1, $3, $3)`, collectionID, fileID, treeTestOwnerID)
	require.NoError(f.t, err)
}

func (f *treeFixture) liveIn(fileID int64) []int64 {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT collection_id FROM collection_files WHERE file_id = $1 AND NOT is_deleted`, fileID)
	require.NoError(f.t, err)
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		require.NoError(f.t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(f.t, rows.Err())
	return ids
}

func (f *treeFixture) inTrash(fileID int64) bool {
	f.t.Helper()
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM trash WHERE file_id = $1 AND NOT is_restored AND NOT is_deleted`, fileID).Scan(&count))
	return count == 1
}

func (f *treeFixture) drainDriveTrash() {
	f.t.Helper()
	trashCtrl := &controller.TrashController{
		TrashRepo:      f.ctrl.CollectionRepo.TrashRepo,
		FileRepo:       f.ctrl.CollectionRepo.FileRepo,
		CollectionRepo: f.ctrl.CollectionRepo,
		QueueRepo:      f.ctrl.QueueRepo,
		TaskLockRepo:   &repo.TaskLockRepository{DB: f.db},
		HostName:       "delete-test",
	}
	trashCtrl.CleanupTrashedDriveCollections()
	require.Empty(f.t, f.queued(repo.TrashCollectionDriveQueue))
}

func TestRecursiveDeleteUnlinksFilesThatAreStillInAnotherLiveFolder(t *testing.T) {
	f := setupDeleteFixture(t)
	tree := f.threeLevelTree()
	outside := f.folder(nil)
	alsoOutside := f.addFile(tree[len(tree)-1])
	f.link(alsoOutside, outside)
	onlyInTree := f.addFile(tree[1])
	inTwoTreeFolders := f.addFile(tree[2])
	f.link(inTwoTreeFolders, tree[3])

	require.NoError(t, f.deleteV4(tree[0], false, true))
	f.drainDriveTrash()

	require.False(t, f.inTrash(alsoOutside))
	require.Equal(t, []int64{outside}, f.liveIn(alsoOutside))
	for _, fileID := range []int64{onlyInTree, inTwoTreeFolders} {
		require.True(t, f.inTrash(fileID))
		require.Empty(t, f.liveIn(fileID))
	}
}

func TestOverlappingRecursiveDeletesOfOneTree(t *testing.T) {
	f := setupDeleteFixture(t)
	for range 20 {
		tree := f.threeLevelTree()
		errs := runConcurrently(
			func() error { return f.deleteV4(tree[0], false, true) },
			func() error { return f.deleteV4(tree[1], false, true) },
			func() error { return f.deleteV4(tree[4], true, true) },
			func() error { return f.deleteV4(tree[6], false, false) },
		)
		for _, err := range errs {
			require.NoError(t, err)
		}
		for _, id := range tree {
			require.True(t, f.isDeleted(id))
		}
		require.Subset(t, f.queued(repo.TrashCollectionDriveQueue), tree)
	}
	var items, distinct int
	require.NoError(t, f.db.QueryRow(`SELECT count(*), count(DISTINCT item) FROM queue WHERE queue_name = $1`,
		repo.TrashCollectionDriveQueue).Scan(&items, &distinct))
	require.Equal(t, 20*7, items)
	require.Equal(t, items, distinct)
}

func TestRecursiveDeletesUseTheirOwnSlots(t *testing.T) {
	f := setupDeleteFixture(t)
	parent := f.folder(nil)
	child := f.folder(&parent)
	setCollectionTreeLimits(t, maxConcurrentTreeChanges, 300*gTime.Millisecond)
	for range maxConcurrentTreeChanges {
		collectionTreeSlots <- struct{}{}
	}
	requireTreeAPIError(t, f.deleteV4(child, false, false), http.StatusServiceUnavailable, ente.CollectionTreeBusy)
	require.NoError(t, f.deleteV4(parent, false, true))
	require.True(t, f.isDeleted(child))
}

func TestTreeChangeDeadlocksAreReportedAsBusy(t *testing.T) {
	f := setupDeleteFixture(t)
	err := f.ctrl.changeCollectionTreeWithin(context.Background(), treeTestOwnerID, collectionTreeSlots, collectionTreeTimeout,
		func(context.Context, *sql.Tx) error {
			return stacktrace.Propagate(&pq.Error{Code: "40P01"}, "")
		})
	requireTreeAPIError(t, err, http.StatusServiceUnavailable, ente.CollectionTreeBusy)
}
