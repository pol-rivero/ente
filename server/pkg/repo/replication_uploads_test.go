package repo

import (
	"database/sql"
	"testing"
	"time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/stretchr/testify/require"
)

func setupReplicationRepoTest(t *testing.T, keys ...string) *sql.DB {
	t.Helper()
	_, db := setupAccessibleObjectTest(t)
	ownerID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "replication-repo@ente.com", CreationTime: 1})
	for _, key := range keys {
		insertObjectTestKey(t, db, insertObjectTestFile(t, db, ownerID), ente.FILE, key, 100, []string{"b2-eu-cen"})
		_, err := db.Exec(`INSERT INTO object_copies(object_key, want_b2, b2, want_wasabi, want_scw) VALUES ($1, true, 1, true, false)`, key)
		require.NoError(t, err)
	}
	return db
}

func TestReplicationUploadsRepository(t *testing.T) {
	db := setupReplicationRepoTest(t, "1/live")
	r := &ReplicationUploadsRepository{DB: db}
	upload := ReplicationUpload{ObjectKey: "1/live", DestDC: "wasabi-eu-central-2-v3", UploadID: "u1", PartSize: 64, SourceETag: "etag"}

	inserted, err := r.Insert(t.Context(), upload)
	require.NoError(t, err)
	require.True(t, inserted)
	inserted, err = r.Insert(t.Context(), ReplicationUpload{ObjectKey: "1/live", DestDC: upload.DestDC, UploadID: "u2"})
	require.NoError(t, err)
	require.False(t, inserted)
	got, err := r.Get(t.Context(), "1/live", upload.DestDC)
	require.NoError(t, err)
	require.Equal(t, upload, *got)
	got, err = r.Get(t.Context(), "1/live", "scw-eu-fr-v3")
	require.NoError(t, err)
	require.Nil(t, got)
	got, err = r.GetByUploadID(t.Context(), "1/live", "u1")
	require.NoError(t, err)
	require.Equal(t, upload, *got)
	got, err = r.GetByUploadID(t.Context(), "1/live", "u2")
	require.NoError(t, err)
	require.Nil(t, got)

	_, err = r.Insert(t.Context(), ReplicationUpload{ObjectKey: "1/gone", DestDC: upload.DestDC, UploadID: "old"})
	require.NoError(t, err)
	_, err = r.Insert(t.Context(), ReplicationUpload{ObjectKey: "1/gone", DestDC: "scw-eu-fr-v3", UploadID: "new"})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE replication_uploads SET created_at = 1 WHERE upload_id IN ('u1', 'old')`)
	require.NoError(t, err)
	listed, err := r.ListForObject(t.Context(), "1/gone")
	require.NoError(t, err)
	require.Len(t, listed, 2)
	abandoned, err := r.ListAbandoned(t.Context(), time.Now().Add(-time.Hour).UnixMicro(), 10)
	require.NoError(t, err)
	require.Len(t, abandoned, 1)
	require.Equal(t, "old", abandoned[0].UploadID)

	require.NoError(t, r.Delete(t.Context(), "1/live", upload.DestDC, "u2"))
	got, err = r.Get(t.Context(), "1/live", upload.DestDC)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NoError(t, r.Delete(t.Context(), "1/live", upload.DestDC, "u1"))
	got, err = r.Get(t.Context(), "1/live", upload.DestDC)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestObjectCopiesReplicationAttempts(t *testing.T) {
	db := setupReplicationRepoTest(t, "1/object")
	r := &ObjectCopiesRepository{DB: db}

	copies, err := r.GetAndLockUnreplicatedObject(t.Context())
	require.NoError(t, err)
	require.Positive(t, copies.LastAttempt)
	got, err := r.Get(t.Context(), "1/object")
	require.NoError(t, err)
	require.Equal(t, ente.ObjectCopies{ObjectKey: "1/object", WantB2: true, B2: got.B2, WantWasabi: true, LastAttempt: copies.LastAttempt}, *got)
	missing, err := r.Get(t.Context(), "1/missing")
	require.NoError(t, err)
	require.Nil(t, missing)
	objects := &ObjectRepository{DB: db}
	size, app, found, err := objects.GetObjectSizeAndApp(t.Context(), "1/object")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(100), size)
	require.Empty(t, app)
	_, err = db.Exec(`UPDATE files SET app = 'drive'`)
	require.NoError(t, err)
	_, app, _, err = objects.GetObjectSizeAndApp(t.Context(), "1/object")
	require.NoError(t, err)
	require.Equal(t, "drive", app)
	_, _, found, err = objects.GetObjectSizeAndApp(t.Context(), "1/missing")
	require.NoError(t, err)
	require.False(t, found)

	next, ok, err := r.ExtendReplicationAttempt(t.Context(), "1/object", copies.LastAttempt)
	require.NoError(t, err)
	require.True(t, ok)
	require.Greater(t, next, copies.LastAttempt)
	_, ok, err = r.ExtendReplicationAttempt(t.Context(), "1/object", copies.LastAttempt)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, r.RetryReplicationAttemptAfter(t.Context(), "1/object", copies.LastAttempt, time.Hour))
	got, err = r.Get(t.Context(), "1/object")
	require.NoError(t, err)
	require.Equal(t, next, got.LastAttempt)
	require.NoError(t, r.RetryReplicationAttemptAfter(t.Context(), "1/object", next, time.Hour))
	got, err = r.Get(t.Context(), "1/object")
	require.NoError(t, err)
	require.InDelta(t, time.Now().Add(-23*time.Hour).UnixMicro(), got.LastAttempt, float64(time.Minute.Microseconds()))
}

func TestListAbandonedRetriesFailedAbortsLast(t *testing.T) {
	db := setupReplicationRepoTest(t)
	r := &ReplicationUploadsRepository{DB: db}
	uploads := make(map[string]ReplicationUpload)
	for _, uploadID := range []string{"failed", "new"} {
		u := ReplicationUpload{ObjectKey: "1/gone-" + uploadID, DestDC: "wasabi-eu-central-2-v3", UploadID: uploadID}
		_, err := r.Insert(t.Context(), u)
		require.NoError(t, err)
		uploads[uploadID] = u
	}
	_, err := db.Exec(`UPDATE replication_uploads SET created_at = CASE upload_id WHEN 'failed' THEN 1 ELSE 2 END`)
	require.NoError(t, err)
	cutoff := time.Now().Add(-time.Hour).UnixMicro()
	abandoned, err := r.ListAbandoned(t.Context(), cutoff, 1)
	require.NoError(t, err)
	require.Equal(t, []ReplicationUpload{uploads["failed"]}, abandoned)

	require.NoError(t, r.MarkAbortFailed(t.Context(), uploads["failed"]))
	abandoned, err = r.ListAbandoned(t.Context(), cutoff, 1)
	require.NoError(t, err)
	require.Equal(t, []ReplicationUpload{uploads["new"]}, abandoned)
}
