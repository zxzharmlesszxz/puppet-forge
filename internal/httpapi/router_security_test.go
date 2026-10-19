package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
)

func TestReadmeImageSourcesValidateExactHosts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		hosts []string
		want  string
	}{
		{"empty", nil, "'self' data:"},
		{"blank entries", []string{"", " "}, "'self' data:"},
		{"normalized", []string{" IMG.Shields.IO ", "img.shields.io", "badges.example.com"}, "'self' data: https://img.shields.io https://badges.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readmeImageSources(tc.hosts)
			if err != nil || got != tc.want {
				t.Fatalf("readmeImageSources() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, host := range []string{
		"*", "*.example.com", "https://img.shields.io", "http://img.shields.io",
		"example.com/path", "example.com:8443", "example.com?key=x", "example.com#fragment",
		"example.com; script-src *", "'unsafe-inline'", "example.com\r\nimg-src *",
		"example..com", "-example.com", "example_.com", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "com",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := readmeImageSources([]string{host}); err == nil {
				t.Fatal("expected invalid README image host to be rejected")
			}
		})
	}
}

func TestRouterUsesConfiguredReadmeImageSources(t *testing.T) {
	t.Parallel()
	st := newHTTPAPITestStore(t)
	cfg := RouterConfig{
		Modules:                 service.NewModuleService(st, testArtifactStorage{}, "modules", nil),
		ManageSessionSecret:     "image-test-manage-session-secret-32-bytes",
		ReadmeImageAllowedHosts: []string{"img.shields.io", "coveralls.io", "s3.amazonaws.com", "www.puppetmodule.info"},
	}
	handler, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	for _, path := range []string{"/manage/login", "/healthz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		policy := recorder.Header().Get("Content-Security-Policy")
		if path == "/manage/login" {
			if !strings.Contains(policy, "img-src 'self' data: https://img.shields.io https://coveralls.io https://s3.amazonaws.com https://www.puppetmodule.info;") {
				t.Errorf("browser CSP misses exact HTTPS image sources: %q", policy)
			}
		} else if strings.Contains(policy, "img-src") {
			t.Errorf("machine endpoint received browser image permissions: %q", policy)
		}
	}
	cfg.ReadmeImageAllowedHosts = []string{"example.com; script-src *"}
	if _, err := NewRouter(cfg); err == nil {
		t.Fatal("NewRouter must reject an invalid image allowlist")
	}
}
