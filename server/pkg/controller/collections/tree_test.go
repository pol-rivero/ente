package collections

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	gTime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/controller/access"
	publicCtrl "github.com/ente/museum/pkg/controller/public"
	"github.com/ente/museum/pkg/repo"
	castRepo "github.com/ente/museum/pkg/repo/cast"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/stretchr/testify/require"
)

const (
	treeTestOwnerID = int64(1)
	treeTestOtherID = int64(2)
)

type treeFixture struct {
	t    *testing.T
	db   *sql.DB
	ctrl *CollectionController
}

func setupTreeFixture(t *testing.T) *treeFixture {
	t.Helper()
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	for _, userID := range []int64{treeTestOwnerID, treeTestOtherID} {
		testutil.InsertUser(t, db, testutil.UserFixture{UserID: userID, Email: fmt.Sprintf("tree-%d@example.com", userID), CreationTime: 1})
		_, err := db.Exec(`INSERT INTO key_attributes(user_id, kek_salt, encrypted_key, key_decryption_nonce,
			public_key, encrypted_secret_key, secret_key_decryption_nonce, mem_limit, ops_limit)
			VALUES ($1, 'salt', 'key', 'nonce', 'public', 'secret', 'secret-nonce', 1, 1)`, userID)
		require.NoError(t, err)
	}
	return &treeFixture{t: t, db: db, ctrl: newTreeTestController(db)}
}

func newTreeTestController(db *sql.DB) *CollectionController {
	linkRepo := public.NewCollectionLinkRepository(db, "")
	linkRepo.Cache = public.NewLinkCache(gTime.Minute, gTime.Minute)
	queueRepo := &repo.QueueRepository{DB: db}
	fileRepo := &repo.FileRepository{DB: db, QueueRepo: queueRepo}
	collectionRepo := &repo.CollectionRepository{DB: db, CollectionLinkRepo: linkRepo, QueueRepo: queueRepo, FileRepo: fileRepo,
		TrashRepo: &repo.TrashRepository{DB: db, QueueRepo: queueRepo, FileRepo: fileRepo, FileLinkRepo: public.NewFileLinkRepo(db)}}
	return &CollectionController{
		CollectionRepo:     collectionRepo,
		UserRepo:           &repo.UserRepository{DB: db},
		AccessCtrl:         access.NewAccessController(collectionRepo, fileRepo),
		CollectionLinkCtrl: &publicCtrl.CollectionLinkController{CollectionLinkRepo: linkRepo},
		CastRepo:           &castRepo.Repository{DB: db},
		QueueRepo:          queueRepo,
	}
}

func treeKey(fill byte) *string {
	key := b64OfLenFilled(encryptedCollectionKeyLen, fill)
	return &key
}

func treeNonce(fill byte) *string {
	nonce := b64OfLenFilled(secretboxNonceBytes, fill)
	return &nonce
}

func b64OfLenFilled(length int, fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, length))
}

func toParent(id *int64) ente.NullableInt64 {
	return ente.NullableInt64{Present: true, Value: id}
}

func treeCollection(ownerID int64, app ente.App, collectionType string, parentID *int64) ente.Collection {
	collection := ente.Collection{
		Owner:               ente.CollectionUser{ID: ownerID},
		EncryptedKey:        b64OfLen(encryptedCollectionKeyLen),
		KeyDecryptionNonce:  b64OfLen(secretboxNonceBytes),
		EncryptedName:       "name",
		NameDecryptionNonce: "nonce",
		Type:                collectionType,
		App:                 string(app),
	}
	if parentID != nil {
		collection.ParentID, collection.ParentEncryptedKey, collection.ParentKeyNonce = parentID, treeKey(1), treeNonce(1)
	}
	return collection
}

func (f *treeFixture) create(collection ente.Collection) (ente.Collection, error) {
	return f.ctrl.Create(context.Background(), collection, collection.Owner.ID)
}

func (f *treeFixture) folder(parentID *int64) int64 {
	f.t.Helper()
	created, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", parentID))
	require.NoError(f.t, err)
	return created.ID
}

func (f *treeFixture) chain(length int) []int64 {
	f.t.Helper()
	ids := make([]int64, 0, length)
	var parent *int64
	for range length {
		id := f.folder(parent)
		ids = append(ids, id)
		parent = &id
	}
	return ids
}

func (f *treeFixture) insert(ownerID int64, app ente.App, collectionType string) int64 {
	f.t.Helper()
	return testutil.InsertCollection(f.t, f.db, ownerID, app, collectionType)
}

func (f *treeFixture) markDeleted(id int64) {
	f.t.Helper()
	_, err := f.db.Exec(`UPDATE collections SET is_deleted = TRUE WHERE collection_id = $1`, id)
	require.NoError(f.t, err)
}

func (f *treeFixture) move(id int64, newParentID *int64) error {
	req := ente.MoveCollectionRequest{CollectionID: id, NewParentID: toParent(newParentID)}
	if newParentID != nil {
		req.ParentEncryptedKey, req.ParentKeyNonce = treeKey(2), treeNonce(2)
	}
	return f.ctrl.MoveCollection(context.Background(), treeTestOwnerID, req)
}

type storedParent struct {
	id           sql.NullInt64
	key, nonce   sql.NullString
	updationTime int64
}

func (f *treeFixture) stored(id int64) storedParent {
	f.t.Helper()
	var p storedParent
	require.NoError(f.t, f.db.QueryRow(`SELECT parent_id, parent_encrypted_key, parent_key_nonce, updation_time
		FROM collections WHERE collection_id = $1`, id).Scan(&p.id, &p.key, &p.nonce, &p.updationTime))
	return p
}

func (f *treeFixture) collectionCount() int {
	f.t.Helper()
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM collections`).Scan(&count))
	return count
}

func (f *treeFixture) maxLiveDepth() int {
	f.t.Helper()
	var depth int
	require.NoError(f.t, f.db.QueryRow(`WITH RECURSIVE tree(id, d) AS (
			SELECT collection_id, 1 FROM collections WHERE parent_id IS NULL AND app = 'drive' AND NOT is_deleted
			UNION ALL
			SELECT c.collection_id, t.d + 1 FROM collections c JOIN tree t ON c.parent_id = t.id
			WHERE NOT c.is_deleted AND t.d < 1000)
		SELECT max(d) FROM tree`).Scan(&depth))
	return depth
}

func (f *treeFixture) requireNoCycle(ids ...int64) {
	f.t.Helper()
	for _, id := range ids {
		seen := map[int64]bool{}
		for current := id; ; {
			require.False(f.t, seen[current], "cycle through %d", current)
			seen[current] = true
			parent := f.stored(current).id
			if !parent.Valid {
				break
			}
			current = parent.Int64
		}
	}
}

func setCollectionTreeLimits(t *testing.T, slots int, timeout gTime.Duration) {
	t.Helper()
	previousSlots, previousOwnerSlots, previousTimeout := collectionTreeSlots, collectionTreeOwnerSlots, collectionTreeTimeout
	collectionTreeSlots, collectionTreeTimeout = make(chan struct{}, slots), timeout
	collectionTreeOwnerSlots = controller.NewKeyedSlots(maxConcurrentTreeChangesPerOwner)
	t.Cleanup(func() {
		collectionTreeSlots, collectionTreeOwnerSlots, collectionTreeTimeout = previousSlots, previousOwnerSlots, previousTimeout
	})
}

func TestCreateDriveFolderWithParent(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	created, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &root))
	require.NoError(t, err)
	require.Equal(t, root, *created.ParentID)
	require.Equal(t, *treeKey(1), *created.ParentEncryptedKey)
	require.Equal(t, *treeNonce(1), *created.ParentKeyNonce)
	stored := f.stored(created.ID)
	require.Equal(t, root, stored.id.Int64)
	require.Equal(t, *treeKey(1), stored.key.String)
	require.Equal(t, *treeNonce(1), stored.nonce.String)
	require.Equal(t, created.UpdationTime, stored.updationTime)

	rootCollection, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", nil))
	require.NoError(t, err)
	require.Nil(t, rootCollection.ParentID)
	require.False(t, f.stored(rootCollection.ID).id.Valid)
}

func TestCreateWithParentRejectsInvalidRequests(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	deleted := f.folder(nil)
	f.markDeleted(deleted)
	missing := int64(1_000_000)
	invalidParents := map[string]int64{
		"other user's folder": f.insert(treeTestOtherID, ente.Drive, "folder"),
		"deleted folder":      deleted,
		"missing":             missing,
		"drive album":         f.insert(treeTestOwnerID, ente.Drive, "album"),
		"drive uncategorized": f.insert(treeTestOwnerID, ente.Drive, "uncategorized"),
		"photos folder":       f.insert(treeTestOwnerID, ente.Photos, "folder"),
		"locker folder":       f.insert(treeTestOwnerID, ente.Locker, "folder"),
	}
	before := f.collectionCount()
	for name, parentID := range invalidParents {
		_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &parentID))
		testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.InvalidParent)
		require.Equal(t, before, f.collectionCount(), name)
	}
	for _, collectionType := range []string{"album", "favorites", "uncategorized"} {
		_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, collectionType, &root))
		testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.InvalidParent)
	}
	shortKey := b64OfLen(encryptedCollectionKeyLen - 1)
	badNonce := "!" + (*treeNonce(1))[1:]
	for name, edit := range map[string]func(*ente.Collection){
		"missing key":         func(c *ente.Collection) { c.ParentEncryptedKey = nil },
		"missing nonce":       func(c *ente.Collection) { c.ParentKeyNonce = nil },
		"short key":           func(c *ente.Collection) { c.ParentEncryptedKey = &shortKey },
		"bad nonce":           func(c *ente.Collection) { c.ParentKeyNonce = &badNonce },
		"keys without parent": func(c *ente.Collection) { c.ParentID = nil },
		"key without parent":  func(c *ente.Collection) { c.ParentID, c.ParentKeyNonce = nil, nil },
	} {
		collection := treeCollection(treeTestOwnerID, ente.Drive, "folder", &root)
		edit(&collection)
		_, err := f.create(collection)
		testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.BadRequest)
		require.Equal(t, before, f.collectionCount(), name)
	}
}

func TestCreateWithParentDepthLimit(t *testing.T) {
	f := setupTreeFixture(t)
	chain := f.chain(ente.MaxCollectionDepth)
	require.Equal(t, ente.MaxCollectionDepth, f.maxLiveDepth())
	_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &chain[len(chain)-1]))
	testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.MaxDepthExceeded)
	f.folder(&chain[len(chain)-2])
}

func TestNonDriveCreateIgnoresParentFields(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	garbage := "not-a-key"
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		for _, collectionType := range []string{"album", "folder"} {
			collection := treeCollection(treeTestOwnerID, app, collectionType, &root)
			collection.ParentEncryptedKey = &garbage
			created, err := f.create(collection)
			require.NoError(t, err)
			require.Nil(t, created.ParentID)
			require.Nil(t, created.ParentEncryptedKey)
			require.Nil(t, created.ParentKeyNonce)
			stored := f.stored(created.ID)
			require.False(t, stored.id.Valid || stored.key.Valid || stored.nonce.Valid)
		}
	}
}

func TestMoveCollection(t *testing.T) {
	f := setupTreeFixture(t)
	a, b := f.folder(nil), f.folder(nil)
	c := f.folder(&a)
	before := f.stored(c)

	require.NoError(t, f.move(c, &b))
	moved := f.stored(c)
	require.Equal(t, b, moved.id.Int64)
	require.Equal(t, *treeKey(2), moved.key.String)
	require.Equal(t, *treeNonce(2), moved.nonce.String)
	require.Greater(t, moved.updationTime, before.updationTime)
	diff, err := f.ctrl.CollectionRepo.GetCollectionsOwnedByUserV2(treeTestOwnerID, before.updationTime, ente.Drive, nil)
	require.NoError(t, err)
	require.Len(t, diff, 1)
	require.Equal(t, c, diff[0].ID)
	require.Equal(t, b, *diff[0].ParentID)

	rewrap := ente.MoveCollectionRequest{CollectionID: c, NewParentID: toParent(&b), ParentEncryptedKey: treeKey(3), ParentKeyNonce: treeNonce(3)}
	require.NoError(t, f.ctrl.MoveCollection(context.Background(), treeTestOwnerID, rewrap))
	rewrapped := f.stored(c)
	require.Equal(t, b, rewrapped.id.Int64)
	require.Equal(t, *treeKey(3), rewrapped.key.String)
	require.Greater(t, rewrapped.updationTime, moved.updationTime)

	require.NoError(t, f.move(c, nil))
	atRoot := f.stored(c)
	require.False(t, atRoot.id.Valid || atRoot.key.Valid || atRoot.nonce.Valid)
	require.Greater(t, atRoot.updationTime, rewrapped.updationTime)
}

func TestMoveCollectionRejectsCycles(t *testing.T) {
	f := setupTreeFixture(t)
	chain := f.chain(6)
	top := chain[0]
	for _, target := range []int64{top, chain[1], chain[5]} {
		err := f.move(top, &target)
		testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.CollectionCycle)
	}
	require.False(t, f.stored(top).id.Valid)
	f.requireNoCycle(chain...)
}

func TestMoveCollectionDepthIncludesSubtreeHeight(t *testing.T) {
	f := setupTreeFixture(t)
	target := f.chain(60)
	subtree := f.chain(5)

	err := f.move(subtree[0], &target[59])
	testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.MaxDepthExceeded)
	require.False(t, f.stored(subtree[0]).id.Valid)
	require.NoError(t, f.move(subtree[0], &target[58]))
	require.Equal(t, ente.MaxCollectionDepth, f.maxLiveDepth())
	require.NoError(t, f.move(subtree[0], nil))

	f.markDeleted(subtree[4])
	require.NoError(t, f.move(subtree[0], &target[59]))
	require.Equal(t, ente.MaxCollectionDepth, f.maxLiveDepth())
}

func TestMoveCollectionRejectsInvalidRequests(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	child := f.folder(&root)
	deleted := f.folder(nil)
	f.markDeleted(deleted)
	missing := int64(1_000_000)
	othersFolder := f.insert(treeTestOtherID, ente.Drive, "folder")
	sharedWithOwner := f.insert(treeTestOtherID, ente.Drive, "folder")
	require.NoError(t, f.ctrl.CollectionRepo.Share(sharedWithOwner, treeTestOtherID, treeTestOwnerID, "share-key", ente.COLLABORATOR, 5))
	invalidMoved := map[int64]error{
		missing:         &ente.ErrNotFoundError,
		othersFolder:    &ente.ErrNotFoundError,
		sharedWithOwner: &ente.ErrNotFoundError,
		deleted:         ente.ErrCollectionDeleted,
		f.insert(treeTestOwnerID, ente.Drive, "album"):         ente.ErrInvalidCollection,
		f.insert(treeTestOwnerID, ente.Drive, "uncategorized"): ente.ErrInvalidCollection,
		f.insert(treeTestOwnerID, ente.Photos, "folder"):       ente.ErrInvalidCollection,
		f.insert(treeTestOwnerID, ente.Locker, "folder"):       ente.ErrInvalidCollection,
	}
	for id, want := range invalidMoved {
		for _, newParentID := range []*int64{&root, nil} {
			var apiErr *ente.ApiError
			require.True(t, errors.As(f.move(id, newParentID), &apiErr))
			require.Equal(t, want, apiErr)
		}
		testutil.RequireAPIError(t, f.move(child, &id), http.StatusBadRequest, ente.InvalidParent)
		require.Equal(t, root, f.stored(child).id.Int64)
	}
	testutil.RequireAPIError(t, f.ctrl.MoveCollection(context.Background(), treeTestOtherID,
		ente.MoveCollectionRequest{CollectionID: child, NewParentID: toParent(nil)}), http.StatusNotFound, ente.NotFoundError)

	shortKey := b64OfLen(encryptedCollectionKeyLen - 1)
	for name, req := range map[string]ente.MoveCollectionRequest{
		"absent parent":      {CollectionID: child},
		"absent parent keys": {CollectionID: child, ParentEncryptedKey: treeKey(2), ParentKeyNonce: treeNonce(2)},
		"missing keys":       {CollectionID: child, NewParentID: toParent(&root)},
		"missing nonce":      {CollectionID: child, NewParentID: toParent(&root), ParentEncryptedKey: treeKey(2)},
		"short key":          {CollectionID: child, NewParentID: toParent(&root), ParentEncryptedKey: &shortKey, ParentKeyNonce: treeNonce(2)},
		"keys to root":       {CollectionID: child, NewParentID: toParent(nil), ParentEncryptedKey: treeKey(2), ParentKeyNonce: treeNonce(2)},
		"nonce only to root": {CollectionID: child, NewParentID: toParent(nil), ParentKeyNonce: treeNonce(2)},
	} {
		err := f.ctrl.MoveCollection(context.Background(), treeTestOwnerID, req)
		testutil.RequireAPIError(t, err, http.StatusBadRequest, ente.BadRequest)
		require.Equal(t, root, f.stored(child).id.Int64, name)
	}
}

func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	var start, done sync.WaitGroup
	start.Add(1)
	for i, fn := range fns {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			errs[i] = fn()
		}()
	}
	start.Done()
	done.Wait()
	return errs
}

func requireOneSucceeded(t *testing.T, errs []error, code ente.ErrorCode) {
	t.Helper()
	failures := 0
	for _, err := range errs {
		if err != nil {
			testutil.RequireAPIError(t, err, http.StatusBadRequest, code)
			failures++
		}
	}
	require.Equal(t, len(errs)-1, failures)
}

func TestConcurrentCrossMovesNeverCreateACycle(t *testing.T) {
	f := setupTreeFixture(t)
	for range 50 {
		a, b := f.folder(nil), f.folder(nil)
		errs := runConcurrently(func() error { return f.move(a, &b) }, func() error { return f.move(b, &a) })
		requireOneSucceeded(t, errs, ente.CollectionCycle)
		f.requireNoCycle(a, b)
	}
}

func TestConcurrentCreateAndMoveNeverExceedMaxDepth(t *testing.T) {
	f := setupTreeFixture(t)
	chain := f.chain(ente.MaxCollectionDepth - 1)
	deepest := chain[len(chain)-1]
	for range 20 {
		x := f.folder(nil)
		errs := runConcurrently(
			func() error {
				_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &x))
				return err
			},
			func() error { return f.move(x, &deepest) },
		)
		requireOneSucceeded(t, errs, ente.MaxDepthExceeded)
		require.LessOrEqual(t, f.maxLiveDepth(), ente.MaxCollectionDepth)
		f.markDeleted(x)
		_, err := f.db.Exec(`UPDATE collections SET is_deleted = TRUE WHERE parent_id = $1`, x)
		require.NoError(t, err)
	}
}

func TestTreeChangesFailFastWhenTheTreeIsBusy(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	child := f.folder(&root)
	setCollectionTreeLimits(t, 1, 300*gTime.Millisecond)
	requireBusy := func() {
		t.Helper()
		start := gTime.Now()
		_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &root))
		testutil.RequireAPIError(t, err, http.StatusServiceUnavailable, ente.CollectionTreeBusy)
		testutil.RequireAPIError(t, f.move(child, nil), http.StatusServiceUnavailable, ente.CollectionTreeBusy)
		require.Less(t, gTime.Since(start), 2*gTime.Second)
	}

	holder := testutil.HoldCollectionTreeLock(t, f.db, treeTestOwnerID)
	requireBusy()
	created, err := f.create(treeCollection(treeTestOtherID, ente.Drive, "folder", nil))
	require.NoError(t, err)
	_, err = f.create(treeCollection(treeTestOtherID, ente.Drive, "folder", &created.ID))
	require.NoError(t, err)
	require.NoError(t, holder.Rollback())

	collectionTreeSlots <- struct{}{}
	requireBusy()
	<-collectionTreeSlots
	f.folder(&root)
	require.NoError(t, f.move(child, nil))
}

func TestConcurrentTreeChangesDontExhaustThePool(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	pool, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	const poolSize = 4
	pool.SetMaxOpenConns(poolSize)
	f.ctrl = newTreeTestController(pool)

	fns := make([]func() error, 0, 3*poolSize)
	for range 3 * poolSize {
		fns = append(fns, func() error {
			_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &root))
			return err
		})
	}
	for _, err := range runConcurrently(fns...) {
		require.NoError(t, err)
	}
	var children int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collections WHERE parent_id = $1`, root).Scan(&children))
	require.Equal(t, 3*poolSize, children)
}

func TestOneOwnersTreeChangesDontStarveOthers(t *testing.T) {
	f := setupTreeFixture(t)
	root := f.folder(nil)
	otherRoot, err := f.create(treeCollection(treeTestOtherID, ente.Drive, "folder", nil))
	require.NoError(t, err)
	setCollectionTreeLimits(t, maxConcurrentTreeChanges, 2*gTime.Second)
	holder := testutil.HoldCollectionTreeLock(t, f.db, treeTestOwnerID)

	const flood = 4 * maxConcurrentTreeChanges
	results := make(chan error, flood)
	for range flood {
		go func() {
			_, err := f.create(treeCollection(treeTestOwnerID, ente.Drive, "folder", &root))
			results <- err
		}()
	}
	lockWaiters := func() int {
		var waiters int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiters))
		return waiters
	}
	require.Eventually(t, func() bool { return lockWaiters() >= maxConcurrentTreeChangesPerOwner }, gTime.Second, 10*gTime.Millisecond)

	start := gTime.Now()
	_, err = f.create(treeCollection(treeTestOtherID, ente.Drive, "folder", &otherRoot.ID))
	require.NoError(t, err)
	require.Less(t, gTime.Since(start), gTime.Second)
	require.Equal(t, maxConcurrentTreeChangesPerOwner, lockWaiters())

	for range flood {
		testutil.RequireAPIError(t, <-results, http.StatusServiceUnavailable, ente.CollectionTreeBusy)
	}
	require.NoError(t, holder.Rollback())
	require.Zero(t, collectionTreeOwnerSlots.Len())
	f.folder(&root)
}
