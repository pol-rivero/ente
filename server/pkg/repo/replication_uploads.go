package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ente/stacktrace"
)

type ReplicationUpload struct {
	ObjectKey  string
	DestDC     string
	UploadID   string
	PartSize   int64
	SourceETag string
}

type ReplicationUploadsRepository struct {
	DB *sql.DB
}

func (repo *ReplicationUploadsRepository) Get(ctx context.Context, objectKey string, destDC string) (*ReplicationUpload, error) {
	return repo.getOne(ctx, `
	SELECT object_key, dest_dc, upload_id, part_size, source_etag FROM replication_uploads
	WHERE object_key = $1 AND dest_dc = $2`, objectKey, destDC)
}

func (repo *ReplicationUploadsRepository) GetByUploadID(ctx context.Context, objectKey string, uploadID string) (*ReplicationUpload, error) {
	return repo.getOne(ctx, `
	SELECT object_key, dest_dc, upload_id, part_size, source_etag FROM replication_uploads
	WHERE object_key = $1 AND upload_id = $2
	LIMIT 1`, objectKey, uploadID)
}

func (repo *ReplicationUploadsRepository) getOne(ctx context.Context, query string, args ...any) (*ReplicationUpload, error) {
	var u ReplicationUpload
	err := repo.DB.QueryRowContext(ctx, query, args...).Scan(&u.ObjectKey, &u.DestDC, &u.UploadID, &u.PartSize, &u.SourceETag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	return &u, nil
}

func (repo *ReplicationUploadsRepository) ListForObject(ctx context.Context, objectKey string) ([]ReplicationUpload, error) {
	return repo.list(ctx, `
	SELECT object_key, dest_dc, upload_id, part_size, source_etag FROM replication_uploads
	WHERE object_key = $1`, objectKey)
}

// Rows created before the cutoff (microseconds) whose object no longer has an
// object_copies row.
func (repo *ReplicationUploadsRepository) ListAbandoned(ctx context.Context, createdBefore int64, limit int) ([]ReplicationUpload, error) {
	return repo.list(ctx, `
	SELECT r.object_key, r.dest_dc, r.upload_id, r.part_size, r.source_etag FROM replication_uploads r
	WHERE r.created_at < $1
		AND NOT EXISTS (SELECT 1 FROM object_copies c WHERE c.object_key = r.object_key)
	LIMIT $2`, createdBefore, limit)
}

func (repo *ReplicationUploadsRepository) list(ctx context.Context, query string, args ...any) ([]ReplicationUpload, error) {
	rows, err := repo.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, stacktrace.Propagate(err, "")
	}
	defer rows.Close()
	var uploads []ReplicationUpload
	for rows.Next() {
		var u ReplicationUpload
		if err := rows.Scan(&u.ObjectKey, &u.DestDC, &u.UploadID, &u.PartSize, &u.SourceETag); err != nil {
			return nil, stacktrace.Propagate(err, "")
		}
		uploads = append(uploads, u)
	}
	return uploads, stacktrace.Propagate(rows.Err(), "")
}

func (repo *ReplicationUploadsRepository) Insert(ctx context.Context, u ReplicationUpload) (bool, error) {
	res, err := repo.DB.ExecContext(ctx, `
	INSERT INTO replication_uploads (object_key, dest_dc, upload_id, part_size, source_etag)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (object_key, dest_dc) DO NOTHING`, u.ObjectKey, u.DestDC, u.UploadID, u.PartSize, u.SourceETag)
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, stacktrace.Propagate(err, "")
	}
	return n == 1, nil
}

func (repo *ReplicationUploadsRepository) Delete(ctx context.Context, objectKey string, destDC string, uploadID string) error {
	_, err := repo.DB.ExecContext(ctx, `
	DELETE FROM replication_uploads WHERE object_key = $1 AND dest_dc = $2 AND upload_id = $3`,
		objectKey, destDC, uploadID)
	return stacktrace.Propagate(err, "")
}
