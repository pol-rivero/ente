package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ente/stacktrace"
	"github.com/lib/pq"

	"github.com/ente/museum/ente"
	"github.com/ente/museum/pkg/utils/time"
)

// ObjectCleanupRepository maintains state related to objects that might need to
// be cleaned up.
//
// In particular, all presigned urls start their life as a "temp object" that is
// liable to be cleaned up if not marked as a successful upload by the client.
type ObjectCleanupRepository struct {
	DB *sql.DB
}

const insertTempObjectQuery = `
		INSERT INTO temp_objects (
		    object_key, expiration_time, upload_id, is_multipart, bucket_id,
		    user_id, app, purpose, content_length, content_md5, client, part_length, reservation_released, is_copy
		) VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6::BIGINT, 0), NULLIF($7, ''), NULLIF($8, ''), $9, $10, NULLIF($11, ''), $12, $13, $14)`

func insertTempObjectArgs(tempObject ente.TempObject, expirationTime int64) []any {
	return []any{tempObject.ObjectKey, expirationTime, tempObject.UploadID, tempObject.IsMultipart, tempObject.BucketId,
		tempObject.UserID, tempObject.App, tempObject.Purpose, tempObject.ContentLength, tempObject.ContentMD5, tempObject.Client,
		tempObject.PartLength, tempObject.ReservationReleased, tempObject.IsCopy}
}

func (repo *ObjectCleanupRepository) AddTempObject(tempObject ente.TempObject, expirationTime int64) error {
	_, err := repo.DB.Exec(insertTempObjectQuery, insertTempObjectArgs(tempObject, expirationTime)...)
	return stacktrace.Propagate(err, "")
}

func (repo *ObjectCleanupRepository) AddTempObjectTx(ctx context.Context, tx *sql.Tx, tempObject ente.TempObject, expirationTime int64) error {
	_, err := tx.ExecContext(ctx, insertTempObjectQuery, insertTempObjectArgs(tempObject, expirationTime)...)
	return stacktrace.Propagate(err, "")
}

// A reserved multipart start is inserted before CreateMultipartUpload as a
// "pending" row: is_multipart = FALSE and upload_id NULL, with part_length set.
// Older binaries' cleanup cron then deletes it like an unused single PUT
// instead of calling AbortMultipartUpload with an empty upload ID.
//
// Expired or released rows aren't updated: the cron may hold them locked
// across S3 calls, and their upload was cancelled anyway.
func (repo *ObjectCleanupRepository) SetTempObjectUploadID(ctx context.Context, objectKey string, uploadID string, now int64) error {
	updated, err := affectsOne(repo.DB.ExecContext(ctx, `
		UPDATE temp_objects SET upload_id = $2, is_multipart = TRUE
		WHERE object_key = $1 AND upload_id IS NULL AND NOT is_multipart
		  AND expiration_time > $3 AND NOT reservation_released`,
		objectKey, uploadID, now))
	if err != nil {
		return err
	}
	if !updated {
		return stacktrace.Propagate(ente.ErrUploadGone, "temp object %s not found to set its upload ID", objectKey)
	}
	return nil
}

// Turns an unused single-PUT row into a pending multipart one, so a crash
// after CreateMultipartUpload leaves a row whose upload the cron finds.
func (repo *ObjectCleanupRepository) SetTempObjectPartLength(ctx context.Context, objectKey string, partLength int64, now int64) error {
	updated, err := affectsOne(repo.DB.ExecContext(ctx, `
		UPDATE temp_objects SET part_length = $2
		WHERE object_key = $1 AND upload_id IS NULL AND NOT is_multipart
		  AND expiration_time > $3 AND NOT reservation_released`,
		objectKey, partLength, now))
	if err != nil {
		return err
	}
	if !updated {
		return stacktrace.Propagate(ente.ErrUploadGone, "temp object %s not found to set its part length", objectKey)
	}
	return nil
}

// The quota sum (UsageRepository.GetUsageWithDriveReservations) and admission
// at Create must agree on which rows hold a reservation. $2 is now.
const liveDriveReservation = `t.app = 'drive' AND t.purpose = 'file_upload'
	AND t.expiration_time > $2 AND NOT t.reservation_released`

func (repo *ObjectCleanupRepository) GetDriveReservedKeys(ctx context.Context, userID int64, sizes map[string]int64, now int64) (map[string]bool, error) {
	keys := make([]string, 0, len(sizes))
	lengths := make([]int64, 0, len(sizes))
	for key, size := range sizes {
		keys = append(keys, key)
		lengths = append(lengths, size)
	}
	rows, err := repo.DB.QueryContext(ctx, `
		SELECT t.object_key FROM temp_objects t
		JOIN unnest($3::text[], $4::bigint[]) AS c(object_key, size) ON c.object_key = t.object_key
		WHERE t.user_id = $1 AND t.content_length >= c.size AND `+liveDriveReservation,
		userID, now, pq.Array(keys), pq.Array(lengths))
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	reserved := make(map[string]bool, len(sizes))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		reserved[key] = true
	}
	return reserved, stacktrace.Propagate(rows.Err(), "")
}

func (repo *ObjectCleanupRepository) ExpireTempObjectNow(ctx context.Context, objectKey string, userID int64) error {
	return expireTempObject(ctx, repo.DB, objectKey, userID, time.Microseconds())
}

func (repo *ObjectCleanupRepository) ReleaseTempObjects(ctx context.Context, objectKeys []string, userID int64, expirationTime int64) error {
	return releaseTempObjects(ctx, repo.DB, objectKeys, userID, expirationTime)
}

func (repo *ObjectCleanupRepository) ReleaseTempObjectsTx(ctx context.Context, tx *sql.Tx, objectKeys []string, userID int64, expirationTime int64) error {
	return releaseTempObjects(ctx, tx, objectKeys, userID, expirationTime)
}

func releaseTempObjects(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, objectKeys []string, userID int64, expirationTime int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE temp_objects SET expiration_time = $1, reservation_released = TRUE
		WHERE object_key = ANY($2) AND user_id = $3 AND expiration_time > $4`,
		expirationTime, pq.Array(objectKeys), userID, time.Microseconds())
	return stacktrace.Propagate(err, "")
}

func (repo *ObjectCleanupRepository) GetLiveTempObjects(ctx context.Context, userID int64, objectKeys []string, now int64) (map[string]ente.TempObject, error) {
	rows, err := repo.DB.QueryContext(ctx, `
		SELECT object_key, is_multipart, upload_id, bucket_id, part_length FROM temp_objects
		WHERE object_key = ANY($1) AND user_id = $2 AND expiration_time > $3 AND NOT reservation_released`,
		pq.Array(objectKeys), userID, now)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	objects := make(map[string]ente.TempObject)
	for rows.Next() {
		object := ente.TempObject{UserID: userID}
		var uploadID, bucketID sql.NullString
		var partLength sql.NullInt64
		if err := rows.Scan(&object.ObjectKey, &object.IsMultipart, &uploadID, &bucketID, &partLength); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		object.UploadID = uploadID.String
		object.BucketId = bucketID.String
		if partLength.Valid {
			object.PartLength = &partLength.Int64
		}
		objects[object.ObjectKey] = object
	}
	return objects, stacktrace.Propagate(rows.Err(), "")
}

// Turns a row whose upload was aborted back into a pending one, so the copy can start again.
func (repo *ObjectCleanupRepository) ResetTempObjectUpload(ctx context.Context, objectKey string, uploadID string, now int64) error {
	updated, err := affectsOne(repo.DB.ExecContext(ctx, `
		UPDATE temp_objects SET upload_id = NULL, is_multipart = FALSE
		WHERE object_key = $1 AND upload_id = $2 AND expiration_time > $3 AND NOT reservation_released`,
		objectKey, uploadID, now))
	if err != nil {
		return err
	}
	if !updated {
		return stacktrace.Propagate(ente.ErrUploadGone, "temp object %s not found to reset its upload", objectKey)
	}
	return nil
}

func (repo *ObjectCleanupRepository) ExpireLockedTempObject(ctx context.Context, tx *sql.Tx, objectKey string, userID int64, now int64) error {
	return expireTempObject(ctx, tx, objectKey, userID, now)
}

func expireTempObject(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, objectKey string, userID int64, now int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE temp_objects SET expiration_time = $1, reservation_released = TRUE
		WHERE object_key = $2 AND user_id = $3 AND expiration_time > $1`,
		now, objectKey, userID)
	return stacktrace.Propagate(err, "")
}

var ErrTempObjectLocked = errors.New("temp object is locked by another transaction")

type LockedTempObject struct {
	ente.TempObject
	ResumePartsCompleted int64
	ExpirationTime       int64
}

// NOWAIT: fail fast if a concurrent resume, abort or Create holds the row.
func (repo *ObjectCleanupRepository) LockLiveTempObject(ctx context.Context, tx *sql.Tx, objectKey string, now int64) (LockedTempObject, error) {
	var row LockedTempObject
	var uploadID, bucketID, app, purpose sql.NullString
	var userID, contentLength, partLength sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT is_multipart, upload_id, bucket_id, user_id, app, purpose, content_length, part_length,
		       COALESCE(resume_parts_completed, 0), expiration_time, is_copy
		FROM temp_objects
		WHERE object_key = $1 AND expiration_time > $2 AND NOT reservation_released
		FOR UPDATE NOWAIT`, objectKey, now).
		Scan(&row.IsMultipart, &uploadID, &bucketID, &userID, &app, &purpose, &contentLength, &partLength,
			&row.ResumePartsCompleted, &row.ExpirationTime, &row.IsCopy)
	if isLockTimeout(err) {
		return row, stacktrace.Propagate(ErrTempObjectLocked, "")
	}
	if err != nil {
		return row, stacktrace.Propagate(err, "")
	}
	row.ObjectKey = objectKey
	row.UploadID = uploadID.String
	row.BucketId = bucketID.String
	row.UserID = userID.Int64
	row.App = ente.App(app.String)
	row.Purpose = purpose.String
	if contentLength.Valid {
		row.ContentLength = &contentLength.Int64
	}
	if partLength.Valid {
		row.PartLength = &partLength.Int64
	}
	return row, nil
}

// The upload read by LockLiveTempObject may have been committed, cancelled or
// expired since; found is false then. partsCompleted may be nil. NOWAIT, like
// LockLiveTempObject: the cron may hold the row across S3 calls.
func (repo *ObjectCleanupRepository) ExtendLiveUploadExpiry(ctx context.Context, objectKey string, uploadID string, now int64,
	expiry int64, maxAge int64, partsCompleted *int64) (newExpiry int64, found bool, err error) {
	err = repo.DB.QueryRowContext(ctx, `
		WITH locked AS (
			SELECT object_key FROM temp_objects
			WHERE object_key = $1 AND upload_id = $2 AND expiration_time > $3 AND NOT reservation_released
			FOR UPDATE NOWAIT
		)
		UPDATE temp_objects t
		SET expiration_time = GREATEST(t.expiration_time, LEAST($4, t.created_at + $5)),
			resume_parts_completed = GREATEST(t.resume_parts_completed, $6)
		FROM locked WHERE t.object_key = locked.object_key
		RETURNING t.expiration_time`,
		objectKey, uploadID, now, expiry, maxAge, partsCompleted).Scan(&newExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if isLockTimeout(err) {
		return 0, false, stacktrace.Propagate(ErrTempObjectLocked, "")
	}
	return newExpiry, err == nil, stacktrace.Propagate(err, "")
}

func (repo *ObjectCleanupRepository) ExpireLiveUpload(ctx context.Context, objectKey string, uploadID string, userID int64, now int64) error {
	_, err := repo.DB.ExecContext(ctx, `
		WITH locked AS (
			SELECT object_key FROM temp_objects
			WHERE object_key = $2 AND upload_id = $3 AND user_id = $4 AND expiration_time > $1 AND NOT reservation_released
			FOR UPDATE NOWAIT
		)
		UPDATE temp_objects t SET expiration_time = $1, reservation_released = TRUE
		FROM locked WHERE t.object_key = locked.object_key`,
		now, objectKey, uploadID, userID)
	if isLockTimeout(err) {
		return stacktrace.Propagate(ErrTempObjectLocked, "")
	}
	return stacktrace.Propagate(err, "")
}

func (repo *ObjectCleanupRepository) IsLiveUpload(ctx context.Context, objectKey string, uploadID string, now int64) (bool, error) {
	var live bool
	err := repo.DB.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM temp_objects
			WHERE object_key = $1 AND upload_id = $2 AND expiration_time > $3 AND NOT reservation_released)`,
		objectKey, uploadID, now).Scan(&live)
	return live, stacktrace.Propagate(err, "")
}

// Pages are ordered by object key, starting after the given one. Copy
// reservations aren't the client's to resume or abort.
func (repo *ObjectCleanupRepository) GetLiveDriveReservations(ctx context.Context, userID int64, now int64, afterKey string, limit int) ([]ente.PendingUpload, error) {
	rows, err := repo.DB.QueryContext(ctx, `
		SELECT t.object_key, COALESCE(t.content_length, 0), t.is_multipart OR t.part_length IS NOT NULL,
		       COALESCE(t.created_at, 0), t.expiration_time
		FROM temp_objects t
		WHERE t.user_id = $1 AND t.object_key > $3 AND NOT t.is_copy AND `+liveDriveReservation+`
		ORDER BY t.object_key
		LIMIT $4`, userID, now, afterKey, limit)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	uploads := make([]ente.PendingUpload, 0)
	for rows.Next() {
		var upload ente.PendingUpload
		if err := rows.Scan(&upload.ObjectKey, &upload.ContentLength, &upload.IsMultipart, &upload.CreatedAt, &upload.ExpiresAt); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		uploads = append(uploads, upload)
	}
	return uploads, stacktrace.Propagate(rows.Err(), "")
}

func (repo *ObjectCleanupRepository) RemoveTempObjectKey(ctx context.Context, tx *sql.Tx, objectKey string, dc string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM temp_objects WHERE object_key = $1`, objectKey)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if rowsAffected != 1 {
		return stacktrace.Propagate(ente.NewBadRequestWithMessage("staged upload not found"), "")
	}
	return nil
}

func (repo *ObjectCleanupRepository) RemoveTempObjectFromDC(ctx context.Context, tx *sql.Tx, objectKey string, dc string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM temp_objects WHERE object_key = $1 and bucket_id = $2`, objectKey, dc)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if rowsAffected != 1 {
		return stacktrace.Propagate(fmt.Errorf("only one row should be affected not %d", rowsAffected), "")
	}
	return nil
}

func (repo *ObjectCleanupRepository) DoesTempObjectExist(ctx context.Context, objectKey string, uploadID string) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM temp_objects WHERE object_key = $1 AND upload_id = $2)`
	err := repo.DB.QueryRowContext(ctx, query, objectKey, uploadID).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, stacktrace.Propagate(err, "failed to check if temp object exists")
	}
	return exists, nil
}

func (repo *ObjectCleanupRepository) GetAndLockExpiredObjects() (*sql.Tx, []ente.TempObject, error) {
	tx, err := repo.DB.Begin()
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "")
	}

	transferred := false
	defer func() {
		if !transferred {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.Query(`
	SELECT object_key, is_multipart, upload_id, bucket_id, part_length FROM temp_objects
	WHERE expiration_time <= $1
	LIMIT 1000
	FOR UPDATE SKIP LOCKED
	`, time.Microseconds())

	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "")
	}

	defer rows.Close()
	tempObjects := make([]ente.TempObject, 0)
	for rows.Next() {
		var tempObject ente.TempObject
		var uploadID sql.NullString
		var bucketID sql.NullString
		var partLength sql.NullInt64
		err := rows.Scan(&tempObject.ObjectKey, &tempObject.IsMultipart, &uploadID, &bucketID, &partLength)
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "")
		}
		if tempObject.IsMultipart {
			tempObject.UploadID = uploadID.String
		}
		if bucketID.Valid {
			tempObject.BucketId = bucketID.String
		}
		if partLength.Valid {
			tempObject.PartLength = &partLength.Int64
		}
		tempObjects = append(tempObjects, tempObject)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, stacktrace.Propagate(err, "")
	}
	transferred = true
	return tx, tempObjects, nil
}

// Filtering on object_key alone (the primary key) also matches multipart rows
// whose upload_id is still NULL. A Drive row the cron has handled once is
// released, so pushing its expiry forward doesn't put it back in the quota sum.
func (repo *ObjectCleanupRepository) SetExpiryForTempObject(tx *sql.Tx, tempObject ente.TempObject, expirationTime int64) error {
	_, err := tx.Exec(`
		UPDATE temp_objects
		SET expiration_time = $1, reservation_released = reservation_released OR app IS NOT DISTINCT FROM 'drive'
		WHERE object_key = $2
		`, expirationTime, tempObject.ObjectKey)
	return stacktrace.Propagate(err, "")
}

func (repo *ObjectCleanupRepository) RemoveTempObject(tx *sql.Tx, tempObject ente.TempObject) error {
	_, err := tx.Exec(`
		DELETE FROM temp_objects WHERE object_key = $1
		`, tempObject.ObjectKey)
	return stacktrace.Propagate(err, "")
}
