package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ente/museum/pkg/controller/discord"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOnlyAsyncDriveCopiesAndJobPollingAreRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rateLimiter := NewRateLimitMiddleware(discord.NewDiscordController(nil, "test", "test"), 1000, time.Minute)
	t.Cleanup(rateLimiter.Stop)
	router := gin.New()
	router.Use(rateLimiter.APIRateLimitForUserMiddleware(func(c *gin.Context) string { return c.FullPath() }))
	router.POST("/files/copy", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/files/copy/:jobID", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := func(method, path, clientPackage string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Auth-User-ID", "1")
		req.Header.Set("X-Client-Package", clientPackage)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response.Code
	}

	for range 100 {
		require.Equal(t, http.StatusNoContent, request(http.MethodPost, "/files/copy?async=true", "io.ente.photos"))
		require.Equal(t, http.StatusNoContent, request(http.MethodPost, "/files/copy", "io.ente.drive"))
	}
	for range 60 {
		require.Equal(t, http.StatusNoContent, request(http.MethodPost, "/files/copy?async=true", "io.ente.drive"))
	}
	require.Equal(t, http.StatusTooManyRequests, request(http.MethodPost, "/files/copy?async=true", "io.ente.drive"))
	require.Equal(t, http.StatusNoContent, request(http.MethodPost, "/files/copy", "io.ente.photos"))
	for range 200 {
		require.Equal(t, http.StatusNoContent, request(http.MethodGet, "/files/copy/1", "io.ente.drive"))
	}
	require.Equal(t, http.StatusTooManyRequests, request(http.MethodGet, "/files/copy/1", "io.ente.drive"))
}
