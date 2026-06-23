package minioclient

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const bucket = "automail"

// EnsureBucket creates the bucket if it doesn't exist yet. Called once at
// startup -- the prototype has exactly one bucket for every blob.
func EnsureBucket(ctx context.Context, client *minio.Client) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
}

// PresignedUploadURL generates a pre-signed PUT URL for a new ciphertext
// blob. The browser uploads directly to MinIO -- the cloud server never
// receives the blob (plans/09-api-contracts.md POST /jobs/upload-url).
func PresignedUploadURL(ctx context.Context, client *minio.Client, ttl time.Duration) (uploadURL string, blobRef string, err error) {
	blobRef = "blobs/" + uuid.New().String()
	u, err := client.PresignedPutObject(ctx, bucket, blobRef, ttl)
	if err != nil {
		return "", "", err
	}
	return u.String(), blobRef, nil
}

// PresignedReadURL generates a pre-signed GET URL handed to the printer
// in a dispatch frame (Phase 4+). Not used by Phase 2's handlers yet, but
// declared here since blob existence checking below needs the same client.
func PresignedReadURL(ctx context.Context, client *minio.Client, blobRef string, ttl time.Duration) (string, error) {
	u, err := client.PresignedGetObject(ctx, bucket, blobRef, ttl, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// BlobExists checks a blob_ref was actually uploaded before POST /jobs
// accepts it -- the 422 INVALID_BLOB_REF precheck in plans/09-api-contracts.md.
func BlobExists(ctx context.Context, client *minio.Client, blobRef string) (bool, error) {
	_, err := client.StatObject(ctx, bucket, blobRef, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
