package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/internal/testutil/fakes3"
	"github.com/ente/museum/pkg/controller/discord"
	"github.com/ente/museum/pkg/controller/lock"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/config"
	"github.com/ente/museum/pkg/utils/s3config"
	timeUtil "github.com/ente/museum/pkg/utils/time"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

const (
	replicationUserID = int64(8800)
	testPartSize      = fakes3.MinPartSize
	wasabiDC          = "wasabi-eu-central-2-v3"
	scwDC             = "scw-eu-fr-v3"
)

type replicationTest struct {
	c               *ReplicationController3
	db              *sql.DB
	b2, wasabi, scw *fakes3.Server
}

func setupReplicationTest(t *testing.T) *replicationTest {
	t.Helper()
	testutil.WithServerRoot(t)
	viper.Reset()
	require.NoError(t, config.ConfigureViper("local"))
	t.Cleanup(viper.Reset)
	rt := &replicationTest{b2: fakes3.New(t), wasabi: fakes3.New(t), scw: fakes3.New(t)}
	for dc, fake := range map[string]*fakes3.Server{"b2-eu-cen": rt.b2, wasabiDC: rt.wasabi, scwDC: rt.scw} {
		viper.Set("s3."+dc+".key", "key")
		viper.Set("s3."+dc+".secret", "secret")
		viper.Set("s3."+dc+".endpoint", fake.URL)
		viper.Set("s3."+dc+".region", "us-east-1")
		viper.Set("s3."+dc+".bucket", fakes3.Bucket)
		viper.Set("s3."+dc+".disable_ssl", true)
	}
	viper.Set("s3.use_path_style_urls", true)

	rt.db = testutil.RequireTestDB(t)
	testutil.ResetTables(t, rt.db)
	t.Cleanup(func() { testutil.ResetTables(t, rt.db) })
	testutil.InsertUser(t, rt.db, testutil.UserFixture{UserID: replicationUserID, Email: "replication@ente.com", CreationTime: 1})

	rt.c = &ReplicationController3{
		S3Config:               s3config.NewS3Config(),
		ObjectRepo:             &repo.ObjectRepository{DB: rt.db},
		ObjectCopiesRepo:       &repo.ObjectCopiesRepository{DB: rt.db},
		ReplicationUploadsRepo: &repo.ReplicationUploadsRepository{DB: rt.db},
		LockController:         &lock.LockController{TaskLockingRepo: &repo.TaskLockRepository{DB: rt.db}, HostName: "replication-test"},
		DiscordController:      &discord.DiscordController{},
		tempStorage:            t.TempDir(),
		mUploadSuccess:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "success"}, []string{"destination"}),
		mUploadFailure:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "failure"}, []string{"destination"}),
	}
	rt.c.createDestinations()
	rt.c.stream = newStreamingConfig(6)
	rt.c.stream.threshold = 2 * testPartSize
	rt.c.stream.minPartSize = testPartSize
	rt.c.stream.diskReserve = 0
	rt.c.stream.idleTimeout = 2 * time.Second
	rt.c.stream.heartbeatInterval = 20 * time.Millisecond
	rt.c.stream.metadataTimeout = 10 * time.Second
	rt.c.stream.uploadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	rt.c.stream.fetchRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	return rt
}

func replicationKey(name string) string {
	return fmt.Sprintf("%d/%s", replicationUserID, name)
}

func randomData(size int64) []byte {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	return data
}

func (rt *replicationTest) addObject(t *testing.T, key string, size int64) []byte {
	t.Helper()
	return rt.addAppObject(t, key, size, ente.Drive)
}

func (rt *replicationTest) addAppObject(t *testing.T, key string, size int64, app ente.App) []byte {
	t.Helper()
	data := randomData(size)
	rt.b2.PutObjectData(key, data)
	rt.addObjectRows(t, key, size, app)
	return data
}

func (rt *replicationTest) addObjectRows(t *testing.T, key string, size int64, app ente.App) {
	t.Helper()
	var fileID int64
	require.NoError(t, rt.db.QueryRow(`INSERT INTO files(owner_id, app, file_decryption_header, thumbnail_decryption_header,
		metadata_decryption_header, encrypted_metadata, updation_time)
		VALUES ($1, $2, 'header', 'header', 'header', 'metadata', 1) RETURNING file_id`, replicationUserID, app).Scan(&fileID))
	_, err := rt.db.Exec(`INSERT INTO object_keys(file_id, o_type, object_key, size, datacenters)
		VALUES ($1, 'file', $2, $3, ARRAY['b2-eu-cen']::s3region[])`, fileID, key, size)
	require.NoError(t, err)
	_, err = rt.db.Exec(`INSERT INTO object_copies(object_key, want_b2, b2, want_wasabi, want_scw) VALUES ($1, true, 1, true, true)`, key)
	require.NoError(t, err)
}

func (rt *replicationTest) insertUploadRow(t *testing.T, key, dc, uploadID, etag string) {
	t.Helper()
	_, err := rt.db.Exec(`INSERT INTO replication_uploads(object_key, dest_dc, upload_id, part_size, source_etag)
		VALUES ($1, $2, $3, $4, $5)`, key, dc, uploadID, testPartSize, etag)
	require.NoError(t, err)
}

func (rt *replicationTest) uploadRows(t *testing.T) []string {
	t.Helper()
	rows, err := rt.db.Query(`SELECT object_key || ' ' || dest_dc || ' ' || upload_id FROM replication_uploads ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		result = append(result, row)
	}
	return result
}

func (rt *replicationTest) lastAttempt(t *testing.T, key string) int64 {
	t.Helper()
	var lastAttempt int64
	require.NoError(t, rt.db.QueryRow(`SELECT last_attempt FROM object_copies WHERE object_key = $1`, key).Scan(&lastAttempt))
	return lastAttempt
}

func (rt *replicationTest) rewind(t *testing.T) {
	t.Helper()
	_, err := rt.db.Exec(`UPDATE object_copies SET last_attempt = last_attempt - 25 * 3600 * 1000000::BIGINT`)
	require.NoError(t, err)
}

func (rt *replicationTest) requireRetrySoon(t *testing.T, key string) {
	t.Helper()
	require.InDelta(t, time.Now().Add(-23*time.Hour).UnixMicro(), rt.lastAttempt(t, key), float64(time.Minute.Microseconds()))
}

func (rt *replicationTest) requireReplicated(t *testing.T, key string, data []byte) {
	t.Helper()
	for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
		object, ok := fake.Object(key)
		require.True(t, ok)
		require.Equal(t, data, object.Data)
		require.Empty(t, fake.Uploads())
	}
	scwObject, _ := rt.scw.Object(key)
	require.Equal(t, "GLACIER", scwObject.StorageClass)
	var wasabi, scw sql.NullInt64
	var dcs []string
	require.NoError(t, rt.db.QueryRow(`SELECT c.wasabi, c.scw, k.datacenters::text[] FROM object_copies c
		JOIN object_keys k ON k.object_key = c.object_key WHERE c.object_key = $1`, key).Scan(&wasabi, &scw, pq.Array(&dcs)))
	require.True(t, wasabi.Valid && scw.Valid)
	require.Subset(t, dcs, []string{"b2-eu-cen", wasabiDC, scwDC})
	require.Empty(t, rt.uploadRows(t))
	rt.requireNoSpools(t)
}

func (rt *replicationTest) requireNoSpools(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(rt.c.tempStorage)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func requireNoAborts(t *testing.T, fakes ...*fakes3.Server) {
	t.Helper()
	for _, fake := range fakes {
		require.Empty(t, fake.RequestsOf(fakes3.OpAbort))
	}
}

func requestedParts(requests []fakes3.Request) []int64 {
	var numbers []int64
	for _, r := range requests {
		numbers = append(numbers, r.PartNumber)
	}
	return numbers
}

func uploadIDs(fake *fakes3.Server) []string {
	return slices.Collect(maps.Keys(fake.Uploads()))
}

func TestStreamingReplicationLimits(t *testing.T) {
	const gib = int64(1) << 30
	require.Less(t, defaultStreamingThreshold, DriveMaxFileSize)
	// The legacy s3manager path sizes parts as size/1000 + 1.
	require.LessOrEqual(t, InternalUserMaxFileSize/1000+1, ente.MaxMultipartPartSize)

	partSize, err := streamingPartSize(DriveMaxFileSize, streamingMinPartSize)
	require.NoError(t, err)
	require.Equal(t, ente.MaxMultipartPartSize, partSize)
	require.Equal(t, int64(streamingMaxParts), DriveMaxFileSize/partSize)
	partSize, err = streamingPartSize(defaultStreamingThreshold+1, streamingMinPartSize)
	require.NoError(t, err)
	require.Equal(t, streamingMinPartSize, partSize)
	partSize, err = streamingPartSize(3000*gib+7, streamingMinPartSize)
	require.NoError(t, err)
	require.Equal(t, int64(3073)<<20, partSize)
	_, err = streamingPartSize(DriveMaxFileSize+1, streamingMinPartSize)
	require.Error(t, err)

	viper.Reset()
	t.Cleanup(viper.Reset)
	c := &ReplicationController3{stream: newStreamingConfig(6)}
	require.Equal(t, defaultStreamingThreshold, c.stream.threshold)
	viper.Set("replication.streaming-threshold", 100*gib)
	require.Equal(t, 100*gib, newStreamingConfig(6).threshold)

	require.True(t, c.stream.slots.TryAcquire(2))
	require.False(t, c.stream.slots.TryAcquire(1))
	viper.Set("replication.streaming-workers", 4)
	require.True(t, newStreamingConfig(6).slots.TryAcquire(4))

	require.True(t, c.isStreamingEligible(gib+1, string(ente.Drive)))
	require.False(t, c.isStreamingEligible(gib, string(ente.Drive)))
	require.False(t, c.isStreamingEligible(InternalUserMaxFileSize, string(ente.Photos)))
	require.True(t, c.isStreamingEligible(InternalUserMaxFileSize+1, string(ente.Photos)))
	streams, err := c.streamsObject(t.Context(), "key", InternalUserMaxFileSize+1)
	require.NoError(t, err)
	require.True(t, streams)
	streams, err = c.streamsObject(t.Context(), "key", gib)
	require.NoError(t, err)
	require.False(t, streams)

	require.True(t, etagsDiffer(`"a"`, `"b"`))
	require.False(t, etagsDiffer(`"a"`, `W/"a"`))
	require.False(t, etagsDiffer(`"a"`, ""))
}

func TestReplicationRouting(t *testing.T) {
	for _, tt := range []struct {
		name      string
		app       ente.App
		size      int64
		streaming bool
	}{
		{name: "drive above threshold", app: ente.Drive, size: 3 << 20, streaming: true},
		{name: "drive at threshold", app: ente.Drive, size: 2 << 20},
		{name: "photos above threshold", app: ente.Photos, size: 3 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			rt.c.stream.threshold = 2 << 20
			key := replicationKey("routed")
			data := rt.addAppObject(t, key, tt.size, tt.app)

			require.NoError(t, rt.c.tryReplicate(t.Context()))

			rt.requireReplicated(t, key, data)
			gets := rt.b2.RequestsOf(fakes3.OpGet)
			require.Len(t, gets, 1)
			for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
				if tt.streaming {
					require.Len(t, fake.RequestsOf(fakes3.OpCreate), 1)
					require.Empty(t, fake.RequestsOf(fakes3.OpPut))
				} else {
					require.Empty(t, gets[0].Range)
					require.Len(t, fake.RequestsOf(fakes3.OpPut), 1)
					require.Empty(t, fake.RequestsOf(fakes3.OpCreate))
				}
			}
		})
	}
}

func TestStreamingReplication(t *testing.T) {
	rt := setupReplicationTest(t)
	rt.c.stream.spool = semaphore.NewWeighted(testPartSize)
	rt.c.stream.spoolCapacity = testPartSize
	var peak atomic.Int64
	measure := func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpUpload {
			var total int64
			entries, _ := os.ReadDir(rt.c.tempStorage)
			for _, entry := range entries {
				if info, err := os.Stat(filepath.Join(rt.c.tempStorage, entry.Name())); err == nil {
					total += info.Size()
				}
			}
			for current := peak.Load(); total > current && !peak.CompareAndSwap(current, total); current = peak.Load() {
			}
		}
		return nil
	}
	rt.wasabi.SetHook(measure)
	rt.scw.SetHook(measure)
	key := replicationKey("large")
	data := rt.addObject(t, key, 3*testPartSize+1234)

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	rt.requireReplicated(t, key, data)
	require.Zero(t, rt.c.stream.spoolPending.Load())
	require.Positive(t, peak.Load())
	require.LessOrEqual(t, peak.Load(), testPartSize)
	var ranges []string
	for _, r := range rt.b2.RequestsOf(fakes3.OpGet) {
		ranges = append(ranges, r.Range)
	}
	require.ElementsMatch(t, []string{"bytes=0-5242879", "bytes=5242880-10485759", "bytes=10485760-15728639", "bytes=15728640-15729873"}, ranges)
	require.Equal(t, "GLACIER", rt.scw.RequestsOf(fakes3.OpCreate)[0].StorageClass)
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpCreate)[0].StorageClass)
	for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
		require.Len(t, fake.RequestsOf(fakes3.OpCreate), 1)
		uploads := fake.RequestsOf(fakes3.OpUpload)
		require.ElementsMatch(t, []int64{1, 2, 3, 4}, requestedParts(uploads))
		for _, upload := range uploads {
			require.NotEmpty(t, upload.ContentMD5)
		}
		require.Equal(t, []int64{1, 2, 3, 4}, fake.RequestsOf(fakes3.OpComplete)[0].Parts)
	}
	requireNoAborts(t, rt.wasabi, rt.scw)
}

func TestStreamingReplicationResumesAfterInterruption(t *testing.T) {
	for _, omitMarkers := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitMarkers=%t", omitMarkers), func(t *testing.T) {
			rt := setupReplicationTest(t)
			rt.wasabi.SetPageSize(1, omitMarkers)
			rt.scw.SetPageSize(1, omitMarkers)
			key := replicationKey("resumed")
			data := rt.addObject(t, key, 4*testPartSize)
			ctx, cancel := context.WithCancel(t.Context())
			rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
				if req.Op == fakes3.OpUpload && req.PartNumber == 3 {
					cancel()
					<-req.Done
					return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
				}
				return nil
			})

			require.Error(t, rt.c.tryReplicate(ctx))

			requireNoAborts(t, rt.wasabi, rt.scw)
			rt.requireNoSpools(t)
			require.Len(t, rt.uploadRows(t), 2)
			done := map[*fakes3.Server]map[int64]bool{}
			for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
				done[fake] = map[int64]bool{}
				for _, upload := range fake.Uploads() {
					for number := range upload.Parts {
						done[fake][number] = true
					}
				}
			}
			require.NotEmpty(t, done[rt.wasabi])
			require.False(t, done[rt.wasabi][3])
			before := map[*fakes3.Server]int{rt.b2: len(rt.b2.Requests()), rt.wasabi: len(rt.wasabi.Requests()), rt.scw: len(rt.scw.Requests())}

			rt.wasabi.SetHook(nil)
			rt.rewind(t)
			require.NoError(t, rt.c.tryReplicate(t.Context()))

			rt.requireReplicated(t, key, data)
			for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
				require.Len(t, fake.RequestsOf(fakes3.OpCreate), 1)
				require.GreaterOrEqual(t, len(fake.RequestsOf(fakes3.OpListPart)), len(done[fake]))
				var resumed []fakes3.Request
				for _, r := range fake.Requests()[before[fake]:] {
					if r.Op == fakes3.OpUpload {
						resumed = append(resumed, r)
					}
				}
				require.NotEmpty(t, resumed)
				for _, number := range requestedParts(resumed) {
					require.False(t, done[fake][number], "part %d uploaded again", number)
				}
			}
			for _, r := range rt.b2.Requests()[before[rt.b2]:] {
				if r.Op != fakes3.OpGet {
					continue
				}
				var start, end int64
				_, err := fmt.Sscanf(r.Range, "bytes=%d-%d", &start, &end)
				require.NoError(t, err)
				part := start/testPartSize + 1
				require.False(t, done[rt.wasabi][part] && done[rt.scw][part], "part %d downloaded again", part)
			}
		})
	}
}

func TestStreamingReplicationRetriesTransientPartError(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("transient")
	data := rt.addObject(t, key, 3*testPartSize)
	var failed atomic.Bool
	rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpUpload && req.PartNumber == 2 && failed.CompareAndSwap(false, true) {
			return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
		}
		return nil
	})

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	rt.requireReplicated(t, key, data)
	require.ElementsMatch(t, []int64{1, 2, 2, 3}, requestedParts(rt.wasabi.RequestsOf(fakes3.OpUpload)))
	require.Len(t, rt.wasabi.RequestsOf(fakes3.OpCreate), 1)
	requireNoAborts(t, rt.wasabi, rt.scw)
}

func TestStreamingReplicationSkipsExistingReplica(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("existing")
	data := rt.addObject(t, key, 3*testPartSize)
	rt.wasabi.PutObjectData(key, data)
	stale := rt.wasabi.StartUpload(key)
	rt.insertUploadRow(t, key, wasabiDC, stale, "")
	var headed atomic.Bool
	rt.scw.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpHead && headed.CompareAndSwap(false, true) {
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "Forbidden"}
		}
		return nil
	})

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	rt.requireReplicated(t, key, data)
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpCreate))
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpUpload))
	require.Equal(t, stale, rt.wasabi.RequestsOf(fakes3.OpAbort)[0].UploadID)
	require.Len(t, rt.scw.RequestsOf(fakes3.OpUpload), 3)
}

func TestStreamingReplicationVerifiesComplianceDenial(t *testing.T) {
	for _, op := range []fakes3.Op{fakes3.OpCreate, fakes3.OpUpload, fakes3.OpComplete} {
		t.Run(string(op), func(t *testing.T) {
			rt := setupReplicationTest(t)
			key := replicationKey("compliance")
			data := rt.addObject(t, key, 3*testPartSize)
			var headed, denied atomic.Bool
			rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
				switch {
				case req.Op == fakes3.OpHead && headed.CompareAndSwap(false, true):
					return &fakes3.Failure{Status: http.StatusNotFound, Code: "NotFound"}
				case req.Op == op && denied.CompareAndSwap(false, true):
					rt.wasabi.PutObjectData(key, data)
					return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
				}
				return nil
			})

			require.NoError(t, rt.c.tryReplicate(t.Context()))

			rt.requireReplicated(t, key, data)
			require.Len(t, rt.wasabi.RequestsOf(fakes3.OpHead), 2)
			if op != fakes3.OpCreate {
				require.Len(t, rt.wasabi.RequestsOf(fakes3.OpAbort), 1)
			}
		})
	}
}

func TestStreamingReplicationUnknownUpload(t *testing.T) {
	for _, tt := range []struct {
		name      string
		dc        string
		op        fakes3.Op
		code      string
		replica   bool
		recovered bool
	}{
		{name: "list parts", dc: scwDC, op: fakes3.OpListPart, code: "NoSuchKey", recovered: true},
		{name: "upload part", dc: scwDC, op: fakes3.OpUpload, code: "NoSuchUpload"},
		{name: "complete", dc: scwDC, op: fakes3.OpComplete, code: "NoSuchKey"},
		{name: "complete after another attempt completed", dc: wasabiDC, op: fakes3.OpComplete, code: "NoSuchUpload", replica: true, recovered: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			fake := map[string]*fakes3.Server{wasabiDC: rt.wasabi, scwDC: rt.scw}[tt.dc]
			key := replicationKey("unknown")
			data := rt.addObject(t, key, 3*testPartSize)
			stored := fake.StartUpload(key)
			rt.insertUploadRow(t, key, tt.dc, stored, "")
			var failed atomic.Bool
			fake.SetHook(func(req fakes3.Request) *fakes3.Failure {
				if req.Op == tt.op && failed.CompareAndSwap(false, true) {
					fake.DropUpload(stored)
					if tt.replica {
						fake.PutObjectData(key, data)
					}
					return &fakes3.Failure{Status: http.StatusNotFound, Code: tt.code}
				}
				return nil
			})

			err := rt.c.tryReplicate(t.Context())

			requireNoAborts(t, fake)
			if tt.recovered {
				require.NoError(t, err)
				rt.requireReplicated(t, key, data)
				return
			}
			require.Error(t, err)
			require.Empty(t, rt.uploadRows(t))
			rt.requireRetrySoon(t, key)
			rt.rewind(t)
			fake.SetHook(nil)
			require.NoError(t, rt.c.tryReplicate(t.Context()))
			rt.requireReplicated(t, key, data)
			require.Len(t, fake.RequestsOf(fakes3.OpCreate), 1)
		})
	}
}

func TestStreamingReplicationSourceProblems(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source []byte
	}{
		{name: "missing"},
		{name: "size mismatch", source: randomData(3*testPartSize + 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			key := replicationKey("source")
			rt.addObjectRows(t, key, 3*testPartSize, ente.Drive)
			if tt.source != nil {
				rt.b2.PutObjectData(key, tt.source)
			}
			stored := rt.wasabi.StartUpload(key)
			rt.insertUploadRow(t, key, wasabiDC, stored, "")
			pickedAt := time.Now()

			require.Error(t, rt.c.tryReplicate(t.Context()))

			require.Empty(t, rt.uploadRows(t))
			require.Empty(t, rt.wasabi.Uploads())
			require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpCreate))
			require.GreaterOrEqual(t, rt.lastAttempt(t, key), pickedAt.Add(-time.Minute).UnixMicro())
		})
	}
}

func TestStreamingReplicationChecksStoredUploads(t *testing.T) {
	for _, tt := range []struct {
		name   string
		etag   string
		reused bool
	}{
		{name: "other source", etag: `"other"`},
		{name: "unknown source", etag: "", reused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			key := replicationKey("stored")
			data := rt.addObject(t, key, 3*testPartSize)
			stored := rt.wasabi.StartUpload(key)
			rt.wasabi.PutPart(stored, 1, data[:testPartSize/2])
			rt.wasabi.PutPart(stored, 2, data[testPartSize:2*testPartSize])
			rt.insertUploadRow(t, key, wasabiDC, stored, tt.etag)

			require.NoError(t, rt.c.tryReplicate(t.Context()))

			rt.requireReplicated(t, key, data)
			parts := requestedParts(rt.wasabi.RequestsOf(fakes3.OpUpload))
			if tt.reused {
				require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpCreate))
				require.ElementsMatch(t, []int64{1, 3}, parts)
			} else {
				require.Equal(t, stored, rt.wasabi.RequestsOf(fakes3.OpAbort)[0].UploadID)
				require.ElementsMatch(t, []int64{1, 2, 3}, parts)
			}
		})
	}
}

func TestStreamingReplicationSourceChangesMidTransfer(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("changed")
	rt.addObject(t, key, 3*testPartSize)
	changed := randomData(3 * testPartSize)
	var replaced atomic.Bool
	rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpUpload && replaced.CompareAndSwap(false, true) {
			rt.b2.PutObjectData(key, changed)
		}
		return nil
	})

	require.ErrorIs(t, rt.c.tryReplicate(t.Context()), errSourceChanged)

	requireNoAborts(t, rt.wasabi, rt.scw)
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpComplete))
	require.Len(t, rt.uploadRows(t), 2)
	rt.requireRetrySoon(t, key)

	rt.rewind(t)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	rt.requireReplicated(t, key, changed)
	require.Len(t, rt.wasabi.RequestsOf(fakes3.OpAbort), 1)
}

func TestStreamingReplicationRecordFailure(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("record")
	data := rt.addObject(t, key, 3*testPartSize)
	rt.scw.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpComplete {
			_, err := rt.db.Exec(`DELETE FROM object_copies WHERE object_key = $1`, key)
			require.NoError(t, err)
		}
		return nil
	})

	require.Error(t, rt.c.tryReplicate(t.Context()))

	_, err := rt.db.Exec(`INSERT INTO object_copies(object_key, want_b2, b2, want_wasabi, wasabi, want_scw) VALUES ($1, true, 1, true, 1, true)`, key)
	require.NoError(t, err)
	rt.scw.SetHook(nil)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	rt.requireReplicated(t, key, data)
	require.Len(t, rt.scw.RequestsOf(fakes3.OpCreate), 1)
}

func TestStreamingReplicationStopsWhenObjectIsDeleted(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("deleted")
	rt.addObject(t, key, 3*testPartSize)
	var deleted atomic.Bool
	rt.scw.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpUpload && req.PartNumber == 3 && deleted.CompareAndSwap(false, true) {
			_, err := rt.db.Exec(`UPDATE object_copies SET want_wasabi = false, want_scw = false WHERE object_key = $1`, key)
			require.NoError(t, err)
		}
		return nil
	})

	require.ErrorIs(t, rt.c.tryReplicate(t.Context()), errSourceDeleted)

	for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
		require.Empty(t, fake.RequestsOf(fakes3.OpComplete))
		require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
		require.Empty(t, fake.Uploads())
	}
	require.Empty(t, rt.uploadRows(t))
}

func TestReplicationOfDeletedObjectDropsStoredUploads(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("deleted")
	rt.addObject(t, key, 3*testPartSize)
	_, err := rt.db.Exec(`UPDATE object_keys SET is_deleted = true WHERE object_key = $1`, key)
	require.NoError(t, err)
	stored := rt.wasabi.StartUpload(key)
	rt.insertUploadRow(t, key, wasabiDC, stored, "")

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	require.Empty(t, rt.wasabi.Uploads())
	require.Empty(t, rt.uploadRows(t))
	require.Empty(t, rt.b2.Requests())
}

func TestStreamingReplicationAbandonsHungDownload(t *testing.T) {
	rt := setupReplicationTest(t)
	rt.c.stream.idleTimeout = 300 * time.Millisecond
	key := replicationKey("hung")
	data := rt.addObject(t, key, 3*testPartSize)
	rt.b2.SetStall(func(fakes3.Request) bool { return true })

	start := time.Now()
	err := rt.c.tryReplicate(t.Context())

	require.ErrorIs(t, err, errDownloadStalled)
	require.Less(t, time.Since(start), 10*time.Second)
	requireNoAborts(t, rt.wasabi, rt.scw)
	rt.requireNoSpools(t)
	rt.requireRetrySoon(t, key)
	_, err = rt.c.ObjectCopiesRepo.GetAndLockUnreplicatedObject(t.Context())
	require.ErrorIs(t, err, sql.ErrNoRows)

	rt.b2.SetStall(nil)
	rt.rewind(t)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	rt.requireReplicated(t, key, data)
	require.Len(t, rt.wasabi.RequestsOf(fakes3.OpCreate), 1)
}

func TestStreamingReplicationStopsWhenLeaseIsLost(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("lease")
	rt.addObject(t, key, 6*testPartSize)
	rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpUpload && req.PartNumber == 2 {
			_, err := rt.db.Exec(`UPDATE object_copies SET last_attempt = last_attempt + 1000 WHERE object_key = $1`, key)
			require.NoError(t, err)
			select {
			case <-req.Done:
			case <-time.After(10 * time.Second):
			}
			return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
		}
		return nil
	})

	require.ErrorIs(t, rt.c.tryReplicate(t.Context()), errLeaseLost)

	requireNoAborts(t, rt.wasabi, rt.scw)
	require.Empty(t, rt.wasabi.RequestsOf(fakes3.OpComplete))
	require.Len(t, rt.uploadRows(t), 2)
	require.Greater(t, rt.lastAttempt(t, key), time.Now().Add(-time.Hour).UnixMicro())
}

func TestStreamingReplicationHeartbeat(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("heartbeat")
	rt.addObject(t, key, 10)
	copies, err := rt.c.ObjectCopiesRepo.GetAndLockUnreplicatedObject(t.Context())
	require.NoError(t, err)
	newJob := func() *streamJob {
		return &streamJob{c: rt.c, cfg: rt.c.stream, key: key, logger: log.WithField("test", t.Name()),
			lease: replicationLease{lastAttempt: copies.LastAttempt, confirmedAt: time.Now()}}
	}

	j := newJob()
	_, err = rt.db.Exec(`UPDATE object_copies SET last_attempt = $2 WHERE object_key = $1`, key, copies.LastAttempt+5)
	require.NoError(t, err)
	j.recoverLease(t.Context())
	require.Equal(t, copies.LastAttempt+5, j.lease.lastAttempt)
	require.True(t, j.beat(t.Context()))
	stale := newJob()
	stale.lease.confirmedAt = time.Now().Add(-replicationRepickWindow)
	stale.recoverLease(t.Context())
	require.Equal(t, copies.LastAttempt, stale.lease.lastAttempt)
	require.False(t, stale.beat(t.Context()))

	j.lease.lastAttempt = rt.lastAttempt(t, key)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		j.heartbeat(ctx, cancel)
	}()
	before := rt.lastAttempt(t, key)
	time.Sleep(5 * rt.c.stream.heartbeatInterval)
	require.Equal(t, before, rt.lastAttempt(t, key))
	j.moved.Add(1)
	require.Eventually(t, func() bool { return rt.lastAttempt(t, key) > before }, 5*time.Second, 5*time.Millisecond)

	_, err = rt.db.Exec(`UPDATE object_copies SET last_attempt = last_attempt + 1 WHERE object_key = $1`, key)
	require.NoError(t, err)
	j.moved.Add(1)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop after another attempt picked the object")
	}
	require.ErrorIs(t, context.Cause(ctx), errLeaseLost)
}

func newRangeIgnoringWorker(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(1<<30))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1024))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(worker.Close)
	return worker
}

func newProxyWorker(t *testing.T, requests *atomic.Int32, forwardRange bool) *httptest.Server {
	t.Helper()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		src, err := base64.StdEncoding.DecodeString(r.URL.Query().Get("src"))
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, string(src), nil)
		require.NoError(t, err)
		if forwardRange {
			req.Header.Set("Range", r.Header.Get("Range"))
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		for _, header := range []string{"Content-Range", "Content-Length", "ETag"} {
			if value := resp.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(worker.Close)
	return worker
}

func TestStreamingReplicationFallsBackWhenWorkerIgnoresRange(t *testing.T) {
	rt := setupReplicationTest(t)
	var requests atomic.Int32
	rt.c.workerURL = newRangeIgnoringWorker(t, &requests).URL
	key := replicationKey("worker-200")
	data := rt.addObject(t, key, 5*testPartSize)

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	rt.requireReplicated(t, key, data)
	probes := requests.Load()
	require.GreaterOrEqual(t, probes, int32(workerRangeFailureLimit))
	require.LessOrEqual(t, probes, int32(workerRangeFailureLimit+streamingPartsInFlight-1))
	require.Len(t, rt.b2.RequestsOf(fakes3.OpGet), 5)
	require.False(t, rt.c.useWorker())

	rt.c.stream.workerBypassUntil.Store(0)
	rt.c.stream.threshold = 1
	rt.addObject(t, replicationKey("worker-reprobe"), testPartSize)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	require.Equal(t, probes+1, requests.Load())
	require.False(t, rt.c.useWorker())
}

func TestStreamingReplicationDownloadsThroughWorker(t *testing.T) {
	for _, tt := range []struct {
		name         string
		forwardRange bool
		size         int64
		parts        int32
	}{
		{name: "ranged", forwardRange: true, size: 2*testPartSize + 1, parts: 3},
		{name: "whole object", size: testPartSize, parts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			rt.c.stream.threshold = 1
			var requests atomic.Int32
			rt.c.workerURL = newProxyWorker(t, &requests, tt.forwardRange).URL
			key := replicationKey("worker")
			data := rt.addObject(t, key, tt.size)

			require.NoError(t, rt.c.tryReplicate(t.Context()))

			rt.requireReplicated(t, key, data)
			require.Equal(t, tt.parts, requests.Load())
			require.Len(t, rt.b2.RequestsOf(fakes3.OpGet), int(tt.parts))
			require.Zero(t, rt.c.stream.workerRangeFailures.Load())
		})
	}
}

func TestStopReplicationStopsPicking(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("stopped")
	rt.addObject(t, key, 3*testPartSize)

	rt.c.StopReplication()
	rt.c.replicate(0)

	require.Empty(t, rt.b2.Requests())
	require.Zero(t, rt.lastAttempt(t, key))
}

func TestSweepOrphanUploads(t *testing.T) {
	for _, omitMarkers := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitMarkers=%t", omitMarkers), func(t *testing.T) {
			rt := setupReplicationTest(t)
			rt.c.stream.threshold = 1
			rt.wasabi.SetPageSize(2, omitMarkers)
			old := time.Now().Add(-15 * 24 * time.Hour)
			drive, photos, replicated := replicationKey("drive"), replicationKey("photos"), replicationKey("replicated")
			rt.addObjectRows(t, drive, 10, ente.Drive)
			rt.addObjectRows(t, photos, 10, ente.Photos)
			rt.addObjectRows(t, replicated, 10, ente.Drive)
			_, err := rt.db.Exec(`UPDATE object_copies SET wasabi = 1 WHERE object_key = $1`, replicated)
			require.NoError(t, err)

			rt.wasabi.StartUploadAt(drive, old)
			tracked := rt.wasabi.StartUploadAt(drive, old)
			rt.insertUploadRow(t, drive, wasabiDC, tracked, "")
			sharedBucket := rt.wasabi.StartUploadAt(drive, old)
			rt.insertUploadRow(t, drive, scwDC, sharedBucket, "")
			young := rt.wasabi.StartUpload(drive)
			legacy := rt.wasabi.StartUploadAt(photos, old)
			stale := rt.wasabi.StartUploadAt(replicated, old)
			rt.insertUploadRow(t, replicated, wasabiDC, stale, "")
			foreign := rt.wasabi.StartUploadAt("other/key", old)
			scwOrphan := rt.scw.StartUploadAt(drive, old)
			rt.insertUploadRow(t, replicationKey("gone"), scwDC, "expired", "")
			_, err = rt.db.Exec(`UPDATE replication_uploads SET created_at = 1 WHERE upload_id = 'expired'`)
			require.NoError(t, err)

			rt.c.sweepOrphanUploads(t.Context())

			require.ElementsMatch(t, []string{tracked, sharedBucket, young, legacy, foreign}, uploadIDs(rt.wasabi))
			require.Empty(t, rt.scw.Uploads())
			var aborted []string
			for _, r := range rt.scw.RequestsOf(fakes3.OpAbort) {
				aborted = append(aborted, r.UploadID)
			}
			require.ElementsMatch(t, []string{scwOrphan, "expired"}, aborted)
			require.Equal(t, []string{drive + " " + scwDC + " " + sharedBucket, drive + " " + wasabiDC + " " + tracked}, rt.uploadRows(t))
		})
	}
}

func TestSweepOrphanUploadsRunsOnOneInstance(t *testing.T) {
	rt := setupReplicationTest(t)
	rt.c.stream.threshold = 1
	key := replicationKey("drive")
	rt.addObjectRows(t, key, 10, ente.Drive)
	orphan := rt.wasabi.StartUploadAt(key, time.Now().Add(-15*24*time.Hour))
	other := &lock.LockController{TaskLockingRepo: rt.c.LockController.TaskLockingRepo, HostName: "other"}
	require.True(t, other.TryLock(orphanSweepLock, timeUtil.MicrosecondsAfterHours(1)))

	rt.c.sweepOrphanUploads(t.Context())
	require.Equal(t, []string{orphan}, uploadIDs(rt.wasabi))

	other.ReleaseLock(orphanSweepLock)
	rt.c.sweepOrphanUploads(t.Context())
	require.Empty(t, rt.wasabi.Uploads())
}

func TestReplicationUsesLegacyPathWhenAppLookupFails(t *testing.T) {
	rt := setupReplicationTest(t)
	rt.c.stream.threshold = 2 << 20
	key := replicationKey("lookup")
	data := rt.addObject(t, key, 3<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, rt.c.tryReplicate(ctx))

	rt.requireReplicated(t, key, data)
	for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
		require.Len(t, fake.RequestsOf(fakes3.OpPut), 1)
		require.Empty(t, fake.RequestsOf(fakes3.OpCreate))
	}
}

func TestReplicationOfDeletedPhotosObjectSkipsStorage(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("deleted-photos")
	rt.addAppObject(t, key, 3*testPartSize, ente.Photos)
	_, err := rt.db.Exec(`UPDATE object_keys SET is_deleted = true WHERE object_key = $1`, key)
	require.NoError(t, err)

	require.NoError(t, rt.c.tryReplicate(t.Context()))

	for _, fake := range []*fakes3.Server{rt.b2, rt.wasabi, rt.scw} {
		require.Empty(t, fake.Requests())
	}
}

func TestStreamingReplicationCapsConcurrentAttempts(t *testing.T) {
	rt := setupReplicationTest(t)
	rt.c.stream.slots = semaphore.NewWeighted(1)
	require.True(t, rt.c.stream.slots.TryAcquire(1))
	drive, photos := replicationKey("drive"), replicationKey("photos")
	rt.addObject(t, drive, 3*testPartSize)
	photosData := rt.addAppObject(t, photos, 3<<20, ente.Photos)

	for range 2 {
		require.NoError(t, rt.c.tryReplicate(t.Context()))
	}

	for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
		object, ok := fake.Object(photos)
		require.True(t, ok)
		require.Equal(t, photosData, object.Data)
		require.Empty(t, fake.RequestsOf(fakes3.OpCreate))
	}
	require.InDelta(t, time.Now().Add(-24*time.Hour+streamingDeferral).UnixMicro(), rt.lastAttempt(t, drive), float64(time.Minute.Microseconds()))
	_, err := rt.c.ObjectCopiesRepo.GetAndLockUnreplicatedObject(t.Context())
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestStreamingReplicationFailsFastOnDisk(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(rt *replicationTest)
		rows    int
	}{
		{name: "before transfer", prepare: func(rt *replicationTest) { rt.c.stream.diskReserve = 1 << 62 }},
		{name: "spool space promised elsewhere", rows: 2, prepare: func(rt *replicationTest) {
			rt.scw.SetHook(func(req fakes3.Request) *fakes3.Failure {
				if req.Op == fakes3.OpCreate {
					rt.c.stream.spoolPending.Add(1 << 62)
				}
				return nil
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := setupReplicationTest(t)
			key := replicationKey("disk")
			rt.addObject(t, key, 3*testPartSize)
			tt.prepare(rt)
			pickedAt := time.Now()

			require.ErrorIs(t, rt.c.tryReplicate(t.Context()), errInsufficientDisk)

			require.Empty(t, rt.b2.RequestsOf(fakes3.OpGet))
			requireNoAborts(t, rt.wasabi, rt.scw)
			require.Len(t, rt.uploadRows(t), tt.rows)
			require.GreaterOrEqual(t, rt.lastAttempt(t, key), pickedAt.Add(-time.Minute).UnixMicro())
			rt.requireNoSpools(t)
		})
	}
}

func TestStreamingReplicationInsertConflictAbortsOnlyItsUpload(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("conflict")
	data := rt.addObject(t, key, 3*testPartSize)
	rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
		if req.Op == fakes3.OpCreate {
			rt.insertUploadRow(t, key, wasabiDC, "other", "")
		}
		return nil
	})

	require.ErrorContains(t, rt.c.tryReplicate(t.Context()), "another attempt is uploading")

	aborts := rt.wasabi.RequestsOf(fakes3.OpAbort)
	require.Len(t, aborts, 1)
	require.Equal(t, rt.wasabi.RequestsOf(fakes3.OpCreate)[0].Key, aborts[0].Key)
	require.Empty(t, rt.wasabi.Uploads())
	require.Equal(t, []string{key + " " + wasabiDC + " other"}, rt.uploadRows(t))
	object, ok := rt.scw.Object(key)
	require.True(t, ok)
	require.Equal(t, data, object.Data)
}

func TestStreamingReplicationVerifyFailureAfterComplete(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("verify")
	data := rt.addObject(t, key, 3*testPartSize)
	var completed atomic.Bool
	rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
		switch {
		case req.Op == fakes3.OpComplete:
			completed.Store(true)
		case req.Op == fakes3.OpHead && completed.Load():
			return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
		}
		return nil
	})

	require.Error(t, rt.c.tryReplicate(t.Context()))

	require.Empty(t, rt.uploadRows(t))
	rt.requireRetrySoon(t, key)
	rt.wasabi.SetHook(nil)
	rt.rewind(t)
	require.NoError(t, rt.c.tryReplicate(t.Context()))
	rt.requireReplicated(t, key, data)
	require.Len(t, rt.wasabi.RequestsOf(fakes3.OpCreate), 1)
}

func TestStreamingReplicationOutcomesPerDestination(t *testing.T) {
	t.Run("one destination fails for good, the other transiently", func(t *testing.T) {
		rt := setupReplicationTest(t)
		key := replicationKey("mixed")
		rt.addObject(t, key, 3*testPartSize)
		rt.scw.SetHook(func(req fakes3.Request) *fakes3.Failure {
			if req.Op == fakes3.OpCreate {
				return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
			}
			return nil
		})
		rt.wasabi.SetHook(func(req fakes3.Request) *fakes3.Failure {
			if req.Op == fakes3.OpUpload && req.PartNumber == 3 {
				return &fakes3.Failure{Status: http.StatusServiceUnavailable, Code: "SlowDown"}
			}
			return nil
		})

		require.Error(t, rt.c.tryReplicate(t.Context()))

		rt.requireRetrySoon(t, key)
		requireNoAborts(t, rt.wasabi, rt.scw)
	})
	t.Run("no destination can make progress", func(t *testing.T) {
		rt := setupReplicationTest(t)
		key := replicationKey("denied")
		rt.addObject(t, key, 3*testPartSize)
		for _, fake := range []*fakes3.Server{rt.wasabi, rt.scw} {
			fake.SetHook(func(req fakes3.Request) *fakes3.Failure {
				if req.Op == fakes3.OpCreate {
					return &fakes3.Failure{Status: http.StatusBadRequest, Code: "InvalidBucketName"}
				}
				return nil
			})
		}
		pickedAt := time.Now()

		require.Error(t, rt.c.tryReplicate(t.Context()))

		require.GreaterOrEqual(t, rt.lastAttempt(t, key), pickedAt.Add(-time.Minute).UnixMicro())
	})
	t.Run("a destination with every part completes when a download fails", func(t *testing.T) {
		rt := setupReplicationTest(t)
		rt.c.stream.threshold = 1
		key := replicationKey("complete")
		data := rt.addObject(t, key, 2*testPartSize)
		stored := rt.wasabi.StartUpload(key)
		rt.wasabi.PutPart(stored, 1, data[:testPartSize])
		rt.wasabi.PutPart(stored, 2, data[testPartSize:])
		rt.insertUploadRow(t, key, wasabiDC, stored, "")
		rt.b2.SetHook(func(req fakes3.Request) *fakes3.Failure {
			if req.Op == fakes3.OpGet {
				return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
			}
			return nil
		})

		require.Error(t, rt.c.tryReplicate(t.Context()))

		object, ok := rt.wasabi.Object(key)
		require.True(t, ok)
		require.Equal(t, data, object.Data)
		var wasabi, scw sql.NullInt64
		require.NoError(t, rt.db.QueryRow(`SELECT wasabi, scw FROM object_copies WHERE object_key = $1`, key).Scan(&wasabi, &scw))
		require.True(t, wasabi.Valid)
		require.False(t, scw.Valid)
		require.Len(t, rt.uploadRows(t), 1)
	})
}

func TestCountingReadSeekerSkipsHashingPass(t *testing.T) {
	rt := setupReplicationTest(t)
	key := replicationKey("counted")
	uploadID := rt.wasabi.StartUpload(key)
	data := randomData(testPartSize)
	var moved atomic.Int64
	body := &countingReadSeeker{ReadSeeker: bytes.NewReader(data), length: int64(len(data)), moved: &moved}

	_, err := rt.c.wasabiDest.Client.UploadPartWithContext(t.Context(), &s3.UploadPartInput{
		Bucket: rt.c.wasabiDest.Bucket, Key: &key, UploadId: &uploadID, PartNumber: aws.Int64(1), Body: body,
	})

	require.NoError(t, err)
	require.NotEmpty(t, rt.wasabi.RequestsOf(fakes3.OpUpload)[0].ContentMD5)
	require.Equal(t, int64(len(data)), moved.Load())
}
