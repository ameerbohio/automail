//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"automail/cloud/minioclient"
)

// httpPut / httpGet drive the pre-signed URLs exactly as the browser (PUT)
// and printer (GET) would -- raw HTTP to MinIO, no cloud server in the
// path. That is the property Part 2 asserts: the blob bytes never traverse
// the cloud.
func httpPut(t *testing.T, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT to presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d, want 200; body: %s", resp.StatusCode, msg)
	}
}

func httpGet(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec -- presigned URL from the test's own MinIO
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET status = %d, want 200; body: %s", resp.StatusCode, msg)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	return got
}

// TestIntegration_PresignedPutGetRoundTrip proves the real MinIO seam: a
// pre-signed PUT uploads ciphertext directly, a pre-signed GET returns the
// exact bytes, BlobExists reflects presence, and RemoveBlob (the
// post-delivery cleanup) actually deletes it. minio-go against a fake can't
// prove presign signing or that the server honors the URLs.
func TestIntegration_PresignedPutGetRoundTrip(t *testing.T) {
	client, _ := startMinio(t)
	ctx := context.Background()

	if err := minioclient.EnsureBucket(ctx, client); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// Opaque ciphertext -- the cloud never learns what this is.
	ciphertext := []byte("\x00\x01\x02 this-is-encrypted-pdf-bytes \xfe\xff")

	uploadURL, blobRef, err := minioclient.PresignedUploadURL(ctx, client, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignedUploadURL: %v", err)
	}
	httpPut(t, uploadURL, ciphertext)

	exists, err := minioclient.BlobExists(ctx, client, blobRef)
	if err != nil {
		t.Fatalf("BlobExists after upload: %v", err)
	}
	if !exists {
		t.Fatal("BlobExists = false after a successful upload")
	}

	readURL, err := minioclient.PresignedReadURL(ctx, client, blobRef, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignedReadURL: %v", err)
	}
	got := httpGet(t, readURL)
	if !bytes.Equal(got, ciphertext) {
		t.Fatalf("round-tripped bytes = %q, want %q", got, ciphertext)
	}

	// Post-delivery cleanup removes the ciphertext.
	if err := minioclient.RemoveBlob(ctx, client, blobRef); err != nil {
		t.Fatalf("RemoveBlob: %v", err)
	}
	exists, err = minioclient.BlobExists(ctx, client, blobRef)
	if err != nil {
		t.Fatalf("BlobExists after remove: %v", err)
	}
	if exists {
		t.Fatal("BlobExists = true after RemoveBlob")
	}
}

// TestIntegration_BlobExistsMissingIsCleanFalse confirms the 422
// INVALID_BLOB_REF precheck: a never-uploaded ref reports absent without an
// error (NoSuchKey is mapped to false, not surfaced as a failure).
func TestIntegration_BlobExistsMissingIsCleanFalse(t *testing.T) {
	client, _ := startMinio(t)
	ctx := context.Background()
	if err := minioclient.EnsureBucket(ctx, client); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	exists, err := minioclient.BlobExists(ctx, client, "blobs/never-uploaded")
	if err != nil {
		t.Fatalf("BlobExists on missing ref should not error: %v", err)
	}
	if exists {
		t.Fatal("BlobExists = true for a ref that was never uploaded")
	}
}

// TestIntegration_TornDownContainerFailsCleanly satisfies the Part 2 Verify
// line: killing a dependency mid-test yields a clean, explained failure --
// not a hang. We terminate MinIO, then a bounded-context call must return
// an error promptly (a connection failure), so a dead dependency surfaces
// fast instead of wedging the caller's goroutine.
func TestIntegration_TornDownContainerFailsCleanly(t *testing.T) {
	client, ctr := startMinio(t)
	ctx := context.Background()
	if err := minioclient.EnsureBucket(ctx, client); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// Kill the container out from under the client.
	if err := ctr.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate minio: %v", err)
	}

	// A call with a bounded context must error quickly, not hang. The outer
	// timer is the "not a hang" assertion; the inner context bounds the call.
	done := make(chan error, 1)
	go func() {
		callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := minioclient.BlobExists(callCtx, client, "blobs/anything")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("BlobExists succeeded against a terminated MinIO; expected a connection error")
		}
		t.Logf("got expected clean failure after teardown: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("BlobExists hung after the container was torn down -- expected a prompt error")
	}
}
