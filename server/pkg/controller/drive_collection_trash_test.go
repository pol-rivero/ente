package controller

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/repo/public"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

const (
	driveTrashOwner = int64(1)
	driveTrashOther = int64(2)
)

type driveTrashFixture struct {
	t    *testing.T
	db   *sql.DB
	ctrl *TrashController
}

func setupDriveTrashFixture(t *testing.T) *driveTrashFixture {
	t.Helper()
	ctrl, db := setupTrashDriveTest(t)
	testutil.InsertUser(t, db, testutil.UserFixture{UserID: driveTrashOther, Email: "drive-trash-other@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, driveTrashOther, 0)
	_, err := db.Exec(`UPDATE usage SET photos_file_count = 5, locker_file_count = 3`)
	require.NoError(t, err)
	linkRepo := public.NewCollectionLinkRepository(db, "")
	ctrl.CollectionRepo = &repo.CollectionRepository{DB: db, FileRepo: ctrl.TrashRepo.FileRepo, TrashRepo: ctrl.TrashRepo,
		QueueRepo: ctrl.QueueRepo, CollectionLinkRepo: linkRepo}
	return &driveTrashFixture{t: t, db: db, ctrl: ctrl}
}

func (f *driveTrashFixture) collection(ownerID int64, app ente.App, collectionType string) int64 {
	f.t.Helper()
	return testutil.InsertCollection(f.t, f.db, ownerID, app, collectionType)
}

func (f *driveTrashFixture) file(ownerID int64, app ente.App, collectionIDs ...int64) int64 {
	f.t.Helper()
	var fileID int64
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, ownerID, app).Scan(&fileID))
	for _, collectionID := range collectionIDs {
		_, err := f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
			SELECT $1, $2, 'key', 'nonce', 1, owner_id, $3 FROM collections WHERE collection_id = $1`, collectionID, fileID, ownerID)
		require.NoError(f.t, err)
	}
	return fileID
}

func (f *driveTrashFixture) live(fileID, collectionID int64) bool {
	f.t.Helper()
	var live bool
	require.NoError(f.t, f.db.QueryRow(`SELECT NOT is_deleted FROM collection_files WHERE file_id = $1 AND collection_id = $2`,
		fileID, collectionID).Scan(&live))
	return live
}

func (f *driveTrashFixture) inTrash(fileID int64) bool {
	f.t.Helper()
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM trash WHERE file_id = $1 AND NOT is_restored AND NOT is_deleted`, fileID).Scan(&count))
	return count == 1
}

func (f *driveTrashFixture) queued(queueName string) int {
	f.t.Helper()
	var count int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM queue WHERE queue_name = $1 AND NOT is_deleted`, queueName).Scan(&count))
	return count
}

func (f *driveTrashFixture) scheduleDrive(ids ...int64) {
	f.t.Helper()
	require.NoError(f.t, f.ctrl.CollectionRepo.ScheduleDeletes(f.t.Context(), ids, repo.TrashCollectionDriveQueue))
}

func (f *driveTrashFixture) holdLock(name string) {
	f.t.Helper()
	ok, err := f.ctrl.TaskLockRepo.AcquireLock(name, time.MicrosecondsAfterHours(1), "another-host")
	require.NoError(f.t, err)
	require.True(f.t, ok)
}

func (f *driveTrashFixture) releaseLock(name string) {
	f.t.Helper()
	require.NoError(f.t, f.ctrl.TaskLockRepo.ReleaseLock(name))
}

func (f *driveTrashFixture) fileCounts() string {
	f.t.Helper()
	var counts string
	require.NoError(f.t, f.db.QueryRow(`SELECT string_agg(user_id || ':' || coalesce(photos_file_count, -1) || ':' ||
		coalesce(locker_file_count, -1) || ':' || file_count_source_version, ',' ORDER BY user_id) FROM usage`).Scan(&counts))
	return counts
}

func TestDriveCollectionTrashKeepsFilesThatAreStillInLiveFolders(t *testing.T) {
	for name, reversed := range map[string]bool{"in order": false, "reversed": true} {
		t.Run(name, func(t *testing.T) {
			f := setupDriveTrashFixture(t)
			a := f.collection(driveTrashOwner, ente.Drive, "folder")
			b := f.collection(driveTrashOwner, ente.Drive, "folder")
			liveFolder := f.collection(driveTrashOwner, ente.Drive, "folder")
			root := f.collection(driveTrashOwner, ente.Drive, "uncategorized")
			othersFolder := f.collection(driveTrashOther, ente.Drive, "folder")
			onlyInA := f.file(driveTrashOwner, ente.Drive, a)
			alsoInLiveFolder := f.file(driveTrashOwner, ente.Drive, a, liveFolder)
			alsoInRoot := f.file(driveTrashOwner, ente.Drive, root, a)
			inBothDeleted := f.file(driveTrashOwner, ente.Drive, a, b)
			alsoInOthersFolder := f.file(driveTrashOwner, ente.Drive, a, othersFolder)
			addedByOther := f.file(driveTrashOther, ente.Drive, othersFolder, a)
			countsBefore := f.fileCounts()

			if reversed {
				f.scheduleDrive(b, a)
			} else {
				f.scheduleDrive(a, b)
			}
			f.ctrl.CleanupTrashedDriveCollections()

			require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
			for _, fileID := range []int64{onlyInA, inBothDeleted, alsoInOthersFolder} {
				require.True(t, f.inTrash(fileID))
				require.False(t, f.live(fileID, a))
			}
			require.False(t, f.live(inBothDeleted, b))
			require.False(t, f.live(alsoInOthersFolder, othersFolder))
			for fileID, home := range map[int64]int64{alsoInLiveFolder: liveFolder, alsoInRoot: root, addedByOther: othersFolder} {
				require.False(t, f.inTrash(fileID))
				require.False(t, f.live(fileID, a))
				require.True(t, f.live(fileID, home))
			}
			require.Equal(t, countsBefore, f.fileCounts())
		})
	}
}

func TestTrashCollectionQueuesRouteItemsByStoredApp(t *testing.T) {
	f := setupDriveTrashFixture(t)
	driveFolder := f.collection(driveTrashOwner, ente.Drive, "folder")
	liveDriveFolder := f.collection(driveTrashOwner, ente.Drive, "folder")
	driveFile := f.file(driveTrashOwner, ente.Drive, driveFolder, liveDriveFolder)
	album := f.collection(driveTrashOwner, ente.Photos, "album")
	liveAlbum := f.collection(driveTrashOwner, ente.Photos, "album")
	photo := f.file(driveTrashOwner, ente.Photos, album, liveAlbum)
	require.NoError(t, f.ctrl.CollectionRepo.ScheduleDelete(driveFolder))
	f.scheduleDrive(album)

	f.holdLock(fmt.Sprintf("CollectionTrash:%d", driveTrashOwner))
	f.ctrl.CleanupTrashedCollections()
	require.Zero(t, f.queued(repo.TrashCollectionQueueV3))
	require.Equal(t, 2, f.queued(repo.TrashCollectionDriveQueue))
	require.True(t, f.live(driveFile, driveFolder))
	f.releaseLock(fmt.Sprintf("CollectionTrash:%d", driveTrashOwner))

	hook := captureLogs(t)
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	require.Equal(t, 1, f.queued(repo.TrashCollectionQueueV3))
	require.False(t, f.inTrash(driveFile))
	require.False(t, f.live(driveFile, driveFolder))
	require.True(t, f.live(driveFile, liveDriveFolder))
	require.True(t, f.live(photo, album))
	misrouted := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level <= logrus.ErrorLevel {
			require.Equal(t, "queued collection isn't a Drive collection, moving it to the V3 queue", entry.Message)
			misrouted++
		}
	}
	require.Equal(t, 1, misrouted)

	f.ctrl.CleanupTrashedCollections()
	require.Zero(t, f.queued(repo.TrashCollectionQueueV3))
	require.True(t, f.inTrash(photo))
	require.False(t, f.live(photo, liveAlbum))
}

func (f *driveTrashFixture) foldersWithAFile(ownerID int64, count int) []int64 {
	f.t.Helper()
	folders := make([]int64, 0, count)
	for range count {
		folder := f.collection(ownerID, ente.Drive, "folder")
		f.file(ownerID, ente.Drive, folder)
		folders = append(folders, folder)
	}
	return folders
}

func (f *driveTrashFixture) queuedNow(queueName string) int {
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM queue WHERE queue_name = $1 AND NOT is_deleted`, queueName).Scan(&count); err != nil {
		return -1
	}
	return count
}

// Fails if the two crons share a running flag or a lock.
func TestPhotosAlbumDeleteIsNotHeldUpByADriveDrain(t *testing.T) {
	f := setupDriveTrashFixture(t)
	stuck := f.collection(driveTrashOwner, ente.Drive, "folder")
	stuckFile := f.file(driveTrashOwner, ente.Drive, stuck)
	f.scheduleDrive(append([]int64{stuck}, f.foldersWithAFile(driveTrashOwner, 2)...)...)
	album := f.collection(driveTrashOwner, ente.Photos, "album")
	photo := f.file(driveTrashOwner, ente.Photos, album)
	require.NoError(t, f.ctrl.CollectionRepo.ScheduleDelete(album))
	holder, err := f.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM files WHERE file_id = $1 FOR UPDATE`, stuckFile)
	require.NoError(t, err)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		f.ctrl.CleanupTrashedDriveCollections()
	}()
	require.Eventually(t, func() bool {
		var waiting int
		return f.db.QueryRow(`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting) == nil && waiting > 0
	}, 10*gotime.Second, gotime.Millisecond)
	f.ctrl.CleanupTrashedCollections()
	require.Zero(t, f.queued(repo.TrashCollectionQueueV3))
	require.True(t, f.inTrash(photo))
	require.Equal(t, 3, f.queued(repo.TrashCollectionDriveQueue))

	require.NoError(t, holder.Rollback())
	<-drained
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
}

func TestDriveCollectionTrashStopsAtItsBudgetAndResumes(t *testing.T) {
	f := setupDriveTrashFixture(t)
	folders := make([]int64, 0, 5)
	for range 5 {
		folders = append(folders, f.collection(driveTrashOwner, ente.Drive, "folder"))
	}
	f.scheduleDrive(folders...)

	f.holdLock("CollectionTrashDrive")
	f.ctrl.CleanupTrashedDriveCollections()
	require.Equal(t, 5, f.queued(repo.TrashCollectionDriveQueue))
	f.releaseLock("CollectionTrashDrive")

	f.ctrl.driveTrashBudgetOverride = gotime.Nanosecond
	f.ctrl.CleanupTrashedDriveCollections()
	require.Equal(t, 4, f.queued(repo.TrashCollectionDriveQueue))
	f.ctrl.CleanupTrashedDriveCollections()
	require.Equal(t, 3, f.queued(repo.TrashCollectionDriveQueue))

	f.ctrl.driveTrashBudgetOverride = 0
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
}

func TestDriveCollectionTrashOfMoreFilesThanABatch(t *testing.T) {
	f := setupDriveTrashFixture(t)
	folder := f.collection(driveTrashOwner, ente.Drive, "folder")
	liveFolder := f.collection(driveTrashOwner, ente.Drive, "folder")
	_, err := f.db.Exec(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
		SELECT $1, 'drive', 'header', 'header', 'header', 'metadata', 1 FROM generate_series(1, 4500)`, driveTrashOwner)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		SELECT $1, file_id, 'key', 'nonce', 1, $2, $2 FROM files WHERE owner_id = $2`, folder, driveTrashOwner)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		SELECT $1, file_id, 'key', 'nonce', 1, $2, $2 FROM files WHERE owner_id = $2 AND file_id % 3 = 0`, liveFolder, driveTrashOwner)
	require.NoError(t, err)
	countsBefore := f.fileCounts()
	f.scheduleDrive(folder)

	f.ctrl.CleanupTrashedDriveCollections()

	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	var liveInFolder, trashed, wronglyTrashed, liveInLiveFolder int
	require.NoError(t, f.db.QueryRow(`SELECT
			(SELECT count(*) FROM collection_files WHERE collection_id = $1 AND NOT is_deleted),
			(SELECT count(*) FROM trash WHERE collection_id = $1),
			(SELECT count(*) FROM trash WHERE file_id % 3 = 0),
			(SELECT count(*) FROM collection_files WHERE collection_id = $2 AND NOT is_deleted)`,
		folder, liveFolder).Scan(&liveInFolder, &trashed, &wronglyTrashed, &liveInLiveFolder))
	require.Zero(t, liveInFolder)
	require.Equal(t, 3000, trashed)
	require.Zero(t, wronglyTrashed)
	require.Equal(t, 1500, liveInLiveFolder)
	require.Equal(t, countsBefore, f.fileCounts())
}

func TestDriveCollectionTrashCanRunAgain(t *testing.T) {
	f := setupDriveTrashFixture(t)
	folder := f.collection(driveTrashOwner, ente.Drive, "folder")
	liveFolder := f.collection(driveTrashOwner, ente.Drive, "folder")
	trashed := f.file(driveTrashOwner, ente.Drive, folder)
	unlinked := f.file(driveTrashOwner, ente.Drive, folder, liveFolder)
	addedByOther := f.file(driveTrashOther, ente.Drive, folder)
	f.scheduleDrive(folder)
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))

	_, err := f.db.Exec(`UPDATE queue SET is_deleted = FALSE WHERE queue_name = $1`, repo.TrashCollectionDriveQueue)
	require.NoError(t, err)
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	finished, err := f.ctrl.CollectionRepo.TrashDriveCollection(t.Context(), folder, driveTrashOwner, gotime.Now(), gotime.Minute)
	require.NoError(t, err)
	require.True(t, finished)

	require.True(t, f.inTrash(trashed))
	require.False(t, f.inTrash(unlinked))
	require.True(t, f.live(unlinked, liveFolder))
	for _, fileID := range []int64{trashed, unlinked, addedByOther} {
		require.False(t, f.live(fileID, folder))
	}
	var trashRows int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM trash`).Scan(&trashRows))
	require.Equal(t, 1, trashRows)
}

func TestDriveCollectionTrashReRootsLiveChildren(t *testing.T) {
	f := setupDriveTrashFixture(t)
	parent := f.collection(driveTrashOwner, ente.Drive, "folder")
	child := f.collection(driveTrashOwner, ente.Drive, "folder")
	grandchild := f.collection(driveTrashOwner, ente.Drive, "folder")
	deletedChild := f.collection(driveTrashOwner, ente.Drive, "folder")
	setParent := func(id, parentID int64) {
		_, err := f.db.Exec(`UPDATE collections SET parent_id = $2, parent_encrypted_key = 'key', parent_key_nonce = 'nonce'
			WHERE collection_id = $1`, id, parentID)
		require.NoError(t, err)
	}
	setParent(child, parent)
	setParent(grandchild, child)
	setParent(deletedChild, parent)
	f.scheduleDrive(parent, deletedChild)
	childFile := f.file(driveTrashOwner, ente.Drive, child)
	type parentRow struct {
		parentID     sql.NullInt64
		key, nonce   sql.NullString
		updationTime int64
	}
	read := func(id int64) parentRow {
		var row parentRow
		require.NoError(t, f.db.QueryRow(`SELECT parent_id, parent_encrypted_key, parent_key_nonce, updation_time FROM collections
			WHERE collection_id = $1`, id).Scan(&row.parentID, &row.key, &row.nonce, &row.updationTime))
		return row
	}
	before := read(child)

	f.ctrl.CleanupTrashedDriveCollections()

	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	after := read(child)
	require.False(t, after.parentID.Valid)
	require.False(t, after.key.Valid)
	require.False(t, after.nonce.Valid)
	require.Greater(t, after.updationTime, before.updationTime)
	require.Equal(t, child, read(grandchild).parentID.Int64)
	require.Equal(t, parent, read(deletedChild).parentID.Int64)
	require.True(t, f.live(childFile, child))
	require.False(t, f.inTrash(childFile))
}

func TestALargeDriveDeleteYieldsToTheNextItem(t *testing.T) {
	f := setupDriveTrashFixture(t)
	large := f.collection(driveTrashOwner, ente.Drive, "folder")
	_, err := f.db.Exec(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
			metadata_decryption_header, encrypted_metadata, updation_time)
		SELECT $1, 'drive', 'header', 'header', 'header', 'metadata', 1 FROM generate_series(1, 4500)`, driveTrashOwner)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO collection_files(collection_id, file_id, encrypted_key, key_decryption_nonce, updation_time, c_owner_id, f_owner_id)
		SELECT $1, file_id, 'key', 'nonce', 1, $2, $2 FROM files WHERE owner_id = $2`, large, driveTrashOwner)
	require.NoError(t, err)
	f.scheduleDrive(large)
	small := f.collection(driveTrashOther, ente.Drive, "folder")
	smallFile := f.file(driveTrashOther, ente.Drive, small)
	f.scheduleDrive(small)
	liveInLarge := func() int {
		var count int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM collection_files WHERE collection_id = $1 AND NOT is_deleted`, large).Scan(&count))
		return count
	}
	f.ctrl.driveTrashBudgetOverride = gotime.Nanosecond
	hook := captureLogs(t)

	f.ctrl.CleanupTrashedDriveCollections()
	require.Equal(t, 2500, liveInLarge())
	require.Equal(t, 2, f.queued(repo.TrashCollectionDriveQueue))
	f.ctrl.CleanupTrashedDriveCollections()
	require.True(t, f.inTrash(smallFile))
	require.Equal(t, 2500, liveInLarge())
	require.Equal(t, 1, f.queued(repo.TrashCollectionDriveQueue))

	f.ctrl.driveTrashBudgetOverride = 0
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, liveInLarge())
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	yielded := 0
	for _, entry := range hook.AllEntries() {
		require.Greater(t, entry.Level, logrus.ErrorLevel, entry.Message)
		if entry.Message == "collection not trashed yet, moving it to the back of the queue" {
			yielded++
		}
	}
	require.Equal(t, 1, yielded)
}

func TestDriveCollectionTrashStopsWhenItLosesTheRunLock(t *testing.T) {
	f := setupDriveTrashFixture(t)
	previousHeartbeat := driveTrashLockHeartbeat
	driveTrashLockHeartbeat = 10 * gotime.Millisecond
	t.Cleanup(func() { driveTrashLockHeartbeat = previousHeartbeat })
	const folderCount = 400
	f.scheduleDrive(f.foldersWithAFile(driveTrashOwner, folderCount)...)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		f.ctrl.CleanupTrashedDriveCollections()
	}()
	require.Eventually(t, func() bool {
		remaining := f.queuedNow(repo.TrashCollectionDriveQueue)
		return remaining >= 0 && remaining < folderCount
	}, 10*gotime.Second, gotime.Millisecond)
	_, err := f.db.Exec(`UPDATE task_lock SET locked_by = 'another-host' WHERE task_name = $1`, driveCollectionTrashLock)
	require.NoError(t, err)
	select {
	case <-stopped:
	case <-gotime.After(10 * gotime.Second):
		t.Fatal("the run didn't stop")
	}
	require.Positive(t, f.queued(repo.TrashCollectionDriveQueue))
	var lockedBy string
	require.NoError(t, f.db.QueryRow(`SELECT locked_by FROM task_lock WHERE task_name = $1`, driveCollectionTrashLock).Scan(&lockedBy))
	require.Equal(t, "another-host", lockedBy)
}

func TestDriveCollectionTrashStopsWhenItCantExtendTheRunLock(t *testing.T) {
	f := setupDriveTrashFixture(t)
	previousHeartbeat, previousLease := driveTrashLockHeartbeat, driveTrashLease
	driveTrashLockHeartbeat, driveTrashLease = 10*gotime.Millisecond, 200*gotime.Millisecond
	t.Cleanup(func() { driveTrashLockHeartbeat, driveTrashLease = previousHeartbeat, previousLease })
	lockDB, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lockDB.Close() })
	f.ctrl.TaskLockRepo = &repo.TaskLockRepository{DB: lockDB}
	const folderCount = 400
	f.scheduleDrive(f.foldersWithAFile(driveTrashOwner, folderCount)...)
	hook := captureLogs(t)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		f.ctrl.CleanupTrashedDriveCollections()
	}()
	require.Eventually(t, func() bool {
		remaining := f.queuedNow(repo.TrashCollectionDriveQueue)
		return remaining >= 0 && remaining < folderCount
	}, 10*gotime.Second, gotime.Millisecond)
	require.NoError(t, lockDB.Close())
	select {
	case <-stopped:
	case <-gotime.After(10 * gotime.Second):
		t.Fatal("the run didn't stop")
	}
	require.Positive(t, f.queued(repo.TrashCollectionDriveQueue))
	gaveUp := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message == "couldn't extend lock, stopping" {
			gaveUp++
		}
	}
	require.Equal(t, 1, gaveUp)
}

func TestPhotosEmptyTrashDoesNotDrainTheDriveQueue(t *testing.T) {
	f := setupDriveTrashFixture(t)
	trashed := make(map[ente.App]int64)
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		collectionID := f.collection(driveTrashOwner, app, "folder")
		fileID := f.file(driveTrashOwner, app, collectionID)
		require.NoError(t, f.ctrl.TrashRepo.TrashFiles(t.Context(), driveTrashOwner, ente.TrashRequest{
			TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: collectionID}},
		}))
		trashed[app] = fileID
	}
	require.NoError(t, f.ctrl.TrashRepo.EmptyTrash(t.Context(), driveTrashOwner, time.Microseconds(), ente.Drive))

	require.NoError(t, f.ctrl.EmptyTrash(t.Context(), driveTrashOwner, ente.EmptyTrashRequest{LastUpdatedAt: time.Microseconds()}, ente.Photos))
	require.False(t, f.inTrash(trashed[ente.Photos]))
	require.True(t, f.inTrash(trashed[ente.Drive]))
	require.Equal(t, 1, f.queued(repo.TrashEmptyDriveQueue))

	f.ctrl.ProcessEmptyTrashRequests()
	require.False(t, f.inTrash(trashed[ente.Drive]))
	require.Zero(t, f.queued(repo.TrashEmptyDriveQueue))
}

func TestDriveCollectionTrashDropsLiveCollections(t *testing.T) {
	f := setupDriveTrashFixture(t)
	folder := f.collection(driveTrashOwner, ente.Drive, "folder")
	fileID := f.file(driveTrashOwner, ente.Drive, folder)
	require.NoError(t, f.ctrl.QueueRepo.InsertItem(t.Context(), repo.TrashCollectionDriveQueue, strconv.FormatInt(folder, 10)))

	f.ctrl.CleanupTrashedDriveCollections()

	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	require.True(t, f.live(fileID, folder))
	require.False(t, f.inTrash(fileID))
	var deleted bool
	require.NoError(t, f.db.QueryRow(`SELECT is_deleted FROM collections WHERE collection_id = $1`, folder).Scan(&deleted))
	require.False(t, deleted)
}

func TestFileCreateIntoADeletedDriveFolderFails(t *testing.T) {
	_, fileRepo, db := setupObjectCleanupRaceTest(t, "http://127.0.0.1:1")
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "drive-create-deleted@ente.com", CreationTime: 1})
	testutil.InsertUsage(t, db, userID, 0)
	collectionRepo := &repo.CollectionRepository{DB: db, QueueRepo: &repo.QueueRepository{DB: db}}
	for i, app := range []ente.App{ente.Drive, ente.Photos, ente.Locker} {
		collectionID := insertObjectCleanupTestCollection(t, db, userID)
		_, err := db.Exec(`UPDATE collections SET app = $1 WHERE collection_id = $2`, app, collectionID)
		require.NoError(t, err)
		require.NoError(t, collectionRepo.ScheduleDeletes(t.Context(), []int64{collectionID}, repo.TrashCollectionDriveQueue))
		fileKey, thumbKey := fmt.Sprintf("1/deleted-file-%d", i), fmt.Sprintf("1/deleted-thumb-%d", i)
		_, err = db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id)
			VALUES ($1, 0, 'b2-eu-cen'), ($2, 0, 'b2-eu-cen')`, fileKey, thumbKey)
		require.NoError(t, err)
		file := objectCleanupTestFile(userID, collectionID, fileKey, thumbKey)

		_, _, err = fileRepo.Create(file, 100, 10, 110, userID, app)

		var files int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM collection_files WHERE collection_id = $1`, collectionID).Scan(&files))
		if app == ente.Drive {
			require.ErrorIs(t, err, ente.ErrCollectionDeleted)
			require.Zero(t, files)
			continue
		}
		require.NoError(t, err, app)
		require.Equal(t, 1, files, app)
	}
}

func TestDriveCollectionTrashTimesOutAnItemStuckOnALock(t *testing.T) {
	f := setupDriveTrashFixture(t)
	previousTimeout := driveTrashBatchTimeout
	driveTrashBatchTimeout = 300 * gotime.Millisecond
	t.Cleanup(func() { driveTrashBatchTimeout = previousTimeout })
	stuck := f.collection(driveTrashOwner, ente.Drive, "folder")
	stuckFile := f.file(driveTrashOwner, ente.Drive, stuck)
	other := f.collection(driveTrashOther, ente.Drive, "folder")
	otherFile := f.file(driveTrashOther, ente.Drive, other)
	f.scheduleDrive(stuck, other)
	holder, err := f.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM files WHERE file_id = $1 FOR UPDATE`, stuckFile)
	require.NoError(t, err)
	hook := captureLogs(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.ctrl.CleanupTrashedDriveCollections()
	}()
	select {
	case <-done:
	case <-gotime.After(10 * gotime.Second):
		t.Fatal("the run didn't finish")
	}

	require.Equal(t, 1, f.queued(repo.TrashCollectionDriveQueue))
	require.True(t, f.live(stuckFile, stuck))
	require.True(t, f.inTrash(otherFile))
	var locks int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM task_lock WHERE task_name LIKE 'CollectionTrash%'`).Scan(&locks))
	require.Zero(t, locks)
	timedOut := 0
	for _, entry := range hook.AllEntries() {
		if entry.Message == "timed out trashing collection" {
			timedOut++
			require.Equal(t, logrus.ErrorLevel, entry.Level)
		}
	}
	require.Equal(t, 1, timedOut)

	require.NoError(t, holder.Rollback())
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	require.True(t, f.inTrash(stuckFile))
}

func TestADriveDeleteStuckOnALockDoesNotHoldUpOtherOwners(t *testing.T) {
	f := setupDriveTrashFixture(t)
	previousTimeout := driveTrashBatchTimeout
	driveTrashBatchTimeout = 300 * gotime.Millisecond
	t.Cleanup(func() { driveTrashBatchTimeout = previousTimeout })
	stuck := f.collection(driveTrashOwner, ente.Drive, "folder")
	stuckFile := f.file(driveTrashOwner, ente.Drive, stuck)
	f.scheduleDrive(stuck)
	other := f.collection(driveTrashOther, ente.Drive, "folder")
	otherFile := f.file(driveTrashOther, ente.Drive, other)
	f.scheduleDrive(other)
	holder, err := f.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM files WHERE file_id = $1 FOR UPDATE`, stuckFile)
	require.NoError(t, err)
	// One item per run.
	f.ctrl.driveTrashBudgetOverride = gotime.Nanosecond

	for range 3 {
		f.ctrl.CleanupTrashedDriveCollections()
	}
	require.True(t, f.inTrash(otherFile))
	require.True(t, f.live(stuckFile, stuck))
	require.Equal(t, 1, f.queued(repo.TrashCollectionDriveQueue))

	require.NoError(t, holder.Rollback())
	f.ctrl.CleanupTrashedDriveCollections()
	require.Zero(t, f.queued(repo.TrashCollectionDriveQueue))
	require.True(t, f.inTrash(stuckFile))
}

// Account deletion queues the same item for every app.
func TestDriveEmptyTrashDoesNotShareTheOtherAppsLock(t *testing.T) {
	f := setupDriveTrashFixture(t)
	collectionID := f.collection(driveTrashOwner, ente.Drive, "folder")
	fileID := f.file(driveTrashOwner, ente.Drive, collectionID)
	require.NoError(t, f.ctrl.TrashRepo.TrashFiles(t.Context(), driveTrashOwner, ente.TrashRequest{
		TrashItems: []ente.TrashItemRequest{{FileID: fileID, CollectionID: collectionID}},
	}))
	lastUpdatedAt := time.Microseconds()
	for _, app := range []ente.App{ente.Photos, ente.Drive} {
		require.NoError(t, f.ctrl.TrashRepo.EmptyTrash(t.Context(), driveTrashOwner, lastUpdatedAt, app))
	}
	photosLock := fmt.Sprintf("EmptyTrash:%d%s%d", driveTrashOwner, repo.EmptyTrashQueueItemSeparator, lastUpdatedAt)
	f.holdLock(photosLock)

	f.ctrl.processEmptyDriveTrashRequests()
	require.False(t, f.inTrash(fileID))
	require.Zero(t, f.queued(repo.TrashEmptyDriveQueue))
	f.ctrl.processEmptyTrashRequests()
	require.Equal(t, 1, f.queued(repo.TrashEmptyQueue))
	f.releaseLock(photosLock)
	f.ctrl.processEmptyTrashRequests()
	require.Zero(t, f.queued(repo.TrashEmptyQueue))
}
