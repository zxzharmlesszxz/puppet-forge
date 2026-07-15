package storage

import (
	"net/url"
	"testing"

	"puppet-forge/internal/httputil"
)

func TestS3StoragePublicURLPathStyle(t *testing.T) {
	t.Parallel()

	storage := &S3Storage{
		bucket:        "forge-artifacts",
		publicBaseURL: mustURL(t, "http://minio:9000/base"),
		pathStyle:     true,
	}

	got := storage.PublicURL("/modules/teamname/apache/1.2.3/archive.tar.gz")
	want := "http://minio:9000/base/forge-artifacts/modules/teamname/apache/1.2.3/archive.tar.gz"
	if got != want {
		t.Fatalf("unexpected public URL:\nwant %s\n got %s", want, got)
	}
}

func TestS3StoragePublicURLVirtualHostStyle(t *testing.T) {
	t.Parallel()

	storage := &S3Storage{
		bucket:        "forge-artifacts",
		publicBaseURL: mustURL(t, "https://s3.example.com"),
		pathStyle:     false,
	}

	got := storage.PublicURL("modules/teamname/apache/1.2.3/archive.tar.gz")
	want := "https://forge-artifacts.s3.example.com/modules/teamname/apache/1.2.3/archive.tar.gz"
	if got != want {
		t.Fatalf("unexpected public URL:\nwant %s\n got %s", want, got)
	}
}

func TestS3StoragePublicURLReturnsEmptyForNilBaseURL(t *testing.T) {
	t.Parallel()

	storage := &S3Storage{bucket: "forge-artifacts"}
	if got := storage.PublicURL("/modules/test"); got != "" {
		t.Fatalf("expected empty URL, got %q", got)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b, want string
	}{
		{"https://example.com/", "/path", "https://example.com/path"},
		{"https://example.com", "path", "https://example.com/path"},
		{"https://example.com/", "path", "https://example.com/path"},
		{"https://example.com", "/path", "https://example.com/path"},
		{"", "path", "/path"},
		{"base/", "", "base/"},
	}

	for _, tt := range tests {
		got := httputil.SingleJoiningSlash(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return parsed
}
