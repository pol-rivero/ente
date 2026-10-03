package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/controller/file_copy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOnlyAsyncDriveCopiesAreQueued(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &FileHandler{FileCopyCtrl: &file_copy.FileCopyController{CollectionCtrl: &collections.CollectionController{}}}
	router := gin.New()
	router.POST("/files/copy", h.CopyFiles)
	router.GET("/files/copy/:jobID", h.GetCopyJob)
	request := func(method, path, clientPackage, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-User-ID", "1")
		req.Header.Set("X-Client-Package", clientPackage)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}
	invalidItem := `{"srcCollectionID":1,"dstCollectionID":2,"files":[{"id":3,"encryptedKey":"key","keyDecryptionNonce":"nonce"}]}`

	for _, tt := range []struct {
		path, clientPackage string
		queued              bool
	}{
		{"/files/copy?async=true", "io.ente.photos", false},
		{"/files/copy?async=true", "io.ente.locker", false},
		{"/files/copy", "io.ente.drive", false},
		{"/files/copy?async=1", "io.ente.drive", false},
		{"/files/copy?async=true", "io.ente.drive", true},
	} {
		recorder := request(http.MethodPost, tt.path, tt.clientPackage, invalidItem)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, tt.queued, strings.Contains(recorder.Body.String(), "requestID"), "%s %s: %s", tt.clientPackage, tt.path, recorder.Body)
	}
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, "/files/copy/abc", "io.ente.drive", "").Code)
}
