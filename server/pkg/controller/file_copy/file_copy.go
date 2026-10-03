package file_copy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/controller"
	"github.com/ente/museum/pkg/controller/collections"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/auth"
	"github.com/ente/museum/pkg/utils/network"
	"github.com/ente/museum/pkg/utils/s3config"
	"github.com/ente/museum/pkg/utils/s3copy"
	enteTime "github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
	"github.com/gin-contrib/requestid"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

const driveCopyConcurrency = 8

// A cancelled CopyObject or CompleteMultipartUpload can still finish on the
// provider; the cron must not delete the row before that object would appear.
const failedDriveCopyCleanupDelay = time.Hour

var (
	maxSingleCopySize = s3copy.MaxSingleCopySize
	minCopyPartSize   = s3copy.MinPartSize
)

type FileCopyController struct {
	S3Config       *s3config.S3Config
	FileController *controller.FileController
	FileRepo       *repo.FileRepository
	CollectionCtrl *collections.CollectionController
	ObjectRepo     *repo.ObjectRepository
}

type copyS3ObjectReq struct {
	SourceS3Object ente.S3ObjectKey
	DestObjectKey  string
}

type fileCopyInternal struct {
	SourceFile       ente.File
	DestCollectionID int64
	// The FileKey is encrypted with the destination collection's key
	EncryptedFileKey      string
	EncryptedFileKeyNonce string
	FileCopyReq           *copyS3ObjectReq
	ThumbCopyReq          *copyS3ObjectReq
}

func (fci fileCopyInternal) newFile(ownedID int64) ente.File {
	newFileAttributes := fci.SourceFile.File
	newFileAttributes.ObjectKey = fci.FileCopyReq.DestObjectKey
	newThumbAttributes := fci.SourceFile.Thumbnail
	newThumbAttributes.ObjectKey = fci.ThumbCopyReq.DestObjectKey
	return ente.File{
		OwnerID:            ownedID,
		CollectionID:       fci.DestCollectionID,
		EncryptedKey:       fci.EncryptedFileKey,
		KeyDecryptionNonce: fci.EncryptedFileKeyNonce,
		File:               newFileAttributes,
		Thumbnail:          newThumbAttributes,
		Metadata:           fci.SourceFile.Metadata,
		PubicMagicMetadata: fci.SourceFile.PubicMagicMetadata,
		UpdationTime:       enteTime.Microseconds(),
		IsDeleted:          false,
	}
}

func (fc *FileCopyController) CopyFiles(c *gin.Context, req ente.CopyFileSyncRequest) (*ente.CopyResponse, error) {
	userID := auth.GetUserID(c.Request.Header)
	app := auth.GetApp(c)
	logger := logrus.WithFields(logrus.Fields{"req_id": requestid.Get(c), "user_id": userID})
	dstApp, err := fc.CollectionCtrl.IsCopyAllowed(c, userID, req)
	if err != nil {
		return nil, err
	}
	isDrive := dstApp == ente.Drive
	if (app == ente.Drive || isDrive) && app != dstApp {
		return nil, stacktrace.Propagate(&ente.ErrCrossAppFile, "copy into a %s collection with app %s", dstApp, app)
	}
	fileIDs := make([]int64, 0, len(req.CollectionFileItems))
	fileToCollectionFileMap := make(map[int64]*ente.CollectionFileItem, len(req.CollectionFileItems))
	for i := range req.CollectionFileItems {
		item := &req.CollectionFileItems[i]
		fileToCollectionFileMap[item.ID] = item
		fileIDs = append(fileIDs, item.ID)
	}
	s3ObjectsToCopy, err := fc.ObjectRepo.GetObjectsForFileIDs(fileIDs)
	if err != nil {
		return nil, err
	}
	// Video previews are not tracked in object_keys.
	if len(s3ObjectsToCopy) != 2*len(fileIDs) {
		return nil, ente.NewInternalError(fmt.Sprintf("expected %d objects, got %d", 2*len(fileIDs), len(s3ObjectsToCopy)))
	}
	// todo:(neeraj) if the total size is greater than 1GB, do an early check if the user can upload the existingFilesToCopy
	// (Drive does it with the batch reservation.)
	var totalSize int64
	for _, obj := range s3ObjectsToCopy {
		totalSize += obj.FileSize
	}
	logger.WithField("totalSize", totalSize).Info("total size of existingFilesToCopy to copy")

	var destKeys []string
	if isDrive {
		for _, obj := range s3ObjectsToCopy {
			if obj.Type != ente.FILE {
				continue
			}
			if err := fc.FileController.CheckFileSize(c.Request.Context(), userID, obj.FileSize, ente.Drive); err != nil {
				return nil, stacktrace.Propagate(err, "")
			}
		}
		destKeys = make([]string, len(s3ObjectsToCopy))
		for i := range destKeys {
			destKeys[i] = strconv.FormatInt(userID, 10) + "/" + uuid.NewString()
		}
	} else {
		// Reuse upload URLs so abandoned copies are cleaned up as orphan objects.
		// todo:(neeraj) optimize this method by removing the need for getting a signed url for each object
		uploadUrls, err := fc.FileController.GetUploadURLs(c, userID, len(s3ObjectsToCopy), app, true, network.GetClientInfo(c))
		if err != nil {
			return nil, err
		}
		destKeys = make([]string, len(uploadUrls))
		for i, uploadURL := range uploadUrls {
			destKeys[i] = uploadURL.ObjectKey
		}
	}
	existingFilesToCopy, err := fc.FileRepo.GetFileAttributesForCopy(fileIDs)
	if err != nil {
		return nil, err
	}
	if len(existingFilesToCopy) != len(fileIDs) {
		return nil, ente.NewInternalError(fmt.Sprintf("expected %d existingFilesToCopy, got %d", len(fileIDs), len(existingFilesToCopy)))
	}
	fileOGS3Object := make(map[int64]*copyS3ObjectReq)
	fileThumbS3Object := make(map[int64]*copyS3ObjectReq)
	for i, s3Obj := range s3ObjectsToCopy {
		if s3Obj.Type == ente.FILE {
			fileOGS3Object[s3Obj.FileID] = &copyS3ObjectReq{
				SourceS3Object: s3Obj,
				DestObjectKey:  destKeys[i],
			}
		} else if s3Obj.Type == ente.THUMBNAIL {
			fileThumbS3Object[s3Obj.FileID] = &copyS3ObjectReq{
				SourceS3Object: s3Obj,
				DestObjectKey:  destKeys[i],
			}
		} else {
			return nil, ente.NewInternalError(fmt.Sprintf("unexpected object type %s", s3Obj.Type))
		}
	}
	fileCopyList := make([]fileCopyInternal, 0, len(existingFilesToCopy))
	for i := range existingFilesToCopy {
		file := existingFilesToCopy[i]
		collectionItem := fileToCollectionFileMap[file.ID]
		if collectionItem.ID != file.ID {
			return nil, ente.NewInternalError(fmt.Sprintf("expected collectionItem.ID %d, got %d", file.ID, collectionItem.ID))
		}
		fileCopy := fileCopyInternal{
			SourceFile:            file,
			DestCollectionID:      req.DstCollection,
			EncryptedFileKey:      fileToCollectionFileMap[file.ID].EncryptedKey,
			EncryptedFileKeyNonce: fileToCollectionFileMap[file.ID].KeyDecryptionNonce,
			FileCopyReq:           fileOGS3Object[file.ID],
			ThumbCopyReq:          fileThumbS3Object[file.ID],
		}
		fileCopyList = append(fileCopyList, fileCopy)
	}
	if isDrive {
		return fc.copyDriveFiles(c, userID, fileCopyList)
	}
	oldToNewFileIDMap := make(map[int64]int64)
	var mapMutex sync.Mutex
	var wg sync.WaitGroup
	errChan := make(chan error, len(fileCopyList))

	for _, fileCopy := range fileCopyList {
		wg.Go(func() {
			newFile, err := fc.createCopy(c, c.Request.Context(), fileCopy, userID, app, dstApp)
			if err != nil {
				errChan <- err
				return
			}
			mapMutex.Lock()
			oldToNewFileIDMap[fileCopy.SourceFile.ID] = newFile.ID
			mapMutex.Unlock()
		})
	}

	wg.Wait()

	close(errChan)
	if err, ok := <-errChan; ok {
		return nil, err
	}
	return &ente.CopyResponse{OldToNewFileIDMap: oldToNewFileIDMap}, nil
}

func (fc *FileCopyController) copyDriveFiles(c *gin.Context, userID int64, fileCopyList []fileCopyInternal) (*ente.CopyResponse, error) {
	ctx := c.Request.Context()
	objects := make([]ente.TempObject, 0, 2*len(fileCopyList))
	destKeys := make([]string, 0, 2*len(fileCopyList))
	client := network.GetClientInfo(c)
	opts := fc.copyOptions()
	for _, fileCopy := range fileCopyList {
		for _, copyReq := range []*copyS3ObjectReq{fileCopy.FileCopyReq, fileCopy.ThumbCopyReq} {
			size := copyReq.SourceS3Object.FileSize
			object := ente.TempObject{
				ObjectKey:     copyReq.DestObjectKey,
				BucketId:      fc.S3Config.GetHotDataCenter(),
				UserID:        userID,
				App:           ente.Drive,
				Purpose:       "file_upload",
				ContentLength: &size,
				Client:        client,
			}
			if opts.IsMultipart(size) {
				partSize := opts.PartSize(size)
				object.PartLength = &partSize
			}
			objects = append(objects, object)
			destKeys = append(destKeys, copyReq.DestObjectKey)
		}
	}
	if err := fc.FileController.ReserveDriveUploads(ctx, userID, objects); err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	oldToNewFileIDMap := make(map[int64]int64, len(fileCopyList))
	var mapMutex sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(driveCopyConcurrency)
	for _, fileCopy := range fileCopyList {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			newFile, err := fc.createCopy(c, gctx, fileCopy, userID, ente.Drive, ente.Drive)
			if err != nil {
				return err
			}
			mapMutex.Lock()
			oldToNewFileIDMap[fileCopy.SourceFile.ID] = newFile.ID
			mapMutex.Unlock()
			return nil
		})
	}
	err := g.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = stacktrace.Propagate(ctxErr, "")
	}
	if err != nil {
		// Committed copies no longer have a row; the cron deletes the rest.
		expiry := time.Now().Add(failedDriveCopyCleanupDelay).UnixMicro()
		if releaseErr := fc.FileController.ObjectCleanupRepo.ReleaseTempObjects(context.WithoutCancel(ctx), destKeys, userID, expiry); releaseErr != nil {
			logrus.WithError(releaseErr).WithField("user_id", userID).Error("Failed to release the reservation of a failed copy")
		}
		return nil, err
	}
	return &ente.CopyResponse{OldToNewFileIDMap: oldToNewFileIDMap}, nil
}

func (fc *FileCopyController) createCopy(c *gin.Context, ctx context.Context, fcInternal fileCopyInternal, userID int64, app ente.App, dstApp ente.App) (*ente.File, error) {
	isDrive := dstApp == ente.Drive
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return fc.copyObject(gctx, fcInternal.FileCopyReq, userID, dstApp)
	})
	g.Go(func() error {
		return fc.copyObject(gctx, fcInternal.ThumbCopyReq, userID, dstApp)
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	file := fcInternal.newFile(userID)
	if isDrive {
		if err := ctx.Err(); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		file.File.Size = fcInternal.FileCopyReq.SourceS3Object.FileSize
		file.Thumbnail.Size = fcInternal.ThumbCopyReq.SourceS3Object.FileSize
	}
	newFile, err := fc.FileController.Create(c, userID, file, "", app, false)
	if err != nil {
		return nil, err
	}
	return &newFile, nil
}

func (fc *FileCopyController) copyOptions() s3copy.Options {
	maxParts := 1000
	if fc.S3Config.GetHotDataCenter() == fc.S3Config.GetHotBackblazeDC() {
		maxParts = 10000
	}
	opts := s3copy.DefaultOptions(maxParts)
	opts.MaxSingleCopySize = maxSingleCopySize
	opts.MinPartSize = minCopyPartSize
	return opts
}

func (fc *FileCopyController) copyObject(ctx context.Context, req *copyS3ObjectReq, userID int64, dstApp ente.App) error {
	if dstApp != ente.Drive {
		return fc.copyObjectWithFallback(ctx, req, userID, dstApp)
	}
	size := req.SourceS3Object.FileSize
	opts := fc.copyOptions()
	if opts.IsMultipart(size) {
		opts.OnUploadCreated = fc.uploadIDRecorder(ctx, req.DestObjectKey)
	}
	start := time.Now()
	err := s3copy.Copy(ctx, fc.S3Config.GetHotS3Client(), *fc.S3Config.GetHotBucket(), req.SourceS3Object.ObjectKey, req.DestObjectKey, size, opts)
	if errors.Is(err, s3copy.ErrSourceSizeMismatch) {
		return stacktrace.Propagate(ente.ErrBadRequest, "%v", err)
	}
	if err != nil {
		return copyFailed(err, req)
	}
	logCopied(req, start)
	return nil
}

// Photos/Locker always try today's CopyObject first. Only B2 rejects sources
// above MaxSingleCopySize, and the provider can't be told from the hot DC's
// name (self-hosters must call it b2-eu-cen), so only its failures are copied
// in parts.
func (fc *FileCopyController) copyObjectWithFallback(ctx context.Context, req *copyS3ObjectReq, userID int64, dstApp ente.App) error {
	copyErr := copyS3Object(fc.S3Config.GetHotS3Client(), fc.S3Config.GetHotBucket(), req)
	size := req.SourceS3Object.FileSize
	opts := fc.copyOptions()
	if copyErr == nil || !opts.IsMultipart(size) {
		return copyErr
	}
	// Create would reject the copy anyway; keep today's error instead of
	// copying the whole object first.
	if err := fc.FileController.CheckFileSize(ctx, userID, size, dstApp); err != nil {
		return copyErr
	}
	logrus.WithError(copyErr).WithField("size", size).Warn("CopyObject failed, copying in parts")
	cleanupRepo := fc.FileController.ObjectCleanupRepo
	if err := cleanupRepo.SetTempObjectPartLength(ctx, req.DestObjectKey, opts.PartSize(size), enteTime.Microseconds()); err != nil {
		return copyFailed(err, req)
	}
	opts.OnUploadCreated = fc.uploadIDRecorder(ctx, req.DestObjectKey)
	start := time.Now()
	err := s3copy.CopyMultipart(ctx, fc.S3Config.GetHotS3Client(), *fc.S3Config.GetHotBucket(), req.SourceS3Object.ObjectKey, req.DestObjectKey, size, opts)
	if err != nil {
		return copyFailed(err, req)
	}
	logCopied(req, start)
	return nil
}

func (fc *FileCopyController) uploadIDRecorder(ctx context.Context, objectKey string) func(string) error {
	return func(uploadID string) error {
		return fc.FileController.ObjectCleanupRepo.SetTempObjectUploadID(context.WithoutCancel(ctx), objectKey, uploadID, enteTime.Microseconds())
	}
}

func copyFailed(err error, req *copyS3ObjectReq) error {
	// Not a 410: the client never started this upload and can't resume it.
	if errors.Is(err, ente.ErrUploadGone) {
		return stacktrace.Propagate(ente.NewInternalError("copy destination is gone"), "%v", err)
	}
	return stacktrace.Propagate(err, "failed to copy (%s) from %s to %s", req.SourceS3Object.Type, req.SourceS3Object.ObjectKey, req.DestObjectKey)
}

func logCopied(req *copyS3ObjectReq, start time.Time) {
	logrus.WithField("duration", time.Since(start)).WithField("size", req.SourceS3Object.FileSize).Infof("copied (%s) from %s to %s", req.SourceS3Object.Type, req.SourceS3Object.ObjectKey, req.DestObjectKey)
}

func copyS3Object(s3Client *s3.S3, bucket *string, req *copyS3ObjectReq) error {
	copySource := fmt.Sprintf("%s/%s", *bucket, req.SourceS3Object.ObjectKey)
	copyInput := &s3.CopyObjectInput{
		Bucket:     bucket,
		CopySource: &copySource,
		Key:        &req.DestObjectKey,
	}
	start := time.Now()
	_, err := s3Client.CopyObject(copyInput)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("failed to copy (%s) from %s to %s: %w", req.SourceS3Object.Type, copySource, req.DestObjectKey, err)
	}
	logrus.WithField("duration", elapsed).WithField("size", req.SourceS3Object.FileSize).Infof("copied (%s) from %s to %s", req.SourceS3Object.Type, copySource, req.DestObjectKey)
	return nil
}
