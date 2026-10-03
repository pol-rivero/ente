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

func TestMoveCollectionIsRateLimitedPerUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rateLimiter := NewRateLimitMiddleware(discord.NewDiscordController(nil, "test", "test"), 1000, time.Minute)
	t.Cleanup(rateLimiter.Stop)
	router := gin.New()
	router.Use(rateLimiter.APIRateLimitForUserMiddleware(func(c *gin.Context) string { return c.FullPath() }))
	router.POST("/collections/move-collection", func(c *gin.Context) { c.Status(http.StatusOK) })
	request := func(userID string) int {
		req := httptest.NewRequest(http.MethodPost, "/collections/move-collection", nil)
		req.Header.Set("X-Auth-User-ID", userID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response.Code
	}

	for range 500 {
		require.Equal(t, http.StatusOK, request("1"))
	}
	require.Equal(t, http.StatusTooManyRequests, request("1"))
	require.Equal(t, http.StatusOK, request("2"))
}
