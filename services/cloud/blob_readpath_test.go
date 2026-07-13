package main

// Static guard for the Part 2 MinIO invariant: "the cloud path only ever
// handles the pre-signed URL, not bytes." The blob ciphertext must flow
// browser -> MinIO (pre-signed PUT) and MinIO -> printer (pre-signed GET),
// never *through* the cloud server. So no cloud code path may call a
// minio-go method that streams object bytes -- GetObject / FGetObject
// (read) or PutObject / FPutObject (write). The allowed surface is exactly
// what minioclient uses: Presigned{Get,Put}Object (URL generation only),
// StatObject (metadata for the blob-exists precheck), RemoveObject
// (post-delivery cleanup), BucketExists / MakeBucket.
//
// This is a build-failing guard (runs in `make ci`, no Docker needed),
// complementing the live round-trip in integration_minio_test.go. Like the
// zero-knowledge scanner, it self-tests that it catches a planted
// violation, so green means "tree is clean", not "detector is broken".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// byteStreamingBlobMethods are the minio-go calls that move object bytes
// through this process -- forbidden anywhere in the cloud tree.
var byteStreamingBlobMethods = map[string]bool{
	"GetObject":  true,
	"FGetObject": true,
	"PutObject":  true,
	"FPutObject": true,
}

// scanBlobByteAccess returns positions of any call to a byte-streaming blob
// method in the given file.
func scanBlobByteAccess(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if byteStreamingBlobMethods[sel.Sel.Name] {
			out = append(out, fset.Position(call.Pos()).String()+": calls "+sel.Sel.Name+" (cloud must never stream blob bytes)")
		}
		return true
	})
	return out
}

// TestInvariant_CloudNeverStreamsBlobBytes walks non-test .go files under
// the cloud tree and asserts none reads or writes object bytes directly.
func TestInvariant_CloudNeverStreamsBlobBytes(t *testing.T) {
	var violations []string
	fset := token.NewFileSet()
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		violations = append(violations, scanBlobByteAccess(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("cloud blob-byte invariant violated (only pre-signed URLs may carry blob bytes):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestInvariant_BlobByteScannerCatchesViolation is the guard's own guard.
func TestInvariant_BlobByteScannerCatchesViolation(t *testing.T) {
	const bad = `package p
import "context"
func read(c interface{ GetObject(context.Context, string) []byte }) {
	c.GetObject(context.Background(), "blobs/x") // must be flagged
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bad.go", bad, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := scanBlobByteAccess(fset, f); len(v) != 1 {
		t.Fatalf("scanner should flag the 1 planted GetObject call, got %d: %v", len(v), v)
	}
}
