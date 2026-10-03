package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	gotime "time"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const mib = int64(1) << 20

type fakeMultipartS3 struct {
	t               *testing.T
	mu              sync.Mutex
	nextID          int
	uploads         map[string]*fakeMultipartUpload
	objects         map[string]int64
	pageSize        int
	omitNextMarker  bool
	alwaysTruncated bool
	listCalls       int
	requests        int
	headMisses      map[string]int
	headCalls       map[string]int
	failAborts      bool
	failDeletes     map[string]bool
}

type fakeMultipartUpload struct {
	key   string
	parts map[int64]int64
}

func newFakeMultipartS3(t *testing.T) (*fakeMultipartS3, string) {
	t.Helper()
	fake := &fakeMultipartS3{
		t: t, uploads: map[string]*fakeMultipartUpload{}, objects: map[string]int64{}, pageSize: 1000,
		headMisses: map[string]int{}, headCalls: map[string]int{}, failDeletes: map[string]bool{},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *fakeMultipartS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	key, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/test-bucket/"))
	query := r.URL.Query()
	uploadID := query.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		f.nextID++
		uploadID = fmt.Sprintf("upload-%d", f.nextID)
		f.uploads[uploadID] = &fakeMultipartUpload{key: key, parts: map[int64]int64{}}
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, uploadID)
	case uploadID != "":
		upload, ok := f.uploads[uploadID]
		if !ok || upload.key != key {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code><Message>The specified upload does not exist.</Message></Error>`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.listCalls++
			marker, _ := strconv.ParseInt(query.Get("part-number-marker"), 10, 64)
			f.writeParts(w, upload, uploadID, marker)
		case http.MethodDelete:
			if f.failAborts {
				writeAccessDenied(w)
				return
			}
			delete(f.uploads, uploadID)
			w.WriteHeader(http.StatusNoContent)
		default:
			f.t.Errorf("unexpected storage request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	case r.Method == http.MethodHead:
		f.headCalls[key]++
		size, ok := f.objects[key]
		if f.headMisses[key] > 0 {
			f.headMisses[key]--
			ok = false
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		if f.failDeletes[key] {
			writeAccessDenied(w)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected storage request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func writeAccessDenied(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))
}

func (f *fakeMultipartS3) set(update func(f *fakeMultipartS3)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

func (f *fakeMultipartS3) writeParts(w http.ResponseWriter, upload *fakeMultipartUpload, uploadID string, marker int64) {
	numbers := make([]int64, 0, len(upload.parts))
	for number := range upload.parts {
		if number > marker {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	truncated := len(numbers) > f.pageSize || f.alwaysTruncated
	if len(numbers) > f.pageSize {
		numbers = numbers[:f.pageSize]
	}
	var body strings.Builder
	fmt.Fprintf(&body, `<ListPartsResult><Bucket>test-bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId><IsTruncated>%t</IsTruncated>`,
		upload.key, uploadID, truncated)
	if truncated && !f.omitNextMarker && len(numbers) > 0 {
		fmt.Fprintf(&body, `<NextPartNumberMarker>%d</NextPartNumberMarker>`, numbers[len(numbers)-1])
	}
	for _, number := range numbers {
		fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>"etag-%d"</ETag><Size>%d</Size></Part>`, number, number, upload.parts[number])
	}
	body.WriteString(`</ListPartsResult>`)
	_, _ = w.Write([]byte(body.String()))
}

func (f *fakeMultipartS3) upload(key string) *fakeMultipartUpload {
	for _, upload := range f.uploads {
		if upload.key == key {
			return upload
		}
	}
	return nil
}

func (f *fakeMultipartS3) uploadPart(t *testing.T, key string, number int64, size int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	upload := f.upload(key)
	require.NotNil(t, upload, key)
	upload.parts[number] = size
}

func (f *fakeMultipartS3) complete(t *testing.T, key string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, upload := range f.uploads {
		if upload.key == key {
			var size int64
			for _, partSize := range upload.parts {
				size += partSize
			}
			f.objects[key] = size
			delete(f.uploads, id)
			return
		}
	}
	t.Fatalf("no upload for %s", key)
}

func (f *fakeMultipartS3) putObject(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = size
}

func (f *fakeMultipartS3) dropUpload(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, upload := range f.uploads {
		if upload.key == key {
			delete(f.uploads, id)
		}
	}
}

func (f *fakeMultipartS3) hasUpload(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upload(key) != nil
}

func (f *fakeMultipartS3) hasObject(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeMultipartS3) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func setupResumeTest(t *testing.T) (*FileController, *sql.DB, *fakeMultipartS3) {
	t.Helper()
	delays := headNotFoundRetryDelays
	headNotFoundRetryDelays = []gotime.Duration{gotime.Millisecond, gotime.Millisecond}
	t.Cleanup(func() { headNotFoundRetryDelays = delays })
	fake, s3URL := newFakeMultipartS3(t)
	c, db := newUploadTestController(t, s3URL, 100*gib)
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

func requireAPIError(t *testing.T, err error, status int, code ente.ErrorCode) {
	t.Helper()
	var apiErr *ente.ApiError
	require.ErrorAs(t, err, &apiErr, "%v", err)
	require.Equal(t, status, apiErr.HttpStatusCode, "%v", err)
	require.Equal(t, code, apiErr.Code, "%v", err)
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

	shortExpiry := time.MicrosecondsAfterHours(1)
	setTempObjectExpiry(t, db, upload.ObjectKey, shortExpiry)
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, shortExpiry, tempObjectExpiry(t, db, upload.ObjectKey))

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
	fake.pageSize = 2

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, 2, fake.listCalls)
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
	fake.set(func(f *fakeMultipartS3) { f.pageSize = 2; f.omitNextMarker = true })

	resume, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.NoError(t, err)
	require.Len(t, resume.CompletedParts, 4)
	require.Equal(t, []int64{4}, partNumbers(resume.PartURLs))

	fake.set(func(f *fakeMultipartS3) { f.alwaysTruncated = true; f.listCalls = 0 })
	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	require.ErrorContains(t, err, "did not advance")
	require.Equal(t, 3, fake.listCalls)
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
	fake.set(func(f *fakeMultipartS3) { f.headMisses[upload.ObjectKey] = len(headNotFoundRetryDelays) })

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
	fake.putObject(thumbKey, 10)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files", nil)
	file, err := c.Create(ctx, uploadLimitsUserID, objectCleanupTestFile(uploadLimitsUserID, collectionID, upload.ObjectKey, thumbKey), "", ente.Drive, false)
	require.NoError(t, err)
	require.Equal(t, 12*mib, file.File.Size)

	_, err = c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	requireAPIError(t, err, http.StatusGone, ente.UploadGone)
	requireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey), http.StatusGone, ente.UploadGone)
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
		requireAPIError(t, err, http.StatusGone, ente.UploadGone)
		requireExpiredAndReleased(t, db, key)
	}
	require.Equal(t, len(headNotFoundRetryDelays)+1, fake.headCalls[aborted.ObjectKey])
	require.Equal(t, 1, fake.headCalls[truncated.ObjectKey])

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
	requestsBefore := fake.requestCount()

	notFound := func(err error) { requireAPIError(t, err, http.StatusNotFound, ente.NotFoundError) }
	gone := func(err error) { requireAPIError(t, err, http.StatusGone, ente.UploadGone) }
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
		{key: single.ObjectKey, resumeCheck: notDrive, abortCheck: notDrive},
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
	require.Equal(t, requestsBefore, fake.requestCount(), "rejected requests reached storage")
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
		requireAPIError(t, call(ctx), status, code)
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
		requireAPIError(t, err, http.StatusGone, ente.UploadGone)
		requireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, key), http.StatusGone, ente.UploadGone)
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
	fake.set(func(f *fakeMultipartS3) { f.failAborts = true })

	require.NoError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey))
	requireExpiredAndReleased(t, db, upload.ObjectKey)
	require.True(t, fake.hasUpload(upload.ObjectKey))
	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	requireAPIError(t, err, http.StatusGone, ente.UploadGone)

	fake.set(func(f *fakeMultipartS3) { f.failAborts = false })
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

	fake.set(func(f *fakeMultipartS3) { f.failDeletes[upload.ObjectKey] = true })
	require.Zero(t, c.ObjectCleanupCtrl.removeUnreportedObjects())
	require.Greater(t, tempObjectExpiry(t, db, upload.ObjectKey), time.Microseconds())

	requestsBefore := fake.requestCount()
	_, err := c.ResumeMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey)
	requireAPIError(t, err, http.StatusGone, ente.UploadGone)
	requireAPIError(t, c.AbortMultipartUpload(t.Context(), uploadLimitsUserID, upload.ObjectKey), http.StatusGone, ente.UploadGone)
	require.Equal(t, requestsBefore, fake.requestCount())
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
	fake.putObject(thumbKey, 10)
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
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM temp_objects`).Scan(&remaining))
	require.Zero(t, remaining)
	require.False(t, fake.hasUpload(legacy.ObjectKey))
}
