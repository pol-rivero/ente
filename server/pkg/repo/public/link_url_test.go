package public

import (
	"testing"

	"github.com/ente/museum/ente"
	"github.com/spf13/viper"
)

func TestPublicLinkURLsUseAppHost(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("apps.public-albums", "https://albums.example")
	viper.Set("apps.public-locker", "https://locker.example")
	viper.Set("apps.public-drive", "https://drive.example")
	collectionLinks := NewCollectionLinkRepository(nil, "https://albums.example")
	fileLinks := NewFileLinkRepo(nil)

	for _, tt := range []struct {
		app            ente.App
		collectionLink string
		fileLink       string
	}{
		{ente.Photos, "https://albums.example/?t=TOKEN", "https://albums.example/file/?t=TOKEN"},
		{ente.Locker, "https://locker.example/c/TOKEN", "https://locker.example/TOKEN"},
		{ente.Drive, "https://drive.example/c/TOKEN", "https://drive.example/TOKEN"},
	} {
		if got := collectionLinks.GetAlbumUrl(tt.app, "TOKEN"); got != tt.collectionLink {
			t.Errorf("GetAlbumUrl(%s) = %q, want %q", tt.app, got, tt.collectionLink)
		}
		if got := fileLinks.FileLink(tt.app, "TOKEN"); got != tt.fileLink {
			t.Errorf("FileLink(%s) = %q, want %q", tt.app, got, tt.fileLink)
		}
	}
}

func TestPublicDriveLinkHostFallsBackToLocker(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("apps.public-locker", "https://locker.example")
	if got := NewCollectionLinkRepository(nil, "").GetAlbumUrl(ente.Drive, "TOKEN"); got != "https://locker.example/c/TOKEN" {
		t.Fatalf("Drive collection link without public-drive = %q", got)
	}
	if got := NewFileLinkRepo(nil).FileLink(ente.Drive, "TOKEN"); got != "https://locker.example/TOKEN" {
		t.Fatalf("Drive file link without public-drive = %q", got)
	}

	viper.Reset()
	if got := NewCollectionLinkRepository(nil, "").GetAlbumUrl(ente.Drive, "TOKEN"); got != "https://share.ente.com/c/TOKEN" {
		t.Fatalf("Drive collection link without config = %q", got)
	}
	if got := NewFileLinkRepo(nil).FileLink(ente.Drive, "TOKEN"); got != "https://share.ente.com/TOKEN" {
		t.Fatalf("Drive file link without config = %q", got)
	}
}
