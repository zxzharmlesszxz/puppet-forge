package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestGCSStoragePublicURL(t *testing.T) {
	t.Parallel()

	storage := NewGCSStorage(nil, "forge-artifacts", "project")
	got := storage.PublicURL("/modules/teamname/apache/1.2.3/archive.tar.gz")
	want := "https://storage.googleapis.com/forge-artifacts/modules/teamname/apache/1.2.3/archive.tar.gz"
	if got != want {
		t.Fatalf("unexpected public URL:\nwant %s\n got %s", want, got)
	}
}

func TestObjectWriterErrorIncludesWriteAndCloseErrors(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")

	err := objectWriterError(writeErr, closeErr)
	if !errors.Is(err, writeErr) {
		t.Fatalf("expected write error in chain, got %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("expected close error in chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "write object") {
		t.Fatalf("expected write context, got %v", err)
	}
	if !strings.Contains(err.Error(), "close object writer") {
		t.Fatalf("expected close context, got %v", err)
	}
}
