package s3copy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/internal/testutil/fakes3"
	"github.com/stretchr/testify/require"
)

const mib = int64(1) << 20

func newTestClient(t *testing.T) (*s3.S3, *fakes3.Server) {
	t.Helper()
	fake := fakes3.New(t)
	sess, err := session.NewSession(&aws.Config{
		Credentials:      credentials.NewStaticCredentials("key", "secret", ""),
		Endpoint:         aws.String(fake.URL),
		Region:           aws.String("us-east-1"),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(true),
	})
	require.NoError(t, err)
	delays := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = delays })
	return s3.New(sess), fake
}

func testOptions(maxParts int) Options {
	return Options{MaxSingleCopySize: 10 * mib, MinPartSize: fakes3.MinPartSize, MaxParts: maxParts, Workers: 3}
}

func singleWorkerOptions() Options {
	opts := testOptions(10000)
	opts.Workers = 1
	return opts
}

func setPartCopySlots(t *testing.T, n int) {
	t.Helper()
	slots := partCopySlots
	partCopySlots = make(chan struct{}, n)
	t.Cleanup(func() { partCopySlots = slots })
}

func TestPartSize(t *testing.T) {
	const gib = int64(1) << 30
	for _, tt := range []struct {
		size     int64
		maxParts int
		want     int64
	}{
		{MaxSingleCopySize + 1, 10000, MinPartSize},
		{10 * gib, 10000, MinPartSize},
		{5000 * gib, 10000, 512 * mib},
		{5000 * gib, 1000, 5 * gib},
		{5000*gib + 1, 10000, 513 * mib},
		{3000*gib + 7, 1000, 3073 * mib},
	} {
		partSize := DefaultOptions(tt.maxParts).PartSize(tt.size)
		require.Equal(t, tt.want, partSize, "size %d, maxParts %d", tt.size, tt.maxParts)
		require.Zero(t, partSize%mib)
		require.LessOrEqual(t, CeilDiv(tt.size, partSize), int64(tt.maxParts))
		require.LessOrEqual(t, partSize, MaxPartSize)
	}
	require.False(t, DefaultOptions(10000).IsMultipart(MaxSingleCopySize))
	require.True(t, DefaultOptions(10000).IsMultipart(MaxSingleCopySize+1))
}

func TestCopyBelowThresholdUsesCopyObject(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 10*mib)

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 10*mib, testOptions(10000)))

	requests := fake.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, fakes3.OpCopy, requests[0].Op)
	require.Equal(t, fakes3.Bucket+"/1/src", requests[0].CopySource)
	object, ok := fake.Object("1/dst")
	require.True(t, ok)
	require.Equal(t, 10*mib, object.Size)
}

func TestCopyAboveThresholdUsesMultipart(t *testing.T) {
	client, fake := newTestClient(t)
	size := 22*mib + 5
	fake.PutObject("1/src", size)
	var recorded []string
	opts := testOptions(10000)
	opts.OnUploadCreated = func(uploadID string) error {
		require.Empty(t, fake.RequestsOf(fakes3.OpPartCopy))
		recorded = append(recorded, uploadID)
		return nil
	}

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", size, opts))

	require.Equal(t, []string{"upload-1"}, recorded)
	require.Empty(t, fake.RequestsOf(fakes3.OpCopy))
	require.Len(t, fake.RequestsOf(fakes3.OpHead), 1)
	parts := fake.RequestsOf(fakes3.OpPartCopy)
	require.Len(t, parts, 5)
	ranges := map[int64]string{}
	for _, part := range parts {
		require.Equal(t, fakes3.Bucket+"/1/src", part.CopySource)
		require.Equal(t, "upload-1", part.UploadID)
		ranges[part.PartNumber] = part.Range
	}
	require.Equal(t, map[int64]string{
		1: "bytes=0-5242879",
		2: "bytes=5242880-10485759",
		3: "bytes=10485760-15728639",
		4: "bytes=15728640-20971519",
		5: "bytes=20971520-23068676",
	}, ranges)
	complete := fake.RequestsOf(fakes3.OpComplete)
	require.Len(t, complete, 1)
	require.Equal(t, []int64{1, 2, 3, 4, 5}, complete[0].Parts)
	object, ok := fake.Object("1/dst")
	require.True(t, ok)
	require.Equal(t, size, object.Size)
	require.Empty(t, fake.Uploads())
}

func TestCopyPartCountCappedByMaxParts(t *testing.T) {
	client, fake := newTestClient(t)
	size := 20*mib + 1
	fake.PutObject("1/src", size)

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", size, testOptions(3)))

	parts := fake.RequestsOf(fakes3.OpPartCopy)
	require.Len(t, parts, 3)
	require.Equal(t, []int64{1, 2, 3}, fake.RequestsOf(fakes3.OpComplete)[0].Parts)
	object, _ := fake.Object("1/dst")
	require.Equal(t, size, object.Size)
}

func TestCopyRetriesTransientPartErrors(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)
	var mu sync.Mutex
	failures := 0
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		mu.Lock()
		defer mu.Unlock()
		if r.Op == fakes3.OpPartCopy && r.PartNumber == 2 && failures < 2 {
			failures++
			if failures == 1 {
				return &fakes3.Failure{Status: http.StatusServiceUnavailable, Code: "SlowDown"}
			}
			return &fakes3.Failure{Status: http.StatusBadRequest, Code: "RequestTimeout"}
		}
		return nil
	})

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, testOptions(10000)))

	require.Equal(t, 2, failures)
	require.Len(t, fake.RequestsOf(fakes3.OpPartCopy), 5)
	require.Empty(t, fake.RequestsOf(fakes3.OpAbort))
	_, ok := fake.Object("1/dst")
	require.True(t, ok)
}

func TestCopyAbortsAfterPersistentPartFailure(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy && r.PartNumber == 2 {
			return &fakes3.Failure{Status: http.StatusInternalServerError, Code: "InternalError"}
		}
		return nil
	})

	require.Error(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, testOptions(10000)))

	failed := 0
	for _, part := range fake.RequestsOf(fakes3.OpPartCopy) {
		if part.PartNumber == 2 {
			failed++
		}
	}
	require.Equal(t, 1+len(retryDelays), failed)
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.RequestsOf(fakes3.OpComplete))
	require.Empty(t, fake.Uploads())
	_, ok := fake.Object("1/dst")
	require.False(t, ok)
}

func TestCopyDoesNotRetryPermanentPartErrors(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy {
			return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
		}
		return nil
	})

	err := Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, singleWorkerOptions())

	require.ErrorContains(t, err, "AccessDenied")
	require.Len(t, fake.RequestsOf(fakes3.OpPartCopy), 1)
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.Uploads())
}

func TestCopyAbortsWhenUploadCannotBeRecorded(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)
	opts := testOptions(10000)
	opts.OnUploadCreated = func(string) error { return context.Canceled }

	require.ErrorIs(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, opts), context.Canceled)

	require.Empty(t, fake.RequestsOf(fakes3.OpPartCopy))
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.Uploads())
}

func TestCopyRejectsSourceOfUnexpectedSize(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)

	require.ErrorIs(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 13*mib, testOptions(10000)), ErrSourceSizeMismatch)

	require.Empty(t, fake.RequestsOf(fakes3.OpCreate))
}

func TestCopyStopsWhenCancelled(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 50*mib)
	ctx, cancel := context.WithCancel(t.Context())
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpPartCopy {
			cancel()
		}
		return nil
	})

	require.Error(t, Copy(ctx, client, fakes3.Bucket, "1/src", "1/dst", 50*mib, singleWorkerOptions()))

	require.LessOrEqual(t, len(fake.RequestsOf(fakes3.OpPartCopy)), 2)
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.Uploads())
}

func TestCopySourceEscapesKeys(t *testing.T) {
	client, fake := newTestClient(t)
	key := "1/a b+c%d?é&=#~_.-x"
	fake.PutObject(key, 12*mib)

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, key, "1/small", 12*mib, Options{MaxSingleCopySize: 16 * mib}))
	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, key, "1/dst", 12*mib, testOptions(10000)))

	want := fakes3.Bucket + "/1/a%20b%2Bc%25d%3F%C3%A9%26%3D%23~_.-x"
	require.Equal(t, want, fake.RequestsOf(fakes3.OpCopy)[0].CopySource)
	for _, part := range fake.RequestsOf(fakes3.OpPartCopy) {
		require.Equal(t, want, part.CopySource)
	}
	_, ok := fake.Object("1/dst")
	require.True(t, ok)
}

func TestCopyMultipartIgnoresThreshold(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 6*mib)

	require.NoError(t, CopyMultipart(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 6*mib, testOptions(10000)))

	require.Empty(t, fake.RequestsOf(fakes3.OpCopy))
	require.Len(t, fake.RequestsOf(fakes3.OpPartCopy), 2)
	object, ok := fake.Object("1/dst")
	require.True(t, ok)
	require.Equal(t, 6*mib, object.Size)
}

func TestCopyAbortsWhenCompleteFails(t *testing.T) {
	client, fake := newTestClient(t)
	fake.PutObject("1/src", 12*mib)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpComplete {
			return &fakes3.Failure{Status: http.StatusBadRequest, Code: "InvalidPart"}
		}
		return nil
	})

	require.ErrorContains(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, testOptions(10000)), "InvalidPart")

	require.Len(t, fake.RequestsOf(fakes3.OpComplete), 1)
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.Uploads())
	_, ok := fake.Object("1/dst")
	require.False(t, ok)
}

func TestPartCopiesShareProcessWideLimit(t *testing.T) {
	client, fake := newTestClient(t)
	setPartCopySlots(t, 2)
	var mu sync.Mutex
	inFlight, peak := 0, 0
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op != fakes3.OpPartCopy {
			return nil
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})
	opts := testOptions(10000)
	opts.Workers = 8
	var wg sync.WaitGroup
	for _, key := range []string{"1/a", "1/b"} {
		fake.PutObject(key, 50*mib)
		wg.Go(func() {
			require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, key, key+"-copy", 50*mib, opts))
		})
	}
	wg.Wait()

	require.Len(t, fake.RequestsOf(fakes3.OpPartCopy), 20)
	require.Equal(t, 2, peak)
}

func TestPartCopyWaitForSlotStopsWhenCancelled(t *testing.T) {
	client, fake := newTestClient(t)
	setPartCopySlots(t, 1)
	partCopySlots <- struct{}{}
	fake.PutObject("1/src", 12*mib)
	ctx, cancel := context.WithCancel(t.Context())
	opts := testOptions(10000)
	opts.OnUploadCreated = func(string) error {
		cancel()
		return nil
	}

	require.ErrorIs(t, Copy(ctx, client, fakes3.Bucket, "1/src", "1/dst", 12*mib, opts), context.Canceled)

	require.Empty(t, fake.RequestsOf(fakes3.OpPartCopy))
	require.Len(t, fake.RequestsOf(fakes3.OpAbort), 1)
	require.Empty(t, fake.Uploads())
}

func TestHungPartCopyIsRetried(t *testing.T) {
	client, fake := newTestClient(t)
	timeout := partCopyAttemptTimeout
	partCopyAttemptTimeout = 100 * time.Millisecond
	t.Cleanup(func() { partCopyAttemptTimeout = timeout })
	fake.PutObject("1/src", 12*mib)
	var mu sync.Mutex
	hung := false
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		mu.Lock()
		first := r.Op == fakes3.OpPartCopy && r.PartNumber == 1 && !hung
		if first {
			hung = true
		}
		mu.Unlock()
		if first {
			select {
			case <-r.Done:
			case <-time.After(5 * time.Second):
				t.Error("the hung part copy wasn't cancelled")
			}
			return &fakes3.Failure{Status: http.StatusServiceUnavailable, Code: "SlowDown"}
		}
		return nil
	})

	require.NoError(t, Copy(t.Context(), client, fakes3.Bucket, "1/src", "1/dst", 12*mib, testOptions(10000)))

	attempts := 0
	for _, part := range fake.RequestsOf(fakes3.OpPartCopy) {
		if part.PartNumber == 1 {
			attempts++
		}
	}
	require.Equal(t, 2, attempts)
	_, ok := fake.Object("1/dst")
	require.True(t, ok)
}
