package s3copy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/stacktrace"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

const (
	// Not 5 GiB: B2's CopyObject rejects sources above 5 × 10⁹ bytes (rclone uses the same cutoff).
	MaxSingleCopySize = int64(4768) << 20
	MinPartSize       = int64(256) << 20
	MaxPartSize       = int64(5) << 30
	DefaultWorkers    = 8
	partAlignment     = int64(1) << 20
	abortTimeout      = 2 * time.Minute
	// Process-wide, across all copies: a 100-file request alone could
	// otherwise run 800 part copies at once.
	MaxConcurrentPartCopies = 32
)

var (
	ErrSourceSizeMismatch = errors.New("copy source size mismatch")
	errPartCopyTimedOut   = errors.New("part copy attempt timed out")

	retryDelays   = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}
	partCopySlots = make(chan struct{}, MaxConcurrentPartCopies)
	// The S3 clients have no HTTP timeout; a hung part copy would otherwise
	// hold its slot until the request context ends.
	partCopyAttemptTimeout = 10 * time.Minute
)

type Options struct {
	MaxSingleCopySize int64
	MinPartSize       int64
	MaxParts          int
	Workers           int
	// Called after CreateMultipartUpload and before any part is copied; an
	// error aborts the upload.
	OnUploadCreated func(uploadID string) error
	// Zero means no timeout beyond ctx.
	SingleCopyTimeout time.Duration
	MetadataTimeout   time.Duration
	CompleteTimeout   time.Duration
}

func DefaultOptions(maxParts int) Options {
	return Options{
		MaxSingleCopySize: MaxSingleCopySize,
		MinPartSize:       MinPartSize,
		MaxParts:          maxParts,
		Workers:           DefaultWorkers,
	}
}

func (o Options) IsMultipart(size int64) bool {
	return size > o.MaxSingleCopySize
}

func (o Options) PartSize(size int64) int64 {
	partSize := max(o.MinPartSize, ceilDiv(size, int64(max(o.MaxParts, 1))))
	return ceilDiv(partSize, partAlignment) * partAlignment
}

func Copy(ctx context.Context, client *s3.S3, bucket, srcKey, dstKey string, size int64, opts Options) error {
	if opts.IsMultipart(size) {
		return CopyMultipart(ctx, client, bucket, srcKey, dstKey, size, opts)
	}
	source := copySource(bucket, srcKey)
	ctx, cancel := withTimeout(ctx, opts.SingleCopyTimeout)
	defer cancel()
	_, err := client.CopyObjectWithContext(ctx, &s3.CopyObjectInput{
		Bucket:     &bucket,
		CopySource: &source,
		Key:        &dstKey,
	})
	return stacktrace.Propagate(err, "")
}

func copySource(bucket, key string) string {
	return escapePath(bucket) + "/" + escapePath(key)
}

func escapePath(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		if isUnreserved(c) || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// CopyMultipart ignores opts.MaxSingleCopySize.
func CopyMultipart(ctx context.Context, client *s3.S3, bucket, srcKey, dstKey string, size int64, opts Options) (err error) {
	source := copySource(bucket, srcKey)
	partSize := opts.PartSize(size)
	if partSize > MaxPartSize {
		return stacktrace.NewError("object of %d bytes needs %d-byte parts, above the %d-byte limit", size, partSize, MaxPartSize)
	}
	headCtx, cancelHead := withTimeout(ctx, opts.MetadataTimeout)
	head, err := client.HeadObjectWithContext(headCtx, &s3.HeadObjectInput{Bucket: &bucket, Key: &srcKey})
	cancelHead()
	if err != nil {
		return stacktrace.Propagate(err, "failed to head copy source")
	}
	if aws.Int64Value(head.ContentLength) != size {
		return stacktrace.Propagate(ErrSourceSizeMismatch, "copy source %s has %d bytes, expected %d", srcKey, aws.Int64Value(head.ContentLength), size)
	}
	createCtx, cancelCreate := withTimeout(ctx, opts.MetadataTimeout)
	created, err := client.CreateMultipartUploadWithContext(createCtx, &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &dstKey})
	cancelCreate()
	if err != nil {
		return stacktrace.Propagate(err, "failed to create multipart upload")
	}
	uploadID := aws.StringValue(created.UploadId)
	if uploadID == "" {
		return stacktrace.NewError("multipart upload for %s has no upload ID", dstKey)
	}
	defer func() {
		if err != nil {
			abortUpload(client, bucket, dstKey, uploadID)
		}
	}()
	if opts.OnUploadCreated != nil {
		if err = opts.OnUploadCreated(uploadID); err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	parts, err := copyParts(ctx, client, bucket, source, dstKey, uploadID, size, partSize, opts.Workers)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	completeCtx, cancelComplete := withTimeout(ctx, opts.CompleteTimeout)
	defer cancelComplete()
	_, err = client.CompleteMultipartUploadWithContext(completeCtx, &s3.CompleteMultipartUploadInput{
		Bucket:          &bucket,
		Key:             &dstKey,
		UploadId:        &uploadID,
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: parts},
	})
	return stacktrace.Propagate(err, "failed to complete multipart copy")
}

// The parts come back ordered by part number, as CompleteMultipartUpload requires.
func copyParts(ctx context.Context, client *s3.S3, bucket, source, dstKey, uploadID string, size, partSize int64, workers int) ([]*s3.CompletedPart, error) {
	partCount := int(ceilDiv(size, partSize))
	parts := make([]*s3.CompletedPart, partCount)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(workers, 1))
	for i := range partCount {
		if gctx.Err() != nil {
			break
		}
		start := int64(i) * partSize
		end := min(start+partSize, size) - 1
		input := &s3.UploadPartCopyInput{
			Bucket:          &bucket,
			Key:             &dstKey,
			UploadId:        &uploadID,
			PartNumber:      aws.Int64(int64(i + 1)),
			CopySource:      &source,
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
		}
		g.Go(func() error {
			partETag, err := copyPart(gctx, client, input)
			if err != nil {
				return stacktrace.Propagate(err, "failed to copy part %d", *input.PartNumber)
			}
			parts[i] = &s3.CompletedPart{ETag: &partETag, PartNumber: input.PartNumber}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	return parts, nil
}

func copyPart(ctx context.Context, client *s3.S3, input *s3.UploadPartCopyInput) (string, error) {
	for attempt := 0; ; attempt++ {
		eTag, err := copyPartOnce(ctx, client, input)
		if err == nil {
			return eTag, nil
		}
		if attempt >= len(retryDelays) || ctx.Err() != nil || !isTransient(err) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryDelays[attempt]):
		}
	}
}

func copyPartOnce(ctx context.Context, s3Client *s3.S3, input *s3.UploadPartCopyInput) (string, error) {
	select {
	case partCopySlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-partCopySlots }()
	attemptCtx, cancel := context.WithTimeout(ctx, partCopyAttemptTimeout)
	defer cancel()
	// copyPart is the only retry layer.
	out, err := s3Client.UploadPartCopyWithContext(attemptCtx, input, func(r *request.Request) {
		r.Retryer = client.NoOpRetryer{}
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return "", stacktrace.Propagate(errPartCopyTimedOut, "%v", err)
		}
		return "", err
	}
	if out.CopyPartResult == nil || aws.StringValue(out.CopyPartResult.ETag) == "" {
		return "", stacktrace.NewError("part copy returned no ETag")
	}
	return *out.CopyPartResult.ETag, nil
}

func isTransient(err error) bool {
	if errors.Is(err, errPartCopyTimedOut) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if request.IsErrorThrottle(err) {
		return true
	}
	var reqErr awserr.RequestFailure
	if errors.As(err, &reqErr) {
		status := reqErr.StatusCode()
		if status == http.StatusTooManyRequests || (status >= 500 && status != http.StatusNotImplemented) {
			return true
		}
		// The SDK treats an unparseable error body as retryable; only retry
		// 4xx codes it lists, such as 400 RequestTimeout.
		return reqErr.Code() != request.ErrCodeSerialization && request.IsErrorRetryable(err)
	}
	return request.IsErrorRetryable(err)
}

// A failed abort leaves the upload to the temp-object cleanup cron.
func abortUpload(client *s3.S3, bucket, key, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	_, err := client.AbortMultipartUploadWithContext(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: &uploadID,
	})
	if err != nil {
		log.WithError(err).WithField("object_key", key).Warn("Failed to abort multipart copy")
	}
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}
