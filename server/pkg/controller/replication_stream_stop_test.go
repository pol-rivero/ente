package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ente/museum/internal/testutil/fakes3"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func (rt *replicationTest) requireHandedBack(t *testing.T, key string) {
	t.Helper()
	want := time.Now().Add(-replicationRepickWindow + streamingShutdownRetry).UnixMicro()
	require.InDelta(t, want, rt.lastAttempt(t, key), float64(30*time.Second.Microseconds()))
}

func failAborts(req fakes3.Request) *fakes3.Failure {
	if req.Op == fakes3.OpAbort {
		return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
	}
	return nil
}

func TestFailedAbortKeepsTheUploadForTheSweeper(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("leak")
	rt.addObject(t, key, 3*testPartSize)
	_, err := rt.db.Exec(`UPDATE object_keys SET is_deleted = true WHERE object_key = $1`, key)
	require.NoError(t, err)
	stored := rt.scw.StartUploadAt(key, time.Now().Add(-30*24*time.Hour))
	rt.scw.PutPart(stored, 1, randomData(testPartSize))
	rt.insertUploadRow(t, key, scwDC, stored, "")
	rt.scw.SetHook(failAborts)

	require.NoError(t, rt.c.tryReplicate(t.Context()))
	require.Len(t, rt.uploadRows(t), 1)
	require.Len(t, rt.scw.Uploads(), 1)

	_, err = rt.db.Exec(`DELETE FROM object_keys WHERE object_key = $1`, key)
	require.NoError(t, err)
	_, err = rt.db.Exec(`UPDATE replication_uploads SET created_at = 1`)
	require.NoError(t, err)
	rt.c.sweepOrphanUploads(t.Context())
	require.Len(t, rt.uploadRows(t), 1)
	require.Len(t, rt.scw.Uploads(), 1)

	rt.scw.SetHook(nil)
	rt.c.sweepOrphanUploads(t.Context())
	require.Empty(t, rt.uploadRows(t))
	require.Empty(t, rt.scw.Uploads())
}

func TestStreamingReplicationKeepsADiscardedUploadItCantAbort(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("discard")
	data := rt.addObject(t, key, 3*testPartSize)
	stored := rt.wasabi.StartUpload(key)
	rt.insertUploadRow(t, key, wasabiDC, stored, `"other"`)
	rt.wasabi.SetHook(failAborts)

	err := rt.c.tryReplicate(t.Context())

	require.ErrorContains(t, err, "Failed to abort upload")
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpCreate))
	require.Equal(t, []string{key + " " + wasabiDC + " " + stored}, rt.uploadRows(t))
	rt.requireRetrySoon(t, key)
	_, ok := rt.scw.Object(key)
	require.True(t, ok)

	rt.wasabi.SetHook(nil)
	rt.rewind(t)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	rt.requireReplicated(t, key, data)
}

func TestStopReplicationHandsBackStreamingAttempts(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("shutdown")
	rt.addObject(t, key, 3*testPartSize)
	blocked := make(chan struct{}, 2)
	release := make(chan struct{})
	block := func(req fakes3.Request) *fakes3.Failure {
		if req.Op != fakes3.OpUpload || req.PartNumber != 2 {
			return nil
		}
		blocked <- struct{}{}
		<-release
		return &fakes3.Failure{Status: http.StatusBadRequest, Code: "InvalidRequest"}
	}
	rt.wasabi.SetHook(block)
	rt.scw.SetHook(block)
	result := make(chan error, 1)
	go func() { result <- rt.c.tryReplicate(context.Background()) }()
	<-blocked

	rt.c.StopReplication()
	rt.requireHandedBack(t, key)
	close(release)

	require.Error(t, <-result)
	rt.requireHandedBack(t, key)
	require.Len(t, rt.uploadRows(t), 2)
	requireNoAborts(t, rt.wasabi, rt.scw)
}

func TestHandedBackAttemptStopsAtItsNextBeat(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("handback")
	rt.addObject(t, key, 10)
	copies, err := rt.c.ObjectCopiesRepo.GetAndLockUnreplicatedObject(t.Context())
	require.NoError(t, err)
	j := &streamJob{c: rt.c, cfg: rt.c.stream, key: key, logger: log.WithField("test", t.Name()),
		leaseLock: make(chan struct{}, 1), lease: replicationLease{lastAttempt: copies.LastAttempt, confirmedAt: time.Now()}}
	require.True(t, rt.c.trackStreamJob(j))

	rt.c.StopReplication()

	rt.requireHandedBack(t, key)
	require.False(t, j.beat(t.Context()))
	rt.requireHandedBack(t, key)
}

func TestStreamingAttemptPickedWhileStoppingIsHandedBack(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("stopping")
	rt.addObject(t, key, 3*testPartSize)
	rt.c.StopReplication()

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	require.Empty(t, rt.b2.Requests())
	rt.requireHandedBack(t, key)
}
