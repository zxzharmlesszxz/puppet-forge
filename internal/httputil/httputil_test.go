package httputil

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFirstHeaderValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"single", "single"},
		{"  spaced  ", "spaced"},
		{"first, second, third", "first"},
		{" first ,second", "first"},
		{",leading-comma", ""},
	}
	for _, tt := range tests {
		got := FirstHeaderValue(tt.input)
		if got != tt.want {
			t.Errorf("FirstHeaderValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestForwardedParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		key   string
		want  string
	}{
		{"", "proto", ""},
		{"for=10.0.0.1;proto=https;host=example.com", "proto", "https"},
		{"for=10.0.0.1;proto=https;host=example.com", "host", "example.com"},
		{"for=10.0.0.1;proto=https;host=example.com", "for", "10.0.0.1"},
		{"for=10.0.0.1;proto=https;host=example.com", "nonexistent", ""},
		{`for=10.0.0.1;proto="https";host=example.com`, "proto", "https"},
		{"for=10.0.0.1; proto=https ; host=example.com", "proto", "https"},
	}
	for _, tt := range tests {
		got := ForwardedParam(tt.value, tt.key)
		if got != tt.want {
			t.Errorf("ForwardedParam(%q, %q) = %q, want %q", tt.value, tt.key, got, tt.want)
		}
	}
}

func TestForwardedScheme(t *testing.T) {
	t.Parallel()

	t.Run("nil request returns http", func(t *testing.T) {
		t.Parallel()
		if got := ForwardedScheme(nil); got != "http" {
			t.Fatalf("ForwardedScheme(nil) = %q, want http", got)
		}
	})

	t.Run("X-Forwarded-Proto", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		if got := ForwardedScheme(req); got != "https" {
			t.Fatalf("ForwardedScheme() = %q, want https", got)
		}
	})

	t.Run("Forwarded header", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		req.Header.Set("Forwarded", "for=10.0.0.1;proto=https")
		if got := ForwardedScheme(req); got != "https" {
			t.Fatalf("ForwardedScheme() = %q, want https", got)
		}
	})

	t.Run("X-Forwarded-Proto takes priority over Forwarded", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Forwarded", "proto=http")
		if got := ForwardedScheme(req); got != "https" {
			t.Fatalf("ForwardedScheme() = %q, want https", got)
		}
	})

	t.Run("TLS", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		req.TLS = &tls.ConnectionState{}
		if got := ForwardedScheme(req); got != "https" {
			t.Fatalf("ForwardedScheme() = %q, want https", got)
		}
	})

	t.Run("fallback to http", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		if got := ForwardedScheme(req); got != "http" {
			t.Fatalf("ForwardedScheme() = %q, want http", got)
		}
	})
}

func TestExternalBaseURL(t *testing.T) {
	t.Parallel()

	t.Run("nil request returns fallback", func(t *testing.T) {
		t.Parallel()
		if got := ExternalBaseURL(nil, "https://forge.example.com"); got != "https://forge.example.com" {
			t.Fatalf("ExternalBaseURL(nil) = %q", got)
		}
	})

	t.Run("nil request with empty fallback", func(t *testing.T) {
		t.Parallel()
		if got := ExternalBaseURL(nil, ""); got != "" {
			t.Fatalf("ExternalBaseURL(nil, '') = %q, want empty", got)
		}
	})

	t.Run("X-Forwarded-Proto and X-Forwarded-Host", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "forge.example.com")
		if got := ExternalBaseURL(req, ""); got != "https://forge.example.com" {
			t.Fatalf("ExternalBaseURL() = %q, want https://forge.example.com", got)
		}
	})

	t.Run("Forwarded standard header", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
		req.Header.Set("Forwarded", `for=10.0.0.1;proto=https;host="forge.alt.example.com"`)
		if got := ExternalBaseURL(req, ""); got != "https://forge.alt.example.com" {
			t.Fatalf("ExternalBaseURL() = %q, want https://forge.alt.example.com", got)
		}
	})

	t.Run("request Host", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://forge.127.0.0.1.nip.io:8080/manage", nil)
		if got := ExternalBaseURL(req, ""); got != "http://forge.127.0.0.1.nip.io:8080" {
			t.Fatalf("ExternalBaseURL() = %q, want http://forge.127.0.0.1.nip.io:8080", got)
		}
	})

	t.Run("fallback when no host headers", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
		req.Host = ""
		if got := ExternalBaseURL(req, "https://forge.example.com/"); got != "https://forge.example.com" {
			t.Fatalf("ExternalBaseURL() = %q, want https://forge.example.com", got)
		}
	})

	t.Run("fallback with trailing slash trimmed", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
		req.Host = ""
		if got := ExternalBaseURL(req, "https://forge.example.com/"); got != "https://forge.example.com" {
			t.Fatalf("ExternalBaseURL() = %q, want https://forge.example.com", got)
		}
	})
}

func TestReleaseVersionFromSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		moduleSlug  string
		releaseSlug string
		want        string
	}{
		{"puppetlabs-stdlib", "puppetlabs-stdlib-1.0.0", "1.0.0"},
		{"puppetlabs-stdlib", "/v3/files/puppetlabs-stdlib-1.0.0.tar.gz", "1.0.0"},
		{"puppetlabs-stdlib", "", ""},
		{"", "puppetlabs-stdlib-1.0.0", ""},
		{"", "", ""},
		{"puppetlabs-stdlib", "other-module-1.0.0", ""},
	}
	for _, tt := range tests {
		got := ReleaseVersionFromSlug(tt.moduleSlug, tt.releaseSlug)
		if got != tt.want {
			t.Errorf("ReleaseVersionFromSlug(%q, %q) = %q, want %q", tt.moduleSlug, tt.releaseSlug, got, tt.want)
		}
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a    string
		b    string
		want string
	}{
		{"a/", "/b", "a/b"},
		{"a", "b", "a/b"},
		{"a/", "b", "a/b"},
		{"a", "/b", "a/b"},
		{"", "b", "/b"},
		{"a", "", "a/"},
		{"", "", "/"},
		{"a//", "//b", "a///b"},
	}
	for _, tt := range tests {
		got := SingleJoiningSlash(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("SingleJoiningSlash(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}
