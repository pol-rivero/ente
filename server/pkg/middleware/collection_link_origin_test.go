package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo/remotestore"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

func TestValidateOriginAllowsConfiguredPublicAppHosts(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("apps.public-albums", "https://albums.example")
	viper.Set("apps.public-locker", "https://locker.example")
	viper.Set("apps.public-drive", "https://drive.example")

	for _, origin := range []string{"", "https://albums.example", "https://locker.example", "https://drive.example"} {
		if err := (&CollectionLinkMiddleware{}).validateOrigin(newOriginTestContext(origin), 1); err != nil {
			t.Fatalf("validateOrigin(%q) error = %v", origin, err)
		}
	}
}

func TestValidateOriginDoesNotTrustUnsetDriveHost(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("apps.public-locker", "https://locker.example")

	middleware := &CollectionLinkMiddleware{RemoteStoreRepo: &remotestore.Repository{DB: db}}
	if err := middleware.validateOrigin(newOriginTestContext("https://locker.example"), 1); err != nil {
		t.Fatalf("validateOrigin(locker) error = %v", err)
	}
	if err := middleware.validateOrigin(newOriginTestContext("https://share.ente.com"), 1); err == nil {
		t.Fatal("validateOrigin accepted https://share.ente.com without public-drive configured")
	}
}

func newOriginTestContext(origin string) *gin.Context {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/public-collection/info", nil)
	if origin != "" {
		ctx.Request.Header.Set("Origin", origin)
	}
	return ctx
}
