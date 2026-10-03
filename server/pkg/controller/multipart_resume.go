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

// The S3 client has no timeout, and resume holds a pooled DB connection and the
// row lock while it talks to S3.
const multipartUploadStorageTimeout = 60 * gTime.Second

// A gone verdict lets the cron delete the object, so don't trust a 404 HEAD
// right after a slow CompleteMultipartUpload.
var headNotFoundRetryDelays = []gTime.Duration{500 * gTime.Millisecond, 1500 * gTime.Millisecond}

func (c *FileController) ResumeMultipartUpload(ctx context.Context, userID int64, objectKey string) (ente.MultipartUploadResume, error) {
	if err := requireOwnObjectKey(userID, objectKey); err != nil {
		return ente.MultipartUploadResume{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, multipartUploadStorageTimeout)
	defer cancel()
	tx, err := c.ObjectCleanupRepo.DB.BeginTx(ctx, nil)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	now := time.Microseconds()
	upload, err := c.lockOwnedDriveUpload(ctx, tx, userID, objectKey, now)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if upload.ContentLength == nil || upload.PartLength == nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.NewBadRequestWithMessage("upload is not resumable"), "")
	}
	dc := c.uploadDataCenter(upload)
	parts, err := c.listUploadedParts(ctx, dc, objectKey, upload.UploadID)
	if err != nil && isUnknownUploadError(err) {
		return c.resumeAssembledUpload(ctx, tx, upload, dc, now)
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
	for i, length := range lengths {
		partNumber := int64(i + 1)
		if uploaded[partNumber] {
			continue
		}
		url, err := c.getPartURL(dc, objectKey, partNumber, &upload.UploadID, &length, nil)
		if err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
		resume.PartURLs[partNumber] = url
	}
	resume.CompleteURL, err = c.getCompleteURL(dc, objectKey, &upload.UploadID)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if completed := int64(len(resume.CompletedParts)); completed > upload.ResumePartsCompleted {
		expiry := now + 2*PreSignedPartUploadRequestDuration.Microseconds()
		if err := c.ObjectCleanupRepo.ExtendTempObjectExpiry(ctx, tx, objectKey, expiry, maxResumableUploadAge.Microseconds(), &completed); err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
	}
	if err := tx.Commit(); err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	return resume, nil
}

// The client may have completed the upload and lost the response.
func (c *FileController) resumeAssembledUpload(ctx context.Context, tx *sql.Tx, upload repo.LockedTempObject, dc string, now int64) (ente.MultipartUploadResume, error) {
	size, found, err := c.headObject(ctx, dc, upload.ObjectKey)
	if err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if !found || size != *upload.ContentLength {
		// Neither can ever be committed: expire now so the cron deletes any
		// stray object and the reservation is released.
		if err := c.ObjectCleanupRepo.ExpireLockedTempObject(ctx, tx, upload.ObjectKey, upload.UserID, now); err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
		if err := tx.Commit(); err != nil {
			return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
		}
		return ente.MultipartUploadResume{}, stacktrace.Propagate(ente.ErrUploadGone, "found=%t size=%d", found, size)
	}
	expiry := now + completedUploadMinValidity.Microseconds()
	if err := c.ObjectCleanupRepo.ExtendTempObjectExpiry(ctx, tx, upload.ObjectKey, expiry, maxResumableUploadAge.Microseconds(), nil); err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	if err := tx.Commit(); err != nil {
		return ente.MultipartUploadResume{}, stacktrace.Propagate(err, "")
	}
	return ente.MultipartUploadResume{Completed: true}, nil
}

// The row is kept: after a completed upload it is the only record of the
// object, and the cleanup cron deletes it only if it was never committed.
func (c *FileController) AbortMultipartUpload(ctx context.Context, userID int64, objectKey string) error {
	if err := requireOwnObjectKey(userID, objectKey); err != nil {
		return err
	}
	upload, err := c.expireOwnedDriveUpload(ctx, userID, objectKey)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	// Best effort: the row is already expired, so the cleanup cron aborts the
	// upload if this fails or the client disconnects.
	ctx, cancel := context.WithTimeout(ctx, multipartUploadStorageTimeout)
	defer cancel()
	if err := c.ObjectCleanupCtrl.abortMultipartUploadWithContext(ctx, objectKey, upload.UploadID, c.uploadDataCenter(upload)); err != nil {
		log.WithError(err).WithField("object_key", objectKey).Warn("Failed to abort multipart upload, leaving it to the cleanup cron")
	}
	return nil
}

func (c *FileController) expireOwnedDriveUpload(ctx context.Context, userID int64, objectKey string) (repo.LockedTempObject, error) {
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
	if err := c.ObjectCleanupRepo.ExpireLockedTempObject(ctx, tx, objectKey, userID, now); err != nil {
		return upload, stacktrace.Propagate(err, "")
	}
	return upload, stacktrace.Propagate(tx.Commit(), "")
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
	if upload.UserID != userID {
		return upload, stacktrace.Propagate(&ente.ErrNotFoundError, "")
	}
	if upload.App != ente.Drive || !upload.IsMultipart || upload.Purpose != "file_upload" || upload.UploadID == "" {
		return upload, stacktrace.Propagate(ente.NewBadRequestWithMessage("not a Drive multipart upload"), "")
	}
	return upload, nil
}

func (c *FileController) uploadDataCenter(upload repo.LockedTempObject) string {
	if upload.BucketId == "" {
		return c.S3Config.GetHotDataCenter()
	}
	return upload.BucketId
}

func (c *FileController) listUploadedParts(ctx context.Context, dc string, objectKey string, uploadID string) ([]ente.MultipartUploadPart, error) {
	s3Client := c.S3Config.GetS3Client(dc)
	parts := make([]ente.MultipartUploadPart, 0)
	var marker *int64
	for {
		output, err := s3Client.ListPartsWithContext(ctx, &s3.ListPartsInput{
			Bucket:           c.S3Config.GetBucket(dc),
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
