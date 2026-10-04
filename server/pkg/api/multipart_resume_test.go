package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/config"
	"github.com/ente/museum/pkg/utils/s3config"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestMultipartResumeAndAbortRoutes(t *testing.T) {
	var aborted atomic.Bool
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inProgress := r.URL.Query().Get("uploadId") == "in-progress" && !aborted.Load()
		switch {
		case r.Method == http.MethodGet && inProgress:
			_, _ = w.Write([]byte(`<ListPartsResult><IsTruncated>false</IsTruncated>` +
				`<Part><PartNumber>1</PartNumber><ETag>"etag-1"</ETag><Size>6</Size></Part></ListPartsResult>`))
		case r.Method == http.MethodDelete && inProgress:
			aborted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "12")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchUpload</Code><Message>missing</Message></Error>`))
		}
	}))
	t.Cleanup(s3Server.Close)
	testutil.WithServerRoot(t)
	viper.Reset()
	t.Cleanup(viper.Reset)
	require.NoError(t, config.ConfigureViper("local"))
	viper.Set("s3.b2-eu-cen.key", "test-key")
	viper.Set("s3.b2-eu-cen.secret", "test-secret")
	viper.Set("s3.b2-eu-cen.endpoint", s3Server.URL)
	viper.Set("s3.b2-eu-cen.region", "us-east-1")
	viper.Set("s3.b2-eu-cen.bucket", "test-bucket")
	viper.Set("s3.b2-eu-cen.disable_ssl", true)
	viper.Set("s3.use_path_style_urls", true)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	userID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "resume-routes@ente.com", CreationTime: 1})
	_, err := db.Exec(`INSERT INTO temp_objects(object_key, expiration_time, bucket_id, user_id, app, purpose, is_multipart, upload_id, content_length, part_length)
		VALUES ('1/in-progress', $1, 'b2-eu-cen', $2, 'drive', 'file_upload', TRUE, 'in-progress', 12, 6),
		       ('1/assembled', $1, 'b2-eu-cen', $2, 'drive', 'file_upload', TRUE, 'assembled', 12, 6)`,
		time.MicrosecondsAfterDays(14), userID)
	require.NoError(t, err)

	s3Config := s3config.NewS3Config()
	cleanupRepo := &repo.ObjectCleanupRepository{DB: db}
	h := &FileHandler{Controller: &controller.FileController{
		S3Config:          s3Config,
		ObjectCleanupRepo: cleanupRepo,
		ObjectCleanupCtrl: controller.NewObjectCleanupController(cleanupRepo, &repo.ObjectRepository{DB: db}, s3Config),
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/files/multipart-upload-url/resume", h.ResumeMultipartUpload)
	router.DELETE("/files/multipart-upload", h.AbortMultipartUpload)
	router.GET("/files/uploads", h.GetPendingUploads)
	serve := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-User-ID", "1")
		req.Header.Set("X-Client-Package", "io.ente.drive")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}

	resume := serve(http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":"1/in-progress"}`)
	require.Equal(t, http.StatusOK, resume.Code, resume.Body.String())
	require.Contains(t, resume.Body.String(), `"completedParts":[{"partNumber":1,"eTag":"\"etag-1\"","size":6}]`)
	require.Contains(t, resume.Body.String(), `"partURLs":{"2":"http`)
	require.Contains(t, resume.Body.String(), `"completeURL":"http`)
	require.NotContains(t, resume.Body.String(), `"completed"`)

	assembled := serve(http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":"1/assembled"}`)
	require.Equal(t, http.StatusOK, assembled.Code, assembled.Body.String())
	require.JSONEq(t, `{"completed":true}`, assembled.Body.String())

	for _, tt := range []struct {
		method, target, body string
		wantCode             int
		wantBody             string
	}{
		{http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":"2/other"}`, http.StatusNotFound, `{"code":"NOT_FOUND","message":""}`},
		{http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":"1/missing"}`, http.StatusGone, `{"code":"UPLOAD_GONE","message":"The upload no longer exists"}`},
		{http.MethodPost, "/files/multipart-upload-url/resume", `{}`, http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"objectKey is required"}`},
		{http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":""}`, http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"objectKey is required"}`},
		{http.MethodDelete, "/files/multipart-upload", "", http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"objectKey is required"}`},
		{http.MethodDelete, "/files/multipart-upload?objectKey=2/other", "", http.StatusNotFound, `{"code":"NOT_FOUND","message":""}`},
	} {
		recorder := serve(tt.method, tt.target, tt.body)
		require.Equal(t, tt.wantCode, recorder.Code, "%+v", tt)
		require.JSONEq(t, tt.wantBody, recorder.Body.String(), "%+v", tt)
	}

	listed := serve(http.MethodGet, "/files/uploads", "")
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	var pending struct {
		Uploads []map[string]any `json:"uploads"`
		HasMore *bool            `json:"hasMore"`
	}
	require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &pending))
	require.NotNil(t, pending.HasMore)
	require.False(t, *pending.HasMore)
	require.Len(t, pending.Uploads, 2)
	for i, key := range []string{"1/assembled", "1/in-progress"} {
		upload := pending.Uploads[i]
		require.ElementsMatch(t, []string{"objectKey", "contentLength", "isMultipart", "createdAt", "expiresAt"}, slices.Collect(maps.Keys(upload)))
		require.Equal(t, key, upload["objectKey"])
		require.EqualValues(t, 12, upload["contentLength"])
		require.Equal(t, true, upload["isMultipart"])
		var expiresAt int64
		require.NoError(t, db.QueryRow(`SELECT expiration_time FROM temp_objects WHERE object_key = $1`, key).Scan(&expiresAt))
		require.EqualValues(t, expiresAt, upload["expiresAt"])
		require.Positive(t, upload["createdAt"])
	}
	listed = serve(http.MethodGet, "/files/uploads?after=1/assembled", "")
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	require.Contains(t, listed.Body.String(), `"objectKey":"1/in-progress"`)
	require.NotContains(t, listed.Body.String(), `"objectKey":"1/assembled"`)

	abort := serve(http.MethodDelete, "/files/multipart-upload?objectKey=1/in-progress", "")
	require.Equal(t, http.StatusOK, abort.Code, abort.Body.String())
	require.True(t, aborted.Load())
	gone := serve(http.MethodPost, "/files/multipart-upload-url/resume", `{"objectKey":"1/in-progress"}`)
	require.Equal(t, http.StatusGone, gone.Code)
	listed = serve(http.MethodGet, "/files/uploads", "")
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	require.NotContains(t, listed.Body.String(), `"objectKey":"1/in-progress"`)
}
