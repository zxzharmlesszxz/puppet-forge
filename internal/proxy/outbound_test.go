package proxy

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestNewForgeProxyRejectsUnsafeUpstreamURLs(t *testing.T) {
	t.Parallel()

	for _, rawURL := range []string{
		"file:///etc/passwd",
		"http://127.0.0.1:8080",
		"http://100.64.0.1:8080",
		"http://169.254.169.254/latest/meta-data",
		"http://192.0.2.1:8080",
		"http://[::1]:8080",
		"http://localhost:8080",
	} {
		if _, err := NewForgeProxy(rawURL, 0, 1024, nil, "cache"); err == nil {
			t.Fatalf("NewForgeProxy(%q) unexpectedly succeeded", rawURL)
		}
	}
}

func TestCustomHTTPClientDoesNotImplicitlyAllowPrivateUpstream(t *testing.T) {
	t.Parallel()

	client := &http.Client{}
	if _, err := NewForgeProxy("http://127.0.0.1:8080", 0, 1024, nil, "cache", WithHTTPClient(client)); err == nil {
		t.Fatal("custom HTTP client implicitly allowed a private upstream")
	}
	if _, err := NewForgeProxy("http://127.0.0.1:8080", 0, 1024, nil, "cache", WithHTTPClient(client), WithPrivateNetworks()); err != nil {
		t.Fatalf("explicit private-network option was rejected: %v", err)
	}
}

func TestUpstreamRedirectPolicyRejectsProhibitedTarget(t *testing.T) {
	t.Parallel()

	client := newUpstreamHTTPClient(false)
	request := &http.Request{URL: mustParseURL(t, "http://169.254.169.254/latest/meta-data")}
	if err := client.CheckRedirect(request, []*http.Request{{}}); err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("CheckRedirect() error = %v, want prohibited address", err)
	}
}

func TestUpstreamRedirectPolicyLimitsRedirectChain(t *testing.T) {
	t.Parallel()

	client := newUpstreamHTTPClient(false)
	request := &http.Request{URL: mustParseURL(t, "https://example.com/next")}
	via := make([]*http.Request, maxUpstreamRedirects)
	if err := client.CheckRedirect(request, via); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("CheckRedirect() error = %v, want redirect limit", err)
	}
}

func TestOutboundPolicyRejectsPrivateResolvedAddresses(t *testing.T) {
	t.Parallel()

	policy := outboundPolicy{resolver: netResolverStub{addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}}
	err := policy.validateURL(context.Background(), mustParseURL(t, "https://forge.example.test"))
	if err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("validateURL() error = %v, want prohibited address", err)
	}
}

type netResolverStub struct {
	addresses []netip.Addr
}

func (s netResolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), s.addresses...), nil
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
	}
	return parsed
}
