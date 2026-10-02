package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/gin-gonic/gin"
)

func TestGetAppMapsClientPackage(t *testing.T) {
	tests := []struct {
		clientPackage string
		want          ente.App
	}{
		{"", ente.Photos},
		{"io.ente.photos", ente.Photos},
		{"io.ente.photos.fdroid", ente.Photos},
		{"io.ente.auth", ente.Auth},
		{"io.ente.auth.web", ente.Auth},
		{"io.ente.locker", ente.Locker},
		{"io.ente.locker.web", ente.Locker},
		{"io.ente.drive", ente.Drive},
		{"io.ente.drive.web", ente.Drive},
		{"io.ente.space.web", ente.Photos},
		{"com.example.unknown", ente.Photos},
	}
	for _, tt := range tests {
		t.Run(tt.clientPackage, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("GET", "/", nil)
			ctx.Request.Header.Set("X-Client-Package", tt.clientPackage)
			if got := GetApp(ctx); got != tt.want {
				t.Fatalf("GetApp(%q) = %q, want %q", tt.clientPackage, got, tt.want)
			}
		})
	}
}
