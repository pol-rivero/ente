package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/internal/testutil"
	"github.com/ente/museum/pkg/repo/remotestore"
	"github.com/ente/museum/pkg/utils/handler"
	"github.com/ente/stacktrace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const gib = int64(1 << 30)

func TestDriveFileSizeConstants(t *testing.T) {
	require.LessOrEqual(t, DriveMaxFileSize, 1000*ente.MaxMultipartPartSize)
	require.LessOrEqual(t, DrivePublicMaxFileSize, DriveMaxFileSize)
	require.Equal(t, int64(10737418240), MaxFileSize)
	require.Equal(t, int64(21474836480), InternalUserMaxFileSize)
}

func TestIsFileSizeAllowedWithoutInternalUserLookup(t *testing.T) {
	// No RemoteStoreRepo: none of these cases may look up the internal flag.
	c := &FileController{}
	for _, tt := range []struct {
		app         ente.App
		public      bool
		size        int64
		wantAllowed bool
		wantLimit   int64
	}{
		{app: ente.Photos, size: MaxFileSize, wantAllowed: true, wantLimit: MaxFileSize},
		{app: ente.Photos, size: InternalUserMaxFileSize + 1, wantLimit: MaxFileSize},
		{app: ente.Photos, public: true, size: MaxFileSize, wantAllowed: true, wantLimit: MaxFileSize},
		{app: ente.Photos, public: true, size: InternalUserMaxFileSize + 1, wantLimit: MaxFileSize},
		{app: ente.Locker, size: MaxFileSize, wantAllowed: true, wantLimit: MaxFileSize},
		{app: ente.Locker, size: MaxFileSize + 1, wantLimit: MaxFileSize},
		{app: ente.Locker, public: true, size: MaxFileSize + 1, wantLimit: MaxFileSize},
		{app: ente.Auth, size: MaxFileSize + 1, wantLimit: MaxFileSize},
		{app: ente.Drive, size: DriveMaxFileSize, wantAllowed: true, wantLimit: DriveMaxFileSize},
		{app: ente.Drive, size: DriveMaxFileSize + 1, wantLimit: DriveMaxFileSize},
		{app: ente.Drive, size: InternalUserMaxFileSize, wantLimit: DriveMaxFileSize},
		{app: ente.Drive, public: true, size: DrivePublicMaxFileSize, wantAllowed: true, wantLimit: DrivePublicMaxFileSize},
		{app: ente.Drive, public: true, size: DrivePublicMaxFileSize + 1, wantLimit: DrivePublicMaxFileSize},
	} {
		allowed, limit, err := c.isFileSizeAllowed(t.Context(), 1, tt.size, tt.app, tt.public)
		require.NoError(t, err)
		require.Equal(t, tt.wantAllowed, allowed, "%+v", tt)
		require.Equal(t, tt.wantLimit, limit, "%+v", tt)
	}
}

func TestIsFileSizeAllowedForInternalUsers(t *testing.T) {
	testutil.WithServerRoot(t)
	db := testutil.RequireTestDB(t)
	testutil.ResetTables(t, db)
	t.Cleanup(func() { testutil.ResetTables(t, db) })
	internalID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 1, Email: "internal@ente.com", CreationTime: 1})
	regularID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 2, Email: "regular@ente.com", CreationTime: 1})
	disabledID := testutil.InsertUser(t, db, testutil.UserFixture{UserID: 3, Email: "disabled@ente.com", CreationTime: 1})
	store := &remotestore.Repository{DB: db}
	require.NoError(t, store.InsertOrUpdate(t.Context(), internalID, string(ente.IsInternalUser), "true"))
	require.NoError(t, store.InsertOrUpdate(t.Context(), disabledID, string(ente.IsInternalUser), "false"))
	c := &FileController{RemoteStoreRepo: store}

	for _, tt := range []struct {
		userID      int64
		app         ente.App
		public      bool
		size        int64
		wantAllowed bool
	}{
		{userID: internalID, app: ente.Photos, size: 15 * gib, wantAllowed: true},
		{userID: internalID, app: ente.Photos, size: InternalUserMaxFileSize, wantAllowed: true},
		{userID: internalID, app: ente.Photos, size: InternalUserMaxFileSize + 1},
		{userID: internalID, app: ente.Photos, public: true, size: 15 * gib, wantAllowed: true},
		{userID: internalID, app: ente.Locker, size: 15 * gib},
		{userID: internalID, app: ente.Drive, size: 15 * gib},
		{userID: internalID, app: ente.Drive, public: true, size: 15 * gib},
		{userID: regularID, app: ente.Photos, size: 15 * gib},
		{userID: disabledID, app: ente.Photos, size: 15 * gib},
	} {
		allowed, limit, err := c.isFileSizeAllowed(t.Context(), tt.userID, tt.size, tt.app, tt.public)
		require.NoError(t, err)
		require.Equal(t, tt.wantAllowed, allowed, "%+v", tt)
		if tt.app != ente.Drive {
			require.Equal(t, MaxFileSize, limit, "%+v", tt)
		}
	}
}

func renderError(t *testing.T, err error) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/files/upload-url", nil)
	handler.Error(ctx, stacktrace.Propagate(err, ""))
	return recorder.Code, recorder.Body.String()
}

func TestContentLengthTooLargeErrorResponses(t *testing.T) {
	for _, app := range []ente.App{ente.Photos, ente.Locker, ente.Auth} {
		err := contentLengthTooLargeError(app, MaxFileSize)
		require.Contains(t, err.Error(), "contentLength exceeds max file size 10737418240")
		code, body := renderError(t, err)
		require.Equal(t, http.StatusBadRequest, code)
		require.JSONEq(t, `{}`, body)
	}
	code, body := renderError(t, contentLengthTooLargeError(ente.Drive, DrivePublicMaxFileSize))
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body, `"message":"contentLength exceeds max file size 10737418240"`)
}

// The too-many-parts error can't be reached under the current caps (10 GiB
// at the 5 MiB minimum part length is 2 048 parts), so test it directly.
func TestTooManyPartsErrorResponses(t *testing.T) {
	for _, app := range []ente.App{ente.Photos, ente.Locker} {
		err := tooManyPartsError(app, 100*gib)
		require.ErrorIs(t, err, ente.ErrBadRequest)
		require.Contains(t, err.Error(), "multipart upload cannot exceed 10000 parts")
		require.NotContains(t, err.Error(), "partLength must be at least")
		code, body := renderError(t, err)
		require.Equal(t, http.StatusBadRequest, code)
		require.JSONEq(t, `{}`, body)
	}
	for _, tt := range []struct {
		contentLength int64
		want          string
	}{
		// ceil(100 GiB / 10 000)
		{contentLength: 100 * gib, want: "partLength must be at least 10737419"},
		{contentLength: 5000 * gib, want: "partLength must be at least 536870912"},
		{contentLength: 10000*ente.MinMultipartPartSize + 1, want: "partLength must be at least 5242881"},
	} {
		code, body := renderError(t, tooManyPartsError(ente.Drive, tt.contentLength))
		require.Equal(t, http.StatusBadRequest, code)
		require.Contains(t, body, `"message":"multipart upload cannot exceed 10000 parts, `+tt.want+`"`)
	}
}
