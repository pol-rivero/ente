package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/internal/testutil/fakes3"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mib = int64(1) << 20

// Key-based helpers over fakes3.
type fakeS3 struct {
	*fakes3.Server
}

func newFakeS3(t *testing.T) fakeS3 {
	t.Helper()
	return fakeS3{fakes3.New(t)}
}

func (f fakeS3) uploadIDs(key string) []string {
	var ids []string
	for id, upload := range f.Uploads() {
		if upload.Key == key {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f fakeS3) uploadID(t *testing.T, key string) string {
	t.Helper()
	ids := f.uploadIDs(key)
	require.Len(t, ids, 1, key)
	return ids[0]
}

func (f fakeS3) uploadPart(t *testing.T, key string, number int64, size int64) {
	t.Helper()
	f.PutPartSize(f.uploadID(t, key), number, size)
}

func (f fakeS3) complete(t *testing.T, key string) {
	t.Helper()
	f.CompleteUpload(f.uploadID(t, key))
}

func (f fakeS3) dropUpload(key string) {
	for _, id := range f.uploadIDs(key) {
		f.DropUpload(id)
	}
}

func (f fakeS3) hasUpload(key string) bool {
	return len(f.uploadIDs(key)) > 0
}

func (f fakeS3) hasObject(key string) bool {
	_, ok := f.Object(key)
	return ok
}

// An empty key counts the requests for every key.
func (f fakeS3) count(op fakes3.Op, key string) int {
	n := 0
	for _, r := range f.RequestsOf(op) {
		if key == "" || r.Key == key {
			n++
		}
	}
	return n
}

func accessDenied() *fakes3.Failure {
	return &fakes3.Failure{Status: http.StatusForbidden, Code: "AccessDenied"}
}

// Fails the given operation on the given keys (all keys if none).
func (f fakeS3) failOn(op fakes3.Op, keys ...string) {
	f.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == op && (len(keys) == 0 || slices.Contains(keys, r.Key)) {
			return accessDenied()
		}
		return nil
	})
}

// HEAD reports the key missing the next n times.
func (f fakeS3) missHeads(key string, n int) {
	var mu sync.Mutex
	f.SetHook(func(r fakes3.Request) *fakes3.Failure {
		mu.Lock()
		defer mu.Unlock()
		if r.Op != fakes3.OpHead || r.Key != key || n == 0 {
			return nil
		}
		n--
		return &fakes3.Failure{Status: http.StatusNotFound, Code: "NotFound"}
	})
}

// Runs fn before each CreateMultipartUpload is handled.
func (f fakeS3) beforeCreate(fn func(key string)) {
	f.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpCreate {
			fn(r.Key)
		}
		return nil
	})
}

func setupResumeTest(t *testing.T) (*FileController, *sql.DB, fakeS3) {
	t.Helper()
	delays := headNotFoundRetryDelays
	headNotFoundRetryDelays = []gotime.Duration{gotime.Millisecond, gotime.Millisecond}
	t.Cleanup(func() { headNotFoundRetryDelays = delays })
	fake := newFakeS3(t)
	c, db := newUploadTestController(t, fake.URL, 100*gib)
	return c, db, fake
}

func startResumeTestUpload(t *testing.T, c *FileController, app ente.App, contentLength int64, partMD5s []string) ente.MultipartUploadURLs {
	t.Helper()
	upload, err := c.GetMultipartUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.MultipartUploadURLRequest{ContentLength: contentLength, PartLength: 5 * mib, PartMD5s: partMD5s}, app, "client", false)
	require.NoError(t, err)
	return upload
}

func resumePartsCompleted(t *testing.T, db *sql.DB, key string) sql.NullInt64 {
	t.Helper()
	var completed sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT resume_parts_completed FROM temp_objects WHERE object_key = $1`, key).Scan(&completed))
	return completed
}

func requireExpiredAndReleased(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	var expiry int64
	var released bool
	require.NoError(t, db.QueryRow(`SELECT expiration_time, reservation_released FROM temp_objects WHERE object_key = $1`, key).
		Scan(&expiry, &released))
	require.LessOrEqual(t, expiry, time.Microseconds(), key)
	require.True(t, released, key)
}

func tempObjectKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT object_key FROM temp_objects ORDER BY object_key`)
	require.NoError(t, err)
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())
	return keys
}

func setTempObjectExpiry(t *testing.T, db *sql.DB, key string, expiry int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE temp_objects SET expiration_time = $1 WHERE object_key = $2`, expiry, key)
	require.NoError(t, err)
}

func partNumbers(urls map[int64]string) []int64 {
	numbers := make([]int64, 0, len(urls))
	for number := range urls {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	return numbers
}

func signedHeaders(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	return parsed.Query().Get("X-Amz-SignedHeaders")
}

func requireResumeURLValidity(t *testing.T, resume ente.MultipartUploadResume, want gotime.Duration) {
	t.Helper()
	urls := slices.Collect(maps.Values(resume.PartURLs))
	for _, rawURL := range append(urls, resume.CompleteURL) {
		parsed, err := url.Parse(rawURL)
		require.NoError(t, err)
		seconds, err := strconv.ParseInt(parsed.Query().Get("X-Amz-Expires"), 10, 64)
		require.NoError(t, err)
		require.InDelta(t, want.Seconds(), float64(seconds), 5, rawURL)
	}
}

func TestResumeMultipartUploadReportsProgress(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, []string{checksum, checksum, checksum})
	require.Contains(t, signedHeaders(t, upload.PartURLs[1]), "content-md5")
	var partLength int64
	require.NoError(t, db.QueryRow(`SELECT part_length FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).Scan(&partLength))
	require.Equal(t, 5*mib, partLength)
	initialExpiry := tempObjectExpiry(t, db, upload.ObjectKey)

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.False(t, resume.Completed)
	require.Empty(t, resume.CompletedParts)
	require.Equal(t, []int64{1, 2, 3}, partNumbers(resume.PartURLs))
	require.Equal(t, initialExpiry, tempObjectExpiry(t, db, upload.ObjectKey))
	require.False(t, resumePartsCompleted(t, db, upload.ObjectKey).Valid)
	requireResumeURLValidity(t, resume, PreSignedPartUploadRequestDuration)

	fake.uploadPart(t, upload.ObjectKey, 1, 5*mib)
	before := time.Microseconds()
	resume, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	after := time.Microseconds()
	require.Equal(t, []ente.MultipartUploadPart{{PartNumber: 1, ETag: `"etag-1"`, Size: 5 * mib}}, resume.CompletedParts)
	require.Equal(t, []int64{2, 3}, partNumbers(resume.PartURLs))
	for _, partURL := range resume.PartURLs {
		require.Contains(t, partURL, "uploadId=upload-1")
		require.Contains(t, signedHeaders(t, partURL), "content-length")
		require.NotContains(t, signedHeaders(t, partURL), "content-md5")
	}
	require.Contains(t, resume.PartURLs[3], "partNumber=3")
	require.Contains(t, resume.CompleteURL, "uploadId=upload-1")
	extension := 2 * PreSignedPartUploadRequestDuration.Microseconds()
	expiry := tempObjectExpiry(t, db, upload.ObjectKey)
	require.GreaterOrEqual(t, expiry, before+extension)
	require.LessOrEqual(t, expiry, after+extension)
	require.Greater(t, expiry, initialExpiry)
	require.Equal(t, int64(1), resumePartsCompleted(t, db, upload.ObjectKey).Int64)

	requireResumeURLValidity(t, resume, PreSignedPartUploadRequestDuration)

	// Without progress the expiry stays, so the URLs must expire an hour
	// before it; a row that expires sooner is gone.
	for remaining, validity := range map[gotime.Duration]gotime.Duration{
		3 * gotime.Hour:       2 * gotime.Hour,
		70 * gotime.Minute:    10 * gotime.Minute,
		64 * gotime.Minute:    0,
		gotime.Hour:           0,
		2 * gotime.Minute:     0,
		30 * 24 * gotime.Hour: PreSignedPartUploadRequestDuration,
	} {
		shortExpiry := time.Microseconds() + remaining.Microseconds()
		_, err = db.Exec(`UPDATE temp_objects SET expiration_time = $1, reservation_released = FALSE WHERE object_key = $2`,
			shortExpiry, upload.ObjectKey)
		require.NoError(t, err)
		got, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
		if validity == 0 {
			testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
			requireExpiredAndReleased(t, db, upload.ObjectKey)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, shortExpiry, tempObjectExpiry(t, db, upload.ObjectKey))
		requireResumeURLValidity(t, got, validity)
	}
	_, err = db.Exec(`UPDATE temp_objects SET reservation_released = FALSE WHERE object_key = $1`, upload.ObjectKey)
	require.NoError(t, err)
	shortExpiry := time.MicrosecondsAfterHours(1)
	setTempObjectExpiry(t, db, upload.ObjectKey, shortExpiry)

	fake.uploadPart(t, upload.ObjectKey, 2, 5*mib)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Greater(t, tempObjectExpiry(t, db, upload.ObjectKey), shortExpiry)
	require.Equal(t, int64(2), resumePartsCompleted(t, db, upload.ObjectKey).Int64)

	longExpiry := time.MicrosecondsAfterDays(60)
	setTempObjectExpiry(t, db, upload.ObjectKey, longExpiry)
	fake.uploadPart(t, upload.ObjectKey, 3, 2*mib)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, longExpiry, tempObjectExpiry(t, db, upload.ObjectKey))
	require.Equal(t, int64(3), resumePartsCompleted(t, db, upload.ObjectKey).Int64)

	body, err := json.Marshal(resume)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &fields))
	require.ElementsMatch(t, []string{"completedParts", "partURLs", "completeURL"}, keysOf(fields))
	require.Contains(t, string(fields["partURLs"]), `"2":`)
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestResumeMultipartUploadPaginatesParts(t *testing.T) {
	c, _, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 25*mib, nil)
	for _, number := range []int64{1, 2, 3, 5} {
		fake.uploadPart(t, upload.ObjectKey, number, 5*mib)
	}
	fake.SetPageSize(2, false)

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, 2, fake.count(fakes3.OpListPart, ""))
	got := make([]int64, 0)
	for _, part := range resume.CompletedParts {
		got = append(got, part.PartNumber)
	}
	require.Equal(t, []int64{1, 2, 3, 5}, got)
	require.Equal(t, []int64{4}, partNumbers(resume.PartURLs))
}

func TestResumeMultipartUploadPaginatesWithoutNextMarker(t *testing.T) {
	c, _, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 25*mib, nil)
	for _, number := range []int64{1, 2, 3, 5} {
		fake.uploadPart(t, upload.ObjectKey, number, 5*mib)
	}
	fake.SetPageSize(2, true)

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Len(t, resume.CompletedParts, 4)
	require.Equal(t, []int64{4}, partNumbers(resume.PartURLs))

	fake.SetListQuirks(fakes3.ListQuirks{AlwaysTruncatedParts: true})
	listsBefore := fake.count(fakes3.OpListPart, "")
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.ErrorContains(t, err, "did not advance")
	require.Equal(t, 3, fake.count(fakes3.OpListPart, "")-listsBefore)
}

func TestResumeMultipartUploadTreatsWrongSizedPartsAsMissing(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, upload.ObjectKey, 1, 5*mib)
	fake.uploadPart(t, upload.ObjectKey, 2, 3*mib)
	fake.uploadPart(t, upload.ObjectKey, 3, 2*mib)
	fake.uploadPart(t, upload.ObjectKey, 4, 1*mib)

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, []ente.MultipartUploadPart{
		{PartNumber: 1, ETag: `"etag-1"`, Size: 5 * mib},
		{PartNumber: 3, ETag: `"etag-3"`, Size: 2 * mib},
	}, resume.CompletedParts)
	require.Equal(t, []int64{2}, partNumbers(resume.PartURLs))
	require.Equal(t, int64(2), resumePartsCompleted(t, db, upload.ObjectKey).Int64)
}

func TestResumeMultipartUploadAfterLostCompleteResponse(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	for number, size := range []int64{5 * mib, 5 * mib, 2 * mib} {
		fake.uploadPart(t, upload.ObjectKey, int64(number+1), size)
	}
	fake.complete(t, upload.ObjectKey)
	setTempObjectExpiry(t, db, upload.ObjectKey, time.MicrosecondsAfterHours(1))
	fake.missHeads(upload.ObjectKey, len(headNotFoundRetryDelays))

	before := time.Microseconds()
	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, ente.MultipartUploadResume{Completed: true}, resume)
	require.GreaterOrEqual(t, tempObjectExpiry(t, db, upload.ObjectKey), before+completedUploadMinValidity.Microseconds())

	laterExpiry := time.MicrosecondsAfterDays(20)
	setTempObjectExpiry(t, db, upload.ObjectKey, laterExpiry)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, laterExpiry, tempObjectExpiry(t, db, upload.ObjectKey))

	thumbKey := uploadLimitsKey("resume-thumb", 10)
	stageUploadLimitsObjects(t, db, thumbKey)
	fake.PutObject(thumbKey, 10)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
	file, err := c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, collectionID, upload.ObjectKey, thumbKey), "", ente.Drive, false)
	require.NoError(t, err)
	require.Equal(t, 12*mib, file.File.Size)

	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
	testutil.RequireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey), http.StatusGone, ente.UploadGone)
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.True(t, fake.hasObject(upload.ObjectKey))
}

func TestResumeMultipartUploadGoneWhenObjectMissing(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	aborted := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.dropUpload(aborted.ObjectKey)
	truncated := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, truncated.ObjectKey, 1, 5*mib)
	fake.complete(t, truncated.ObjectKey)

	untouched := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)

	for _, key := range []string{aborted.ObjectKey, truncated.ObjectKey} {
		_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
		testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
		requireExpiredAndReleased(t, db, key)
	}
	require.Equal(t, len(headNotFoundRetryDelays)+1, fake.count(fakes3.OpHead, aborted.ObjectKey))
	require.Equal(t, 1, fake.count(fakes3.OpHead, truncated.ObjectKey))

	require.Equal(t, 2, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasObject(truncated.ObjectKey))
	require.Equal(t, []string{untouched.ObjectKey}, tempObjectKeys(t, db))
}

func TestResumeMultipartUploadExpiryCap(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	createdAt := time.Microseconds() - (89 * 24 * gotime.Hour).Microseconds()
	_, err := db.Exec(`UPDATE temp_objects SET created_at = $1, expiration_time = $2 WHERE object_key = $3`,
		createdAt, time.MicrosecondsAfterHours(1), upload.ObjectKey)
	require.NoError(t, err)
	fake.uploadPart(t, upload.ObjectKey, 1, 5*mib)

	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, createdAt+maxResumableUploadAge.Microseconds(), tempObjectExpiry(t, db, upload.ObjectKey))
}

func TestResumeAndAbortRejectForeignAndUnsupportedUploads(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	otherUserID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "resume-other@ente.com", CreationTime: 1})
	future := time.MicrosecondsAfterDays(14)
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, is_multipart, upload_id, content_length, part_length)
		VALUES ('2/other', $1, 'b2-eu-cen', $2, 'drive', 'file_upload', TRUE, 'u', 10, 10),
		       ('1/foreign-row', $1, 'b2-eu-cen', $2, 'drive', 'file_upload', TRUE, 'u', 10, 10),
		       ('1/legacy', $1, 'b2-eu-cen', NULL, 'drive', 'file_upload', TRUE, 'u', 10, 10),
		       ('1/v1', $1, 'b2-eu-cen', $3, 'drive', 'file_upload', TRUE, 'u', NULL, NULL),
		       ('1/pre-151', $1, 'b2-eu-cen', $3, 'drive', 'file_upload', TRUE, 'u', 10, NULL),
		       ('1/copy', $1, 'b2-eu-cen', $3, 'drive', NULL, TRUE, 'u', 10, 10),
		       ('1/expired', 5, 'b2-eu-cen', $3, 'drive', 'file_upload', TRUE, 'u', 10, 10)`,
		future, otherUserID, uploadLimitsUserID)
	require.NoError(t, err)
	single, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: 10, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Drive, "client", false)
	require.NoError(t, err)
	photos := startResumeTestUpload(t, c, ente.Photos, 12*mib, nil)
	locker := startResumeTestUpload(t, c, ente.Locker, 12*mib, nil)
	fake.uploadPart(t, photos.ObjectKey, 1, 5*mib)
	requestsBefore := len(fake.Requests())

	notFound := func(err error) { testutil.RequireAPIError(t, err, http.StatusNotFound, ente.NotFoundError) }
	gone := func(err error) { testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone) }
	notDrive := func(err error) { requireBadRequestMessage(t, err, "not a Drive multipart upload") }
	for _, tt := range []struct {
		key         string
		resumeCheck func(error)
		abortCheck  func(error)
	}{
		{key: "2/other", resumeCheck: notFound, abortCheck: notFound},
		{key: "1/foreign-row", resumeCheck: notFound, abortCheck: notFound},
		{key: "1/legacy", resumeCheck: notFound, abortCheck: notFound},
		{key: "1/missing", resumeCheck: gone, abortCheck: gone},
		{key: "1/expired", resumeCheck: gone, abortCheck: gone},
		{key: "1/copy", resumeCheck: notDrive, abortCheck: notDrive},
		{key: single.ObjectKey, resumeCheck: notDrive},
		{key: photos.ObjectKey, resumeCheck: notDrive, abortCheck: notDrive},
		{key: locker.ObjectKey, resumeCheck: notDrive, abortCheck: notDrive},
		{key: "1/v1", resumeCheck: func(err error) { requireBadRequestMessage(t, err, "upload is not resumable") }},
		{key: "1/pre-151", resumeCheck: func(err error) { requireBadRequestMessage(t, err, "upload is not resumable") }},
	} {
		var expiry int64
		_ = db.QueryRow(`SELECT expiration_time FROM temp_objects WHERE object_key = $1`, tt.key).Scan(&expiry)
		_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, tt.key)
		tt.resumeCheck(err)
		if tt.abortCheck != nil {
			tt.abortCheck(c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, tt.key))
		}
		var after int64
		_ = db.QueryRow(`SELECT expiration_time FROM temp_objects WHERE object_key = $1`, tt.key).Scan(&after)
		require.Equal(t, expiry, after, tt.key)
	}
	require.Equal(t, requestsBefore, len(fake.Requests()), "rejected requests reached storage")
	require.True(t, fake.hasUpload(photos.ObjectKey))
	require.False(t, resumePartsCompleted(t, db, photos.ObjectKey).Valid)

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, "1/pre-151"))
	require.LessOrEqual(t, tempObjectExpiry(t, db, "1/pre-151"), time.Microseconds())
}

func TestResumeAndAbortFailFastOnLockedRow(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	requireFast := func(call func(ctx context.Context) error, status int, code ente.ErrorCode) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*gotime.Second)
		defer cancel()
		start := gotime.Now()
		testutil.RequireAPIError(t, call(ctx), status, code)
		require.Less(t, gotime.Since(start), gotime.Second)
	}
	resume := func(ctx context.Context) error {
		_, err := c.ResumeMultipartUpload(ctx, uploadLimitsUserID, upload.ObjectKey)
		return err
	}
	abort := func(ctx context.Context) error {
		return c.AbortMultipartUpload(ctx, uploadLimitsUserID, upload.ObjectKey)
	}

	holder, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM temp_objects WHERE object_key = $1 FOR UPDATE`, upload.ObjectKey)
	require.NoError(t, err)
	requireFast(resume, http.StatusConflict, ente.UploadBusy)
	requireFast(abort, http.StatusConflict, ente.UploadBusy)
	require.NoError(t, holder.Rollback())
	require.True(t, fake.hasUpload(upload.ObjectKey))
	require.Greater(t, tempObjectExpiry(t, db, upload.ObjectKey), time.Microseconds())

	setTempObjectExpiry(t, db, upload.ObjectKey, 1)
	cronTx, locked, err := c.ObjectCleanupRepo.GetAndLockExpiredObjects()
	require.NoError(t, err)
	t.Cleanup(func() { _ = cronTx.Rollback() })
	require.Len(t, locked, 1)
	requireFast(resume, http.StatusGone, ente.UploadGone)
	requireFast(abort, http.StatusGone, ente.UploadGone)
	require.NoError(t, cronTx.Rollback())

	setTempObjectExpiry(t, db, upload.ObjectKey, time.MicrosecondsAfterDays(1))
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
}

func TestAbortMultipartUploadLeavesRowForCleanup(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	inProgress := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, inProgress.ObjectKey, 1, 5*mib)
	completed := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, completed.ObjectKey, 1, 12*mib)
	fake.complete(t, completed.ObjectKey)
	untouched := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)

	for _, key := range []string{inProgress.ObjectKey, completed.ObjectKey} {
		before := time.Microseconds()
		require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, key))
		require.False(t, fake.hasUpload(key))
		var expiry int64
		var released bool
		require.NoError(t, db.QueryRow(`SELECT expiration_time, reservation_released FROM temp_objects WHERE object_key = $1`, key).
			Scan(&expiry, &released))
		require.GreaterOrEqual(t, expiry, before)
		require.LessOrEqual(t, expiry, time.Microseconds())
		require.True(t, released)
		_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
		testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
		testutil.RequireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, key), http.StatusGone, ente.UploadGone)
	}
	require.True(t, fake.hasObject(completed.ObjectKey))

	require.Equal(t, 2, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasObject(completed.ObjectKey))
	var remaining []string
	rows, err := db.Query(`SELECT object_key FROM temp_objects`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		remaining = append(remaining, key)
	}
	require.Equal(t, []string{untouched.ObjectKey}, remaining)
	require.True(t, fake.hasUpload(untouched.ObjectKey))
}

func TestAbortMultipartUploadSucceedsWhenStorageAbortFails(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, upload.ObjectKey, 1, 5*mib)
	fake.failOn(fakes3.OpAbort)

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey))
	requireExpiredAndReleased(t, db, upload.ObjectKey)
	require.True(t, fake.hasUpload(upload.ObjectKey))
	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)

	fake.SetHook(nil)
	require.Equal(t, 1, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.False(t, fake.hasUpload(upload.ObjectKey))
	require.Empty(t, tempObjectKeys(t, db))
}

func TestReleasedUploadStaysGoneAfterCleanupRetryDelay(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, upload.ObjectKey, 1, 12*mib)
	fake.complete(t, upload.ObjectKey)
	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey))

	fake.failOn(fakes3.OpDelete, upload.ObjectKey)
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Greater(t, tempObjectExpiry(t, db, upload.ObjectKey), time.Microseconds())

	requestsBefore := len(fake.Requests())
	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
	testutil.RequireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey), http.StatusGone, ente.UploadGone)
	require.Equal(t, requestsBefore, len(fake.Requests()))
	require.True(t, fake.hasObject(upload.ObjectKey))
}

func TestCreateAfterAbortOfCompletedUploadKeepsObject(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	collectionID := insertUploadLimitsCollection(t, db, ente.Drive)
	upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, upload.ObjectKey, 1, 12*mib)
	fake.complete(t, upload.ObjectKey)
	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey))
	requireExpiredAndReleased(t, db, upload.ObjectKey)

	thumbKey := uploadLimitsKey("abort-thumb", 10)
	stageUploadLimitsObjects(t, db, thumbKey)
	fake.PutObject(thumbKey, 10)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
	file, err := c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, collectionID, upload.ObjectKey, thumbKey), "", ente.Drive, false)
	require.NoError(t, err)
	require.Equal(t, 12*mib, file.File.Size)

	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.True(t, fake.hasObject(upload.ObjectKey))
	require.True(t, fake.hasObject(thumbKey))
}

func TestNonDriveMultipartUploadsUnchanged(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	const checksum = "XUFAKrxLKna5cZ2REBfFkg=="
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		before := time.Microseconds()
		upload := startResumeTestUpload(t, c, app, 12*mib, []string{checksum, checksum, checksum})
		body, err := json.Marshal(upload)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &fields))
		require.ElementsMatch(t, []string{"objectKey", "partURLs", "completeURL"}, keysOf(fields))
		require.Len(t, upload.PartURLs, 3)
		for i, partURL := range upload.PartURLs {
			require.Contains(t, partURL, fmt.Sprintf("partNumber=%d", i+1))
			require.Contains(t, signedHeaders(t, partURL), "content-md5")
		}
		require.Contains(t, upload.CompleteURL, "uploadId=")
		var tempApp string
		var contentLength, partLength int64
		var partsCompleted sql.NullInt64
		var released bool
		require.NoError(t, db.QueryRow(`SELECT app, content_length, part_length, resume_parts_completed, reservation_released
			FROM temp_objects WHERE object_key = $1`, upload.ObjectKey).Scan(&tempApp, &contentLength, &partLength, &partsCompleted, &released))
		require.Equal(t, string(app), tempApp)
		require.Equal(t, 12*mib, contentLength)
		require.Equal(t, 5*mib, partLength)
		require.False(t, partsCompleted.Valid)
		require.False(t, released)
		validity := 2 * PreSignedPartUploadRequestDuration.Microseconds()
		require.GreaterOrEqual(t, tempObjectExpiry(t, db, upload.ObjectKey), before+validity)
		require.LessOrEqual(t, tempObjectExpiry(t, db, upload.ObjectKey), time.Microseconds()+validity)
	}

	legacy, err := c.GetMultipartUploadURLs(t.Context(), uploadLimitsUserID, 2, ente.Photos, "client")
	require.NoError(t, err)
	require.Len(t, legacy.PartURLs, 2)
	require.Contains(t, legacy.PartURLs[1], "partNumber=2")
	require.Contains(t, legacy.CompleteURL, "uploadId=")
	var partLength sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT part_length FROM temp_objects WHERE object_key = $1`, legacy.ObjectKey).Scan(&partLength))
	require.False(t, partLength.Valid)

	_, err = db.Exec(`UPDATE temp_objects SET expiration_time = 1`)
	require.NoError(t, err)
	require.Equal(t, 3, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Zero(t, fake.count(fakes3.OpList, ""))
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM temp_objects`).Scan(&remaining))
	require.Zero(t, remaining)
	require.False(t, fake.hasUpload(legacy.ObjectKey))
}

func TestResumeDoesNotHoldAPooledConnectionDuringStorageCalls(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	const resumes = 4
	keys := make([]string, 0, resumes)
	for range resumes {
		upload := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
		fake.uploadPart(t, upload.ObjectKey, 1, 5*mib)
		keys = append(keys, upload.ObjectKey)
	}
	pool, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(resumes)
	c.ObjectCleanupRepo = &repo.ObjectCleanupRepository{DB: pool}
	listing := make(chan struct{}, resumes)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		if r.Op == fakes3.OpListPart {
			listing <- struct{}{}
			gotime.Sleep(2 * gotime.Second)
		}
		return nil
	})

	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Go(func() {
			resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
			assert.NoError(t, err)
			assert.Len(t, resume.CompletedParts, 1)
		})
	}
	for range resumes {
		<-listing
	}
	start := gotime.Now()
	var one int
	require.NoError(t, pool.QueryRow(`SELECT 1`).Scan(&one))
	require.Less(t, gotime.Since(start), gotime.Second)
	// A Create of the same key doesn't wait for the resumes either.
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SET LOCAL lock_timeout = 500`)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM temp_objects WHERE object_key = $1`, keys[0])
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	wg.Wait()
	for _, key := range keys {
		require.Equal(t, int64(1), resumePartsCompleted(t, db, key).Int64)
	}
}

func TestResumeReportsGoneWhenTheRowChangesDuringStorageCalls(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	inProgress := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, inProgress.ObjectKey, 1, 5*mib)
	assembled := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, assembled.ObjectKey, 1, 12*mib)
	fake.complete(t, assembled.ObjectKey)
	idle := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	var mu sync.Mutex
	expired := make(map[string]bool)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		mu.Lock()
		defer mu.Unlock()
		if (r.Op == fakes3.OpListPart || r.Op == fakes3.OpHead) && !expired[r.Key] {
			// A concurrent abort or cron pass while the resume talks to storage.
			expired[r.Key] = true
			assert.NoError(t, c.ObjectCleanupRepo.ExpireTempObjectNow(context.Background(), r.Key, uploadLimitsUserID))
		}
		return nil
	})
	for _, key := range []string{inProgress.ObjectKey, assembled.ObjectKey, idle.ObjectKey} {
		_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, key)
		testutil.RequireAPIError(t, err, http.StatusGone, ente.UploadGone)
		requireExpiredAndReleased(t, db, key)
		require.False(t, resumePartsCompleted(t, db, key).Valid)
	}
}

// The cron holds expired rows locked across S3 calls; resume must not wait.
func TestResumeIsBusyWhenTheRowIsLockedDuringStorageCalls(t *testing.T) {
	c, db, fake := setupResumeTest(t)
	inProgress := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, inProgress.ObjectKey, 1, 5*mib)
	assembled := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, assembled.ObjectKey, 1, 12*mib)
	fake.complete(t, assembled.ObjectKey)
	truncated := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	fake.uploadPart(t, truncated.ObjectKey, 1, 5*mib)
	fake.complete(t, truncated.ObjectKey)
	holder, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	var mu sync.Mutex
	locked := make(map[string]bool)
	fake.SetHook(func(r fakes3.Request) *fakes3.Failure {
		mu.Lock()
		defer mu.Unlock()
		if (r.Op == fakes3.OpListPart || r.Op == fakes3.OpHead) && !locked[r.Key] {
			locked[r.Key] = true
			_, err := holder.Exec(`SELECT 1 FROM temp_objects WHERE object_key = $1 FOR UPDATE`, r.Key)
			assert.NoError(t, err)
		}
		return nil
	})
	for _, key := range []string{inProgress.ObjectKey, assembled.ObjectKey, truncated.ObjectKey} {
		expiry := tempObjectExpiry(t, db, key)
		ctx, cancel := context.WithTimeout(t.Context(), 5*gotime.Second)
		start := gotime.Now()
		_, err := c.ResumeMultipartUpload(ctx, uploadLimitsUserID, key)
		cancel()
		testutil.RequireAPIError(t, err, http.StatusConflict, ente.UploadBusy)
		require.Less(t, gotime.Since(start), 2*gotime.Second, key)
		require.Equal(t, expiry, tempObjectExpiry(t, db, key))
	}
}

func TestPendingDriveUploads(t *testing.T) {
	c, db, _ := setupResumeTest(t)
	otherUserID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "pending-other@ente.com", CreationTime: 1})
	before := time.Microseconds()
	multipart := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	single, err := c.GetUploadURLWithMetadata(t.Context(), uploadLimitsUserID,
		ente.UploadURLRequest{ContentLength: 10, ContentMD5: "XUFAKrxLKna5cZ2REBfFkg=="}, ente.Drive, "client", false)
	require.NoError(t, err)
	aborted := startResumeTestUpload(t, c, ente.Drive, 12*mib, nil)
	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, aborted.ObjectKey))
	startResumeTestUpload(t, c, ente.Photos, 12*mib, nil)
	_, err = startPublicDriveUpload(c, 12*mib)
	require.NoError(t, err)
	expiry := time.MicrosecondsAfterDays(1)
	_, err = db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, content_length, part_length)
		VALUES ('2/other', $1, 'b2-eu-cen', $2, 'drive', 'file_upload', 10, 10),
		       ('1/pending', $1, 'b2-eu-cen', $3, 'drive', 'file_upload', 10, 10),
		       ('1/expired', 5, 'b2-eu-cen', $3, 'drive', 'file_upload', 10, 10)`, expiry, otherUserID, uploadLimitsUserID)
	require.NoError(t, err)
	after := time.Microseconds()

	pending, err := c.GetPendingDriveUploads(t.Context(), uploadLimitsUserID, "")
	require.NoError(t, err)
	require.False(t, pending.HasMore)
	byKey := make(map[string]ente.PendingUpload)
	for _, upload := range pending.Uploads {
		require.GreaterOrEqual(t, upload.CreatedAt, before)
		require.LessOrEqual(t, upload.CreatedAt, after)
		require.Equal(t, tempObjectExpiry(t, db, upload.ObjectKey), upload.ExpiresAt)
		upload.CreatedAt, upload.ExpiresAt = 0, 0
		byKey[upload.ObjectKey] = upload
	}
	require.Equal(t, map[string]ente.PendingUpload{
		multipart.ObjectKey: {ObjectKey: multipart.ObjectKey, ContentLength: 12 * mib, IsMultipart: true},
		single.ObjectKey:    {ObjectKey: single.ObjectKey, ContentLength: 10},
		"1/pending":         {ObjectKey: "1/pending", ContentLength: 10, IsMultipart: true},
	}, byKey)

	previous := pendingUploadsPageSize
	pendingUploadsPageSize = 2
	t.Cleanup(func() { pendingUploadsPageSize = previous })
	var paged []string
	for after, hasMore := "", true; hasMore; {
		page, err := c.GetPendingDriveUploads(t.Context(), uploadLimitsUserID, after)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Uploads), 2)
		for _, upload := range page.Uploads {
			paged = append(paged, upload.ObjectKey)
		}
		hasMore = page.HasMore
		if hasMore {
			after = paged[len(paged)-1]
		}
	}
	require.Len(t, paged, 3)
	require.True(t, slices.IsSorted(paged))
	require.ElementsMatch(t, slices.Collect(maps.Keys(byKey)), paged)
}
