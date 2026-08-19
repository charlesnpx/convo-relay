package blobstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPutDeduplicatesAndOpenDetectsCorruption(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, Limits{})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	first, err := store.PutMediaType(bytes.NewReader([]byte("same bytes")), "text/plain")
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	second, err := store.PutMediaType(bytes.NewReader([]byte("same bytes")), "text/plain")
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if !first.Equal(second) {
		t.Fatalf("deduplicated refs differ: %#v != %#v", first, second)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("physical blob count = %d, want 1", len(listed))
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", SHA256Directory, first.SHA256), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	reader, err := store.Open(first)
	if err != nil {
		t.Fatalf("open corrupted blob: %v", err)
	}
	_, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	var mismatch *DigestMismatchError
	if !errors.As(readErr, &mismatch) && !errors.As(closeErr, &mismatch) {
		t.Fatalf("corruption returned read=%v close=%v, want DigestMismatchError", readErr, closeErr)
	}
}

func TestLimitsAreTypedAndSweepDoesNotDelete(t *testing.T) {
	perBlob, err := New(t.TempDir(), Limits{MaxBlobBytes: 3})
	if err != nil {
		t.Fatalf("new per-blob store: %v", err)
	}
	_, err = perBlob.Put(bytes.NewReader([]byte("four")))
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Scope != "per-blob bytes" {
		t.Fatalf("per-blob put error = %v, want typed per-blob LimitError", err)
	}

	root := t.TempDir()
	total, err := New(root, Limits{MaxTotalBytes: 5})
	if err != nil {
		t.Fatalf("new total store: %v", err)
	}
	ref, err := total.Put(bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatalf("first total put: %v", err)
	}
	_, err = total.Put(bytes.NewReader([]byte("def")))
	if !errors.As(err, &limit) || limit.Scope != "total bytes" {
		t.Fatalf("total put error = %v, want typed total LimitError", err)
	}
	report, err := total.Sweep(nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.Unreferenced) != 1 || report.Unreferenced[0].SHA256 != ref.SHA256 {
		t.Fatalf("sweep report = %#v, want unreferenced %s", report, ref.SHA256)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", SHA256Directory, ref.SHA256)); err != nil {
		t.Fatalf("sweep deleted blob: %v", err)
	}
}
