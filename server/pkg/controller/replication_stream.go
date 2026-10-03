package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/repo"
	fileutil "github.com/ente/museum/pkg/utils/file"
	"github.com/ente/museum/pkg/utils/s3copy"
	"github.com/ente/stacktrace"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const (
	defaultStreamingThreshold = int64(1) << 30
	defaultSpoolBudget        = int64(40) << 30
	defaultOrphanUploadDays   = 14
	streamingMinPartSize      = int64(64) << 20
	streamingMaxParts         = 1000
	streamingPartAlignment    = int64(1) << 20
	streamingPartsInFlight    = 2
	replicationRepickWindow   = 24 * time.Hour
	streamingRetryAfter       = time.Hour
	streamingDeferral         = 10 * time.Minute
	workerRangeFailureLimit   = 3
	workerReprobeInterval     = time.Hour
)

var (
	errSourceMissing      = errors.New("replication source object not found")
	errSourceSizeMismatch = errors.New("replication source size mismatch")
	errSourceDeleted      = errors.New("object was deleted during replication")
	errSourceChanged      = errors.New("replication source ETag changed")
	errRangeNotHonoured   = errors.New("ranged GET returned something other than the requested range")
	errDownloadStalled    = errors.New("download stalled")
	errDownloadTimedOut   = errors.New("download timed out")
	errS3CallTimedOut     = errors.New("S3 call timed out")
	errLeaseLost          = errors.New("object was picked by another replication attempt")
	errNoActiveTargets    = errors.New("no destination left to upload to")
	errInsufficientDisk   = errors.New("insufficient disk space for streaming replication")
)

type streamingConfig struct {
	threshold         int64
	slots             *semaphore.Weighted
	minPartSize       int64
	spool             *semaphore.Weighted
	spoolCapacity     int64
	diskReserve       int64
	idleTimeout       time.Duration
	heartbeatInterval time.Duration
	metadataTimeout   time.Duration
	completeTimeout   time.Duration
	orphanUploadAge   time.Duration
	uploadRetryDelays []time.Duration
	fetchRetryDelays  []time.Duration
	client            *http.Client

	// Spool bytes taken from the budget but not yet on disk.
	spoolPending        atomic.Int64
	workerRangeFailures atomic.Int32
	workerBypassUntil   atomic.Int64
	workerNotice        sync.Once
	diskNotice          sync.Once
}

func newStreamingConfig(workerCount int) *streamingConfig {
	threshold := viper.GetInt64("replication.streaming-threshold")
	if threshold <= 0 {
		threshold = defaultStreamingThreshold
	}
	// Leaves most workers to Photos and Locker objects while multi-day
	// streaming attempts run.
	slots := viper.GetInt64("replication.streaming-workers")
	if slots <= 0 {
		slots = int64(max(1, workerCount/3))
	}
	budget := viper.GetInt64("replication.spool-budget")
	if budget <= 0 {
		budget = defaultSpoolBudget
	}
	orphanDays := viper.GetInt("replication.orphan-upload-days")
	if orphanDays <= 0 {
		orphanDays = defaultOrphanUploadDays
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return &streamingConfig{
		threshold:     threshold,
		slots:         semaphore.NewWeighted(slots),
		minPartSize:   streamingMinPartSize,
		spool:         semaphore.NewWeighted(budget),
		spoolCapacity: budget,
		// Room for the largest legacy download, so spools never make the
		// legacy path's disk check fail.
		diskReserve:       InternalUserMaxFileSize + int64(2)<<30,
		idleTimeout:       2 * time.Minute,
		heartbeatInterval: 5 * time.Minute,
		metadataTimeout:   2 * time.Minute,
		completeTimeout:   15 * time.Minute,
		orphanUploadAge:   time.Duration(orphanDays) * 24 * time.Hour,
		uploadRetryDelays: []time.Duration{time.Second, 4 * time.Second, 16 * time.Second, 64 * time.Second},
		fetchRetryDelays: []time.Duration{time.Second, 4 * time.Second, 16 * time.Second, 64 * time.Second,
			2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute},
		client: &http.Client{Transport: transport},
	}
}

// Photos and Locker objects never exceed InternalUserMaxFileSize, so they
// always keep the legacy path.
func (c *ReplicationController3) isStreamingEligible(size int64, app string) bool {
	return size > InternalUserMaxFileSize || (size > c.stream.threshold && app == string(ente.Drive))
}

func (c *ReplicationController3) streamsObject(ctx context.Context, objectKey string, size int64) (bool, error) {
	if c.stream == nil || size <= min(c.stream.threshold, InternalUserMaxFileSize) {
		return false, nil
	}
	if size > InternalUserMaxFileSize {
		return true, nil
	}
	_, app, found, err := c.ObjectRepo.GetObjectSizeAndApp(ctx, objectKey)
	if err != nil {
		return false, stacktrace.Propagate(err, "Failed to fetch the object's app")
	}
	return found && c.isStreamingEligible(size, app), nil
}

func streamingPartSize(size int64, minPartSize int64) (int64, error) {
	partSize := max(minPartSize, s3copy.CeilDiv(size, streamingMaxParts))
	partSize = s3copy.CeilDiv(partSize, streamingPartAlignment) * streamingPartAlignment
	if partSize > ente.MaxMultipartPartSize {
		return 0, stacktrace.NewError("object of %d bytes needs %d-byte parts, above the %d-byte limit", size, partSize, ente.MaxMultipartPartSize)
	}
	return partSize, nil
}

func transferTimeout(length int64) time.Duration {
	return 10*time.Minute + time.Duration(length>>20)*time.Second
}

type streamTarget struct {
	dest     *UploadDestination
	markCopy func() error
	uploadID string
	etags    map[int64]string
	err      error
	finished bool
}

type replicationLease struct {
	lastAttempt int64
	confirmedAt time.Time
}

type streamJob struct {
	c          *ReplicationController3
	cfg        *streamingConfig
	key        string
	size       int64
	partSize   int64
	partCount  int64
	sourceETag string
	logger     *log.Entry
	moved      atomic.Int64
	lease      replicationLease

	mu      sync.Mutex
	targets []*streamTarget
}

// Never aborts uploads on transient errors or cancellation: the next attempt
// resumes them from replication_uploads.
func (c *ReplicationController3) replicateStreaming(ctx context.Context, copies *ente.ObjectCopies, size int64, logger *log.Entry) error {
	partSize, err := streamingPartSize(size, c.stream.minPartSize)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	j := &streamJob{
		c:         c,
		cfg:       c.stream,
		key:       copies.ObjectKey,
		size:      size,
		partSize:  partSize,
		partCount: s3copy.CeilDiv(size, partSize),
		logger:    logger.WithField("replication_path", "streaming"),
		lease:     replicationLease{lastAttempt: copies.LastAttempt, confirmedAt: time.Now()},
	}
	if copies.WantWasabi && copies.Wasabi == nil {
		j.targets = append(j.targets, &streamTarget{dest: c.wasabiDest, markCopy: func() error {
			return c.ObjectCopiesRepo.MarkObjectReplicatedWasabi(j.key)
		}})
	}
	if copies.WantSCW && copies.SCW == nil {
		j.targets = append(j.targets, &streamTarget{dest: c.scwDest, markCopy: func() error {
			return c.ObjectCopiesRepo.MarkObjectReplicatedScaleway(j.key)
		}})
	}
	j.logger.Infof("Streaming replication of %d bytes in %d parts of %d bytes", size, j.partCount, partSize)

	ctx, cancel := context.WithCancelCause(ctx)
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		j.heartbeat(ctx, cancel)
	}()
	err = j.run(ctx)
	cancel(nil)
	heartbeat.Wait()

	if cause := context.Cause(ctx); errors.Is(cause, errLeaseLost) {
		return stacktrace.Propagate(cause, "")
	}
	if j.shouldRetrySoon() {
		c.retryAttemptAfter(j.key, j.lease.lastAttempt, streamingRetryAfter, j.logger)
	}
	return err
}

func (c *ReplicationController3) deferStreaming(copies *ente.ObjectCopies, logger *log.Entry) {
	logger.Info("All streaming replication slots are busy, deferring the object")
	c.retryAttemptAfter(copies.ObjectKey, copies.LastAttempt, streamingDeferral, logger)
}

func (c *ReplicationController3) retryAttemptAfter(objectKey string, lastAttempt int64, after time.Duration, logger *log.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.ObjectCopiesRepo.RetryReplicationAttemptAfter(ctx, objectKey, lastAttempt, after); err != nil {
		logger.WithError(err).Warn("Failed to schedule an earlier replication retry")
	}
}

// Retry within the hour only when a destination can still succeed and the
// attempt made progress or failed in a way that usually clears up quickly.
func (j *streamJob) shouldRetrySoon() bool {
	for _, t := range j.targets {
		if t.err != nil && !isPermanentStreamingError(t.err) && (j.moved.Load() > 0 || isTransientStreamingError(t.err)) {
			return true
		}
	}
	return false
}

func isPermanentStreamingError(err error) bool {
	return errors.Is(err, errSourceMissing) || errors.Is(err, errSourceSizeMismatch) || errors.Is(err, errSourceDeleted) ||
		strings.Contains(err.Error(), "size of the uploaded file")
}

func isTransientStreamingError(err error) bool {
	for _, transient := range []error{errDownloadStalled, errDownloadTimedOut, errSourceChanged, errS3CallTimedOut} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var awsErr awserr.Error
	var netErr net.Error
	return (errors.As(err, &awsErr) && s3copy.IsTransient(awsErr)) || errors.As(err, &netErr)
}

func (j *streamJob) run(ctx context.Context) error {
	var pending []*streamTarget
	for _, t := range j.targets {
		if err := j.skipIfReplicated(ctx, t); err != nil {
			t.err = err
		} else if !t.finished {
			pending = append(pending, t)
		}
	}
	if len(pending) > 0 {
		if err := j.headSource(ctx); err != nil {
			if errors.Is(err, errSourceMissing) || errors.Is(err, errSourceSizeMismatch) {
				j.c.dropStoredUploads(j.key, j.logger)
			}
			for _, t := range pending {
				t.err = err
			}
			pending = nil
		}
	}
	if len(pending) > 0 {
		if err := j.checkDisk(j.partSize); err != nil {
			for _, t := range pending {
				t.err = err
			}
			pending = nil
		}
	}
	var active []*streamTarget
	for _, t := range pending {
		if err := j.openUpload(ctx, t); err != nil {
			t.err = err
		} else if !t.finished {
			active = append(active, t)
		}
	}
	if len(active) > 0 {
		err := j.transferParts(ctx, active)
		for _, t := range active {
			if t.err != nil || t.finished {
				continue
			}
			if int64(len(t.etags)) == j.partCount && ctx.Err() == nil {
				t.err = j.complete(ctx, t)
			} else if err != nil && !errors.Is(err, errNoActiveTargets) {
				t.err = err
			} else {
				t.err = stacktrace.NewError("expected %d uploaded parts, have %d", j.partCount, len(t.etags))
			}
		}
	}
	var errs []error
	for _, t := range j.targets {
		if t.err != nil {
			errs = append(errs, j.c.destinationFailure(t.dest, t.err, j.logger))
		}
	}
	return errors.Join(errs...)
}

func (j *streamJob) metadataContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, j.cfg.metadataTimeout)
}

// A 403 means the key is missing when the credentials can't list the bucket.
func (j *streamJob) headReplica(ctx context.Context, dest *UploadDestination) (int64, bool, error) {
	hctx, cancel := j.metadataContext(ctx)
	defer cancel()
	out, err := dest.Client.HeadObjectWithContext(hctx, &s3.HeadObjectInput{Bucket: dest.Bucket, Key: &j.key})
	if status := s3StatusCode(err); status == http.StatusNotFound || status == http.StatusForbidden {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, stacktrace.Propagate(err, "Fetching object info from bucket %s failed", *dest.Bucket)
	}
	return aws.Int64Value(out.ContentLength), true, nil
}

func (j *streamJob) skipIfReplicated(ctx context.Context, t *streamTarget) error {
	size, exists, err := j.headReplica(ctx, t.dest)
	if err != nil || !exists || size != j.size {
		return err
	}
	j.logger.WithField("destination", t.dest.Label).Info("Destination already has the object")
	return j.finish(ctx, t)
}

func (j *streamJob) headSource(ctx context.Context) error {
	hctx, cancel := j.metadataContext(ctx)
	defer cancel()
	out, err := j.c.b2Client.HeadObjectWithContext(hctx, &s3.HeadObjectInput{Bucket: j.c.b2Bucket, Key: &j.key})
	if s3StatusCode(err) == http.StatusNotFound {
		j.c.notifyDiscord("🔥 Could not find object in HotStorage: " + j.key)
		return stacktrace.Propagate(errSourceMissing, "")
	}
	if err != nil {
		return stacktrace.Propagate(err, "Failed to fetch source object info")
	}
	if size := aws.Int64Value(out.ContentLength); size != j.size {
		j.c.notifyDiscord(fmt.Sprintf("⚠️ Replication source size mismatch for %s: got %d bytes, expected %d", j.key, size, j.size))
		return stacktrace.Propagate(errSourceSizeMismatch, "source has %d bytes, expected %d", size, j.size)
	}
	j.sourceETag = normalizeETag(aws.StringValue(out.ETag))
	return nil
}

func normalizeETag(etag string) string {
	return strings.Trim(strings.TrimPrefix(etag, "W/"), `"`)
}

func etagsDiffer(a, b string) bool {
	a, b = normalizeETag(a), normalizeETag(b)
	return a != "" && b != "" && a != b
}

func (j *streamJob) openUpload(ctx context.Context, t *streamTarget) error {
	stored, err := j.c.ReplicationUploadsRepo.Get(ctx, j.key, t.dest.DC)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	logger := j.logger.WithField("destination", t.dest.Label)
	if stored != nil {
		if stored.PartSize != j.partSize || etagsDiffer(stored.SourceETag, j.sourceETag) {
			logger.Infof("Discarding upload %s made for a different source or part size", stored.UploadID)
			j.c.dropUpload(t.dest, j.key, stored.UploadID, true, logger)
		} else {
			etags, err := j.listParts(ctx, t.dest, stored.UploadID)
			if err == nil {
				t.uploadID, t.etags = stored.UploadID, etags
				logger.Infof("Resuming upload %s with %d of %d parts done", stored.UploadID, len(etags), j.partCount)
				return nil
			}
			if !isUnknownUploadError(err) {
				return stacktrace.Propagate(err, "Failed to list uploaded parts")
			}
			logger.Infof("Stored upload %s no longer exists", stored.UploadID)
			j.c.dropUpload(t.dest, j.key, stored.UploadID, false, logger)
		}
	}
	return j.createUpload(ctx, t)
}

func (j *streamJob) listParts(ctx context.Context, dest *UploadDestination, uploadID string) (map[int64]string, error) {
	lctx, cancel := j.metadataContext(ctx)
	defer cancel()
	parts, err := listMultipartUploadParts(lctx, dest.Client, dest.Bucket, j.key, uploadID)
	if err != nil {
		return nil, err
	}
	etags := make(map[int64]string, len(parts))
	for _, part := range parts {
		if part.PartNumber >= 1 && part.PartNumber <= j.partCount && part.Size == j.partLength(part.PartNumber) && part.ETag != "" {
			etags[part.PartNumber] = part.ETag
		}
	}
	return etags, nil
}

func (j *streamJob) createUpload(ctx context.Context, t *streamTarget) error {
	input := &s3.CreateMultipartUploadInput{Bucket: t.dest.Bucket, Key: &j.key}
	if t.dest.IsGlacier {
		input.StorageClass = aws.String(s3.ObjectStorageClassGlacier)
	}
	cctx, cancel := j.metadataContext(ctx)
	out, err := t.dest.Client.CreateMultipartUploadWithContext(cctx, input)
	cancel()
	if err != nil {
		if t.dest.HasComplianceHold && isAccessDenied(err) {
			return j.verifyExisting(ctx, t, err)
		}
		return stacktrace.Propagate(err, "Failed to create multipart upload in bucket %s", *t.dest.Bucket)
	}
	uploadID := aws.StringValue(out.UploadId)
	if uploadID == "" {
		return stacktrace.NewError("multipart upload in bucket %s has no upload ID", *t.dest.Bucket)
	}
	inserted, err := j.c.ReplicationUploadsRepo.Insert(ctx, repo.ReplicationUpload{
		ObjectKey:  j.key,
		DestDC:     t.dest.DC,
		UploadID:   uploadID,
		PartSize:   j.partSize,
		SourceETag: j.sourceETag,
	})
	if err != nil || !inserted {
		j.c.abortUpload(context.Background(), t.dest, j.key, uploadID, j.logger)
		if err != nil {
			return stacktrace.Propagate(err, "Failed to record multipart upload")
		}
		return stacktrace.NewError("another attempt is uploading %s to %s", j.key, t.dest.Label)
	}
	t.uploadID, t.etags = uploadID, map[int64]string{}
	return nil
}

func (j *streamJob) partLength(part int64) int64 {
	return min(j.partSize, j.size-(part-1)*j.partSize)
}

func (j *streamJob) transferParts(ctx context.Context, active []*streamTarget) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(streamingPartsInFlight)
	for part := int64(1); part <= j.partCount && gctx.Err() == nil; part++ {
		g.Go(func() error {
			return j.transferPart(gctx, part, active)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}

func (j *streamJob) isActive(t *streamTarget) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return t.err == nil && !t.finished
}

func (j *streamJob) targetsNeeding(part int64, active []*streamTarget) (needing []*streamTarget, anyActive bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, t := range active {
		if t.err == nil && !t.finished {
			anyActive = true
			if t.etags[part] == "" {
				needing = append(needing, t)
			}
		}
	}
	return needing, anyActive
}

func (j *streamJob) transferPart(ctx context.Context, part int64, active []*streamTarget) error {
	targets, anyActive := j.targetsNeeding(part, active)
	if !anyActive {
		return errNoActiveTargets
	}
	if len(targets) == 0 {
		return nil
	}
	length := j.partLength(part)
	weight := min(length, j.cfg.spoolCapacity)
	if err := j.cfg.spool.Acquire(ctx, weight); err != nil {
		return err
	}
	defer j.cfg.spool.Release(weight)
	reservation := &spoolReservation{pending: &j.cfg.spoolPending, length: length}
	reservation.reset()
	defer reservation.release()
	if err := j.checkDisk(0); err != nil {
		return err
	}
	spool, err := fileutil.CreateUniqueTemporaryFile(j.c.tempStorage, j.key, fmt.Sprintf("part%d", part))
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	if err := j.fetchPart(ctx, spool, reservation, part, length); err != nil {
		return stacktrace.Propagate(err, "Failed to download part %d", part)
	}
	for _, t := range targets {
		if !j.isActive(t) {
			continue
		}
		etag, err := j.uploadPart(ctx, t, part, spool, length)
		if err == nil {
			j.mu.Lock()
			t.etags[part] = etag
			j.mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j.uploadFailed(ctx, t, part, err)
	}
	if _, anyActive := j.targetsNeeding(part, active); !anyActive {
		return errNoActiveTargets
	}
	return nil
}

func (j *streamJob) uploadFailed(ctx context.Context, t *streamTarget, part int64, err error) {
	j.mu.Lock()
	if t.err != nil || t.finished {
		j.mu.Unlock()
		return
	}
	t.err = stacktrace.Propagate(err, "Failed to upload part %d to bucket %s", part, *t.dest.Bucket)
	j.mu.Unlock()
	switch {
	case t.dest.HasComplianceHold && isAccessDenied(err):
		// The part group's context ends once no destination is left active,
		// which is the case while this one is being verified.
		verr := j.verifyExisting(context.WithoutCancel(ctx), t, err)
		j.mu.Lock()
		t.err = verr
		j.mu.Unlock()
	case isUnknownUploadError(err):
		j.c.dropUpload(t.dest, j.key, t.uploadID, false, j.logger)
	}
}

// Spools may only use what is left after the largest legacy download, and
// space promised to in-flight spools (spoolPending) counts as used.
func (j *streamJob) checkDisk(extra int64) error {
	err := fileutil.EnsureAvailableSpace(j.c.tempStorage, j.cfg.spoolPending.Load()+j.cfg.diskReserve+extra)
	if err == nil {
		return nil
	}
	j.logger.WithError(err).Error("Not enough disk space for streaming replication")
	j.cfg.diskNotice.Do(func() {
		j.c.notifyDiscord("⚠️ Streaming replication is short of disk space in " + j.c.tempStorage)
	})
	return stacktrace.Propagate(errInsufficientDisk, "%v", err)
}

func (j *streamJob) fetchPart(ctx context.Context, spool *os.File, reservation *spoolReservation, part int64, length int64) error {
	start := (part - 1) * j.partSize
	for attempt := 0; ; attempt++ {
		if err := spool.Truncate(0); err != nil {
			return stacktrace.Propagate(err, "")
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return stacktrace.Propagate(err, "")
		}
		reservation.reset()
		w := &reservedWriter{w: spool, reservation: reservation}
		err := j.c.downloadRange(ctx, j.key, start, start+length-1, j.size, j.sourceETag, w, &j.moved)
		if err == nil {
			return nil
		}
		if attempt >= len(j.cfg.fetchRetryDelays) || ctx.Err() != nil {
			return err
		}
		j.logger.WithError(err).Infof("Retrying download of part %d", part)
		if err := sleepWithContext(ctx, j.cfg.fetchRetryDelays[attempt]); err != nil {
			return err
		}
	}
}

func (j *streamJob) uploadPart(ctx context.Context, t *streamTarget, part int64, spool *os.File, length int64) (string, error) {
	for attempt := 0; ; attempt++ {
		etag, err := j.uploadPartOnce(ctx, t, part, spool, length)
		if err == nil {
			return etag, nil
		}
		if attempt >= len(j.cfg.uploadRetryDelays) || ctx.Err() != nil || !isTransientS3Error(err) {
			return "", err
		}
		j.logger.WithError(err).WithField("destination", t.dest.Label).Infof("Retrying upload of part %d", part)
		if err := sleepWithContext(ctx, j.cfg.uploadRetryDelays[attempt]); err != nil {
			return "", err
		}
	}
}

func (j *streamJob) uploadPartOnce(ctx context.Context, t *streamTarget, part int64, spool *os.File, length int64) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, transferTimeout(length))
	defer cancel()
	// A seekable body makes the SDK send Content-MD5, which object-locked
	// buckets require.
	body := &countingReadSeeker{ReadSeeker: io.NewSectionReader(spool, 0, length), length: length, moved: &j.moved}
	out, err := t.dest.Client.UploadPartWithContext(pctx, &s3.UploadPartInput{
		Bucket:        t.dest.Bucket,
		Key:           &j.key,
		UploadId:      &t.uploadID,
		PartNumber:    aws.Int64(part),
		Body:          body,
		ContentLength: aws.Int64(length),
	}, func(r *request.Request) {
		r.Retryer = client.NoOpRetryer{}
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return "", stacktrace.Propagate(errS3CallTimedOut, "%v", err)
		}
		return "", err
	}
	if aws.StringValue(out.ETag) == "" {
		return "", stacktrace.NewError("part upload returned no ETag")
	}
	return *out.ETag, nil
}

func (j *streamJob) complete(ctx context.Context, t *streamTarget) error {
	if int64(len(t.etags)) != j.partCount {
		return stacktrace.NewError("expected %d uploaded parts, have %d", j.partCount, len(t.etags))
	}
	copies, err := j.c.ObjectCopiesRepo.Get(ctx, j.key)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	state, err := j.c.ObjectRepo.GetObjectState(j.key)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if !j.c.needsReplica(copies, t.dest) || state.IsFileDeleted || state.IsUserDeleted {
		j.c.dropUpload(t.dest, j.key, t.uploadID, true, j.logger)
		return stacktrace.Propagate(errSourceDeleted, "")
	}
	parts := make([]*s3.CompletedPart, 0, j.partCount)
	for part := int64(1); part <= j.partCount; part++ {
		parts = append(parts, &s3.CompletedPart{ETag: aws.String(t.etags[part]), PartNumber: aws.Int64(part)})
	}
	cctx, cancel := context.WithTimeout(ctx, j.cfg.completeTimeout)
	_, err = t.dest.Client.CompleteMultipartUploadWithContext(cctx, &s3.CompleteMultipartUploadInput{
		Bucket:          t.dest.Bucket,
		Key:             &j.key,
		UploadId:        &t.uploadID,
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: parts},
	})
	cancel()
	if err != nil {
		switch {
		case t.dest.HasComplianceHold && isAccessDenied(err):
			return j.verifyExisting(ctx, t, err)
		case isUnknownUploadError(err):
			j.c.dropUpload(t.dest, j.key, t.uploadID, false, j.logger)
			// An earlier Complete whose response was lost may have finished it.
			if size, exists, herr := j.headReplica(ctx, t.dest); herr == nil && exists && size == j.size {
				return j.record(t)
			}
		}
		return stacktrace.Propagate(err, "Failed to complete multipart upload in bucket %s", *t.dest.Bucket)
	}
	vctx, cancel := j.metadataContext(ctx)
	defer cancel()
	verr := j.c.verifyUploadedFileSize(vctx, &UploadInput{ObjectKey: j.key, ExpectedSize: j.size, Logger: j.logger}, t.dest)
	j.c.dropUpload(t.dest, j.key, t.uploadID, false, j.logger)
	if verr != nil {
		return stacktrace.Propagate(verr, "Failed to verify upload")
	}
	j.logger.WithField("destination", t.dest.Label).Info("Completed streaming upload")
	return j.record(t)
}

// Compliance-locked Wasabi denies writes to keys that already exist.
func (j *streamJob) verifyExisting(ctx context.Context, t *streamTarget, cause error) error {
	j.logger.Infof("Ignoring object that already exists on remote (we'll verify it using a HEAD check): %s", cause)
	vctx, cancel := j.metadataContext(ctx)
	defer cancel()
	if err := j.c.verifyUploadedFileSize(vctx, &UploadInput{ObjectKey: j.key, ExpectedSize: j.size, Logger: j.logger}, t.dest); err != nil {
		return stacktrace.Propagate(err, "Failed to verify upload")
	}
	return j.finish(ctx, t)
}

// The replica already exists, so any stored upload for it is useless.
func (j *streamJob) finish(ctx context.Context, t *streamTarget) error {
	stored, err := j.c.ReplicationUploadsRepo.Get(ctx, j.key, t.dest.DC)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if stored != nil {
		j.c.dropUpload(t.dest, j.key, stored.UploadID, true, j.logger)
	}
	return j.record(t)
}

func (j *streamJob) record(t *streamTarget) error {
	if err := j.c.recordReplica(j.key, t.dest, j.logger, t.markCopy); err != nil {
		return err
	}
	j.mu.Lock()
	t.finished = true
	j.mu.Unlock()
	return nil
}

func (c *ReplicationController3) needsReplica(copies *ente.ObjectCopies, dest *UploadDestination) bool {
	switch {
	case copies == nil:
		return false
	case dest == c.wasabiDest:
		return copies.WantWasabi && copies.Wasabi == nil
	case dest == c.scwDest:
		return copies.WantSCW && copies.SCW == nil
	}
	return false
}

func (c *ReplicationController3) destination(dc string) *UploadDestination {
	for _, dest := range []*UploadDestination{c.wasabiDest, c.scwDest} {
		if dest.DC == dc {
			return dest
		}
	}
	return nil
}

// Best effort: an upload left behind is aborted by the sweeper.
func (c *ReplicationController3) dropUpload(dest *UploadDestination, objectKey string, uploadID string, abort bool, logger *log.Entry) {
	if abort {
		c.abortUpload(context.Background(), dest, objectKey, uploadID, logger)
	}
	c.deleteUploadRow(context.Background(), repo.ReplicationUpload{ObjectKey: objectKey, DestDC: dest.DC, UploadID: uploadID}, logger)
}

func (c *ReplicationController3) deleteUploadRow(ctx context.Context, u repo.ReplicationUpload, logger *log.Entry) {
	ctx, cancel := context.WithTimeout(ctx, c.stream.metadataTimeout)
	defer cancel()
	if err := c.ReplicationUploadsRepo.Delete(ctx, u.ObjectKey, u.DestDC, u.UploadID); err != nil {
		logger.WithError(err).Warn("Failed to delete stored replication upload")
	}
}

func (c *ReplicationController3) abortUpload(ctx context.Context, dest *UploadDestination, objectKey string, uploadID string, logger *log.Entry) bool {
	ctx, cancel := context.WithTimeout(ctx, c.stream.metadataTimeout)
	defer cancel()
	_, err := dest.Client.AbortMultipartUploadWithContext(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   dest.Bucket,
		Key:      &objectKey,
		UploadId: &uploadID,
	})
	if err != nil && !isUnknownUploadError(err) {
		logger.WithError(err).WithFields(log.Fields{"destination": dest.Label, "upload_id": uploadID}).Warn("Failed to abort multipart upload")
		return false
	}
	return true
}

func (c *ReplicationController3) dropStoredUploads(objectKey string, logger *log.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), c.stream.metadataTimeout)
	defer cancel()
	uploads, err := c.ReplicationUploadsRepo.ListForObject(ctx, objectKey)
	if err != nil {
		logger.WithError(err).Warn("Failed to list stored replication uploads")
		return
	}
	for _, u := range uploads {
		if dest := c.destination(u.DestDC); dest != nil {
			c.dropUpload(dest, objectKey, u.UploadID, true, logger)
		}
	}
}

// Keeps last_attempt fresh while bytes move, so no other worker re-picks a
// long healthy attempt, while a hung one still becomes pickable after 24 h.
func (j *streamJob) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(j.cfg.heartbeatInterval)
	defer ticker.Stop()
	var beat int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		moved := j.moved.Load()
		if moved == beat {
			continue
		}
		if !j.beat(ctx) {
			j.logger.Warn("Another replication attempt picked this object, stopping")
			cancel(errLeaseLost)
			return
		}
		beat = moved
	}
}

func (j *streamJob) beat(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	next, ok, err := j.c.ObjectCopiesRepo.ExtendReplicationAttempt(hctx, j.key, j.lease.lastAttempt)
	if err != nil {
		j.logger.WithError(err).Warn("Failed to extend replication attempt")
		j.recoverLease(ctx)
		return true
	}
	if ok {
		j.lease = replicationLease{lastAttempt: next, confirmedAt: time.Now()}
	}
	return ok
}

// A failed extension may still have been written. No other attempt can pick
// the object within 24 h of our last confirmed beat, so a newer value is ours.
func (j *streamJob) recoverLease(ctx context.Context) {
	if time.Since(j.lease.confirmedAt) >= replicationRepickWindow {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	copies, err := j.c.ObjectCopiesRepo.Get(rctx, j.key)
	if err == nil && copies != nil && copies.LastAttempt > j.lease.lastAttempt {
		j.lease.lastAttempt = copies.LastAttempt
	}
}

func (c *ReplicationController3) downloadRange(ctx context.Context, objectKey string, start, end, total int64, sourceETag string, w io.Writer, moved *atomic.Int64) error {
	presignedURL, err := c.getPresignedB2URL(objectKey)
	if err != nil {
		return stacktrace.Propagate(err, "Could not create presigned URL for downloading object")
	}
	if c.useWorker() {
		workerURL, err := url.Parse(c.workerURL)
		if err != nil {
			return stacktrace.Propagate(err, "Invalid worker URL %s", c.workerURL)
		}
		q := workerURL.Query()
		q.Add("src", base64.StdEncoding.EncodeToString([]byte(presignedURL)))
		workerURL.RawQuery = q.Encode()
		err = c.getRange(ctx, workerURL.String(), start, end, total, sourceETag, w, moved)
		if !errors.Is(err, errRangeNotHonoured) {
			if err == nil {
				c.stream.workerRangeFailures.Store(0)
			}
			return err
		}
		c.workerRangeFailed(err)
	}
	return c.getRange(ctx, presignedURL, start, end, total, sourceETag, w, moved)
}

func (c *ReplicationController3) useWorker() bool {
	return !c.S3Config.AreLocalBuckets() && c.workerURL != "" && time.Now().UnixNano() >= c.stream.workerBypassUntil.Load()
}

func (c *ReplicationController3) workerRangeFailed(err error) {
	if c.stream.workerRangeFailures.Add(1) < workerRangeFailureLimit {
		return
	}
	c.stream.workerBypassUntil.Store(time.Now().Add(workerReprobeInterval).UnixNano())
	log.WithError(err).Warnf("Replication worker doesn't honour Range requests, streaming replication downloads directly for %s", workerReprobeInterval)
	c.stream.workerNotice.Do(func() {
		c.notifyDiscord("⚠️ Replication worker doesn't honour Range requests, streaming replication is downloading directly from B2")
	})
}

// Fails with errRangeNotHonoured, before writing anything, if the response
// isn't exactly the requested range.
func (c *ReplicationController3) getRange(ctx context.Context, rawURL string, start, end, total int64, sourceETag string, w io.Writer, moved *atomic.Int64) error {
	want := end - start + 1
	ctx, cancelTimeout := context.WithTimeoutCause(ctx, transferTimeout(want), errDownloadTimedOut)
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	failed := func(err error) error {
		if cause := context.Cause(ctx); errors.Is(cause, errDownloadStalled) || errors.Is(cause, errDownloadTimedOut) {
			return stacktrace.Propagate(cause, "%v", err)
		}
		return stacktrace.Propagate(err, "Ranged GET failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := c.stream.client.Do(req)
	if err != nil {
		return failed(err)
	}
	// Closing an unread body drops the connection instead of draining it.
	defer resp.Body.Close()
	wholeObject := start == 0 && end == total-1
	switch {
	case resp.StatusCode == http.StatusOK && wholeObject && (resp.ContentLength < 0 || resp.ContentLength == total):
	case resp.StatusCode == http.StatusOK:
		return stacktrace.Propagate(errRangeNotHonoured, "got HTTP 200")
	case resp.StatusCode != http.StatusPartialContent:
		return stacktrace.NewError("ranged GET failed with HTTP status %s", resp.Status)
	case resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, total):
		return stacktrace.Propagate(errRangeNotHonoured, "got Content-Range %q", resp.Header.Get("Content-Range"))
	case resp.ContentLength >= 0 && resp.ContentLength != want:
		return stacktrace.Propagate(errRangeNotHonoured, "got Content-Length %d", resp.ContentLength)
	}
	if etag := resp.Header.Get("ETag"); etagsDiffer(etag, sourceETag) {
		return stacktrace.Propagate(errSourceChanged, "got ETag %s, expected %s", etag, sourceETag)
	}
	idle := time.AfterFunc(c.stream.idleTimeout, func() { cancel(errDownloadStalled) })
	defer idle.Stop()
	body := &idleTimeoutReader{r: io.LimitReader(resp.Body, want+1), timer: idle, timeout: c.stream.idleTimeout, moved: moved}
	n, err := io.Copy(w, body)
	if err != nil {
		return failed(err)
	}
	if n != want {
		return stacktrace.NewError("ranged GET returned %d bytes, expected %d", n, want)
	}
	return nil
}

type idleTimeoutReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
	moved   *atomic.Int64
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
		r.moved.Add(int64(n))
	}
	return n, err
}

// Counts only what is read after the SDK's first full pass, which hashes the
// body locally, so a stalled destination doesn't look like progress.
type countingReadSeeker struct {
	io.ReadSeeker
	length int64
	read   int64
	hashed bool
	moved  *atomic.Int64
}

func (r *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := r.ReadSeeker.Read(p)
	r.read += int64(n)
	if r.hashed {
		r.moved.Add(int64(n))
	}
	return n, err
}

func (r *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if r.read >= r.length && offset == 0 && whence == io.SeekStart {
		r.hashed = true
	}
	return r.ReadSeeker.Seek(offset, whence)
}

type spoolReservation struct {
	pending   *atomic.Int64
	length    int64
	remaining int64
}

func (r *spoolReservation) reset() {
	r.pending.Add(r.length - r.remaining)
	r.remaining = r.length
}

func (r *spoolReservation) consume(n int64) {
	n = min(n, r.remaining)
	r.remaining -= n
	r.pending.Add(-n)
}

func (r *spoolReservation) release() {
	r.pending.Add(-r.remaining)
	r.remaining = 0
}

type reservedWriter struct {
	w           io.Writer
	reservation *spoolReservation
}

func (w *reservedWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.reservation.consume(int64(n))
	return n, err
}

func s3StatusCode(err error) int {
	var reqErr awserr.RequestFailure
	if errors.As(err, &reqErr) {
		return reqErr.StatusCode()
	}
	return 0
}

func isAccessDenied(err error) bool {
	var reqErr awserr.RequestFailure
	return errors.As(err, &reqErr) && reqErr.Code() == "AccessDenied"
}

func isTransientS3Error(err error) bool {
	return errors.Is(err, errS3CallTimedOut) || s3copy.IsTransient(err)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
