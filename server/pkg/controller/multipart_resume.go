package controller

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	gTime "time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	log "github.com/sirupsen/logrus"
)

const maxResumableUploadAge = 90 * 24 * gTime.Hour

const completedUploadMinValidity = 7 * 24 * gTime.Hour

// The S3 client has no timeout.
const multipartUploadStorageTimeout = 60 * gTime.Second

// Resumed URLs expire before the row, so a part PUT started just before they
// expire can finish before the cron aborts the upload.
const (
	resumeURLExpiryMargin = gTime.Hour
	minResumeURLValidity  = 5 * gTime.Minute
)

// A gone verdict lets the cron delete the object, so don't trust a 404 HEAD
// right after a slow CompleteMultipartUpload.
var headNotFoundRetryDelays = []gTime.Duration{500 * gTime.Millisecond, 1500 * gTime.Millisecond}

// No transaction is held across S3 calls: the row is re-checked by the
// conditional updates instead.
func (c *FileController) ResumeMultipartUpload(ctx context.Context, userID int64, objectKey string) (ente.MultipartUploadResume, error) {
	if err := requireOwnObjectKey(userID, objectKey); err != nil {
		return ente.MultipartUploadResume{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, multipartUploadStorageTimeout)
	defer cancel()
	upload, err := c.readOwnedDriveUpload(ctx, userID, objectKey, false)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if !upload.IsMultipart {
		if upload.PartLength != nil {
			// A multipart start that hasn't stored its upload ID yet.
			return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadBusy, "")
		}
		return ente.MultipartUploadResume{}, stacktrace.Propagate(notDriveMultipartUpload(), "")
	}
	if upload.ContentLength == nil || upload.PartLength == nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.NewBadRequestWithMessage("upload is not resumable"), "")
	}
	dc := upload.BucketId
	parts, err := c.listUploadedParts(ctx, dc, objectKey, upload.UploadID)
	if err != nil && isUnknownUploadError(err) {
		return c.resumeAssembledUpload(ctx, upload)
	}
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}

	partCount := calculateMultipartPartCount(*upload.ContentLength, *upload.PartLength)
	lengths := computePartLengths(*upload.ContentLength, *upload.PartLength, partCount)
	resume := ente.MultipartUploadResume{CompletedParts: make([]ente.MultipartUploadPart, 0, len(parts)), PartURLs: make(map[int64]string)}
	uploaded := make(map[int64]bool, len(parts))
	for _, part := range parts {
		if part.PartNumber >= 1 && part.PartNumber <= int64(len(lengths)) && part.Size == lengths[part.PartNumber-1] {
			resume.CompletedParts = append(resume.CompletedParts, part)
			uploaded[part.PartNumber] = true
		}
	}
	expiry := upload.ExpirationTime
	now := time.Microseconds()
	progressed := false
	if completed := int64(len(resume.CompletedParts)); completed > upload.ResumePartsCompleted {
		var found bool
		expiry, found, err = c.ObjectCleanupRepo.ExtendLiveUploadExpiry(ctx, objectKey, upload.UploadID, now,
			now+2*PreSignedPartUploadRequestDuration.Microseconds(), maxResumableUploadAge.Microseconds(), &completed)
		if err != nil {
			return ente.MultipartUploadResume{}, uploadBusyIfLocked(err)
		}
		if !found {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "")
		}
		progressed = true
	}
	validity := resumeURLValidity(expiry, now)
	if validity <= 0 {
		// The row expires too soon for URLs that end well before it.
		if err := c.ObjectCleanupRepo.ExpireLiveUpload(ctx, objectKey, upload.UploadID, upload.UserID, time.Microseconds()); err != nil {
			return ente.MultipartUploadResume{}, uploadBusyIfLocked(err)
		}
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "")
	}
	for i, length := range lengths {
		partNumber := int64(i + 1)
		if uploaded[partNumber] {
			continue
		}
		url, err := c.getPartURL(dc, objectKey, partNumber, &upload.UploadID, &length, nil, validity)
		if err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
		resume.PartURLs[partNumber] = url
	}
	resume.CompleteURL, err = c.getCompleteURL(dc, objectKey, &upload.UploadID, validity)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if !progressed {
		// The expiry was read before ListParts; an abort, commit or the cron
		// may have released the row since.
		live, err := c.ObjectCleanupRepo.IsLiveUpload(ctx, objectKey, upload.UploadID, time.Microseconds())
		if err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
		if !live {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "")
		}
	}
	return resume, nil
}

// Expiry only moves on progress, so URLs from a resume without progress may
// be valid for less than the usual 7 days. Zero if the row expires too soon.
func resumeURLValidity(expiry int64, now int64) gTime.Duration {
	remaining := gTime.Duration(expiry-now) * gTime.Microsecond
	validity := min(PreSignedPartUploadRequestDuration, remaining-resumeURLExpiryMargin)
	if validity < minResumeURLValidity {
		return 0
	}
	return validity
}

func uploadBusyIfLocked(err error) error {
	if errors.Is(err, repo.ErrTempObjectLocked) {
		return stacktrace.Propagate(ente.ErrUploadBusy, "")
	}
	return stacktrace.Propagate(err, "")
}

// The client may have completed the upload and lost the response.
func (c *FileController) resumeAssembledUpload(ctx context.Context, upload repo.LockedTempObject) (ente.MultipartUploadResume, error) {
	size, found, err := c.headObject(ctx, upload.BucketId, upload.ObjectKey)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if !found || size != *upload.ContentLength {
		// Neither can ever be committed: expire now so the cron deletes any
		// stray object and the reservation is released.
		if err := c.ObjectCleanupRepo.ExpireLiveUpload(ctx, upload.ObjectKey, upload.UploadID, upload.UserID, time.Microseconds()); err != nil {
			return ente.MultipartUploadResume{}, uploadBusyIfLocked(err)
		}
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "found=%t size=%d", found, size)
	}
	now := time.Microseconds()
	_, found, err = c.ObjectCleanupRepo.ExtendLiveUploadExpiry(ctx, upload.ObjectKey, upload.UploadID, now,
		now+completedUploadMinValidity.Microseconds(), maxResumableUploadAge.Microseconds(), nil)
	if err != nil {
		return ente.MultipartUploadResume{}, uploadBusyIfLocked(err)
	}
	if !found {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "")
	}
	return ente.MultipartUploadResume{Completed: true}, nil
}

// The row is kept: after a completed upload it is the only record of the
// object, and the cleanup cron deletes it only if it was never committed. For
// single PUTs and multipart starts without an upload ID yet, the cron also
// deletes or aborts whatever reached storage.
func (c *FileController) AbortMultipartUpload(ctx context.Context, userID int64, objectKey string) error {
	if err := requireOwnObjectKey(userID, objectKey); err != nil {
		return err
	}
	upload, err := c.readOwnedDriveUpload(ctx, userID, objectKey, true)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if !upload.IsMultipart {
		return nil
	}
	// Best effort: the row is already expired, so the cleanup cron aborts the
	// upload if this fails or the client disconnects.
	ctx, cancel := context.WithTimeout(ctx, multipartUploadStorageTimeout)
	defer cancel()
	if err := c.ObjectCleanupCtrl.AbortMultipartUploadWithContext(ctx, objectKey, upload.UploadID, upload.BucketId); err != nil {
		log.WithError(err).WithField("object_key", objectKey).Warn("Failed to abort multipart upload, leaving it to the cleanup cron")
	}
	return nil
}

// The row lock only detects a concurrent resume, abort or Create (409); it is
// released before any S3 call.
func (c *FileController) readOwnedDriveUpload(ctx context.Context, userID int64, objectKey string, expire bool) (repo.LockedTempObject, error) {
	tx, err := c.ObjectCleanupRepo.DB.BeginTx(ctx, nil)
	if err != nil {
		return repo.LockedTempObject{}, stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	now := time.Microseconds()
	upload, err := c.lockOwnedDriveUpload(ctx, tx, userID, objectKey, now)
	if err != nil {
		return upload, stacktrace.Propagate(err, "")
	}
	if expire {
		if err := c.ObjectCleanupRepo.ExpireLockedTempObject(ctx, tx, objectKey, userID, now); err != nil {
			return upload, stacktrace.Propagate(err, "")
		}
	}
	return upload, stacktrace.Propagate(tx.Commit(), "")
}

var pendingUploadsPageSize = 1000

func (c *FileController) GetPendingDriveUploads(ctx context.Context, userID int64, afterKey string) (ente.PendingUploads, error) {
	uploads, err := c.ObjectCleanupRepo.GetLiveDriveReservations(ctx, userID, time.Microseconds(), afterKey, pendingUploadsPageSize+1)
	if err != nil {
		return ente.PendingUploads{}, stacktrace.Propagate(err, "")
	}
	hasMore := len(uploads) > pendingUploadsPageSize
	if hasMore {
		uploads = uploads[:pendingUploadsPageSize]
	}
	return ente.PendingUploads{Uploads: uploads, HasMore: hasMore}, nil
}

// Never lock rows outside the caller's key prefix: that could make the owner's
// own resume or Create wait or fail.
func requireOwnObjectKey(userID int64, objectKey string) error {
	if !strings.HasPrefix(objectKey, strconv.FormatInt(userID, 10)+"/") {
		return stacktrace.Propagate(&ente.ErrNotFoundError, "")
	}
	return nil
}

func (c *FileController) lockOwnedDriveUpload(ctx context.Context, tx *sql.Tx, userID int64, objectKey string, now int64) (repo.LockedTempObject, error) {
	upload, err := c.ObjectCleanupRepo.LockLiveTempObject(ctx, tx, objectKey, now)
	if errors.Is(err, sql.ErrNoRows) {
		return upload, stacktrace.Propagate(ente.ErrUploadGone, "")
	}
	if errors.Is(err, repo.ErrTempObjectLocked) {
		return upload, stacktrace.Propagate(ente.ErrUploadBusy, "")
	}
	if err != nil {
		return upload, stacktrace.Propagate(err, "")
	}
	if upload.UserID != userID || upload.IsCopy {
		return upload, stacktrace.Propagate(&ente.ErrNotFoundError, "")
	}
	if upload.App != ente.Drive || upload.Purpose != "file_upload" {
		return upload, stacktrace.Propagate(notDriveMultipartUpload(), "")
	}
	return upload, nil
}

func notDriveMultipartUpload() error {
	return ente.NewBadRequestWithMessage("not a Drive multipart upload")
}

func (c *FileController) listUploadedParts(ctx context.Context, dc string, objectKey string, uploadID string) ([]ente.MultipartUploadPart, error) {
	s3Client := c.S3Config.GetS3Client(dc)
	return listMultipartUploadParts(ctx, &s3Client, c.S3Config.GetBucket(dc), objectKey, uploadID)
}

func listMultipartUploadParts(ctx context.Context, s3Client *s3.S3, bucket *string, objectKey string, uploadID string) ([]ente.MultipartUploadPart, error) {
	parts := make([]ente.MultipartUploadPart, 0)
	var marker *int64
	for {
		output, err := s3Client.ListPartsWithContext(ctx, &s3.ListPartsInput{
			Bucket:           bucket,
			Key:              &objectKey,
			UploadId:         &uploadID,
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		for _, part := range output.Parts {
			parts = append(parts, ente.MultipartUploadPart{
				PartNumber: aws.Int64Value(part.PartNumber),
				ETag:       aws.StringValue(part.ETag),
				Size:       aws.Int64Value(part.Size),
			})
		}
		if !aws.BoolValue(output.IsTruncated) {
			return parts, nil
		}
		next := aws.Int64Value(output.NextPartNumberMarker)
		// Some S3-compatible stores omit NextPartNumberMarker.
		if next == 0 && len(output.Parts) > 0 {
			next = aws.Int64Value(output.Parts[len(output.Parts)-1].PartNumber)
		}
		if next <= aws.Int64Value(marker) {
			return nil, stacktrace.NewError("truncated part listing did not advance the part number marker")
		}
		marker = &next
	}
}

func (c *FileController) headObject(ctx context.Context, dc string, objectKey string) (int64, bool, error) {
	s3Client := c.S3Config.GetS3Client(dc)
	for attempt := 0; ; attempt++ {
		output, err := s3Client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
			Bucket: c.S3Config.GetBucket(dc),
			Key:    &objectKey,
		})
		if err == nil {
			return aws.Int64Value(output.ContentLength), true, nil
		}
		var requestFailure awserr.RequestFailure
		if !errors.As(err, &requestFailure) || requestFailure.StatusCode() != http.StatusNotFound {
			return 0, false, stacktrace.Propagate(err, "")
		}
		if attempt == len(headNotFoundRetryDelays) {
			return 0, false, nil
		}
		select {
		case <-ctx.Done():
			return 0, false, stacktrace.Propagate(ctx.Err(), "")
		case <-gTime.After(headNotFoundRetryDelays[attempt]):
		}
	}
}
