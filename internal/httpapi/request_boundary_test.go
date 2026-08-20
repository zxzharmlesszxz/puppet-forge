package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
)

// Plaintext HTTP is required to verify that untrusted forwarded headers cannot change the request origin.
//
//goland:noinspection HttpUrlsUsage
func TestExternalRequestBoundaryIgnoresForwardedHeadersFromUntrustedClient(t *testing.T) {
	t.Parallel()

	router := &Router{trustForwardedHeaders: true, trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(httputil.ExternalBaseURL(req, "")))
	}))
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/modules", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-Host", "attacker.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "http://forge.example.com" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestExternalRequestBoundaryAcceptsValidatedHeadersFromTrustedProxy(t *testing.T) {
	t.Parallel()

	router := &Router{
		trustForwardedHeaders: true,
		trustedProxyCIDRs:     []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		allowedPublicHosts:    []string{"forge.example.com"},
	}
	handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(httputil.ExternalBaseURL(req, "")))
	}))
	req := httptest.NewRequest(http.MethodGet, "http://internal:8080/modules", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("Forwarded", `for=192.0.2.10;proto=https;host="forge.example.com"`)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "https://forge.example.com" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestExternalRequestBoundarySupportsDynamicIngressHostsFromTrustedNetworks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		remoteAddr string
	}{
		{name: "IPv4", remoteAddr: "192.0.2.10:1234"},
		{name: "IPv6", remoteAddr: "[2001:db8::10]:1234"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			router := &Router{
				trustForwardedHeaders: true,
				trustedProxyCIDRs: []netip.Prefix{
					netip.MustParsePrefix("0.0.0.0/0"),
					netip.MustParsePrefix("::/0"),
				},
			}
			handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = w.Write([]byte(httputil.ExternalBaseURL(req, "")))
			}))
			req := httptest.NewRequest(http.MethodGet, "http://internal:8080/auth/login", nil)
			req.RemoteAddr = test.remoteAddr
			req.Header.Set("X-Forwarded-Host", "dynamic.forge.example.com")
			req.Header.Set("X-Forwarded-Proto", "https")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || rec.Body.String() != "https://dynamic.forge.example.com" {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestExternalRequestBoundaryRejectsMalformedOrConflictingForwardedHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "malformed standard header", headers: map[string]string{"Forwarded": "not-a-pair"}},
		{name: "invalid proto", headers: map[string]string{"X-Forwarded-Proto": "javascript"}},
		{name: "invalid client address", headers: map[string]string{"X-Forwarded-For": "not-an-ip"}},
		{name: "conflicting hosts", headers: map[string]string{"Forwarded": "host=one.example.com", "X-Forwarded-Host": "two.example.com"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			router := &Router{trustForwardedHeaders: true, trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
			handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			req := httptest.NewRequest(http.MethodGet, "http://internal:8080/modules", nil)
			req.RemoteAddr = "10.1.2.3:1234"
			for name, value := range test.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

// Plaintext HTTP is part of the disallowed-host boundary scenario.
//
//goland:noinspection HttpUrlsUsage
func TestExternalRequestBoundaryRejectsDisallowedHost(t *testing.T) {
	t.Parallel()

	router := &Router{allowedPublicHosts: []string{"forge.example.com"}}
	handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodGet, "http://attacker.example.com/modules", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func FuzzValidateForwardedHeaders(f *testing.F) {
	f.Add(`for=192.0.2.10;proto=https;host="forge.example.com"`, "forge.example.com", "https", "192.0.2.10")
	f.Add("not-a-pair", "", "", "")
	f.Add("", "attacker.example.com", "javascript", "not-an-ip")
	f.Fuzz(func(t *testing.T, forwarded, host, proto, forwardedFor string) {
		req := httptest.NewRequest(http.MethodGet, "http://internal:8080/modules", nil)
		req.Header.Set("Forwarded", forwarded)
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Forwarded-Proto", proto)
		req.Header.Set("X-Forwarded-For", forwardedFor)
		_ = validateForwardedHeaders(req)
	})
}
