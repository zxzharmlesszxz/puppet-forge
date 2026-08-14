package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestRateLimiterBlocksAfterLimit(t *testing.T) {
	t.Parallel()

	now := time.Now()
	limiter := newRateLimiter(func() time.Time { return now })
	key := "login:127.0.0.1"

	if !limiter.Allow(key, 2, time.Minute) {
		t.Fatal("first request should be allowed")
	}
	if !limiter.Allow(key, 2, time.Minute) {
		t.Fatal("second request should be allowed")
	}
	if limiter.Allow(key, 2, time.Minute) {
		t.Fatal("third request should be blocked")
	}

	now = now.Add(time.Minute)
	if !limiter.Allow(key, 2, time.Minute) {
		t.Fatal("request after window reset should be allowed")
	}
}

func TestRateLimitKeyIgnoresSpoofableForwardedFor(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	req.Header.Set("X-Forwarded-For", "192.0.2.10, 10.0.0.10")
	router := &Router{}

	if got := router.rateLimitKey(req, "manage-login"); got != "manage-login:10.0.0.10" {
		t.Fatalf("unexpected rate limit key: %s", got)
	}
}

func TestRateLimitKeyUsesForwardedClientFromTrustedProxy(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	router := &Router{trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}

	if got := router.rateLimitKey(req, "manage-login"); got != "manage-login:192.0.2.10" {
		t.Fatalf("unexpected rate limit key: %s", got)
	}
}

func TestRateLimitKeySkipsTrustedForwardingChain(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	req.Header.Set("X-Forwarded-For", "192.0.2.10, 172.16.1.20, 10.1.2.3")
	router := &Router{trustedProxyCIDRs: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
	}}

	if got := router.rateLimitKey(req, "manage-login"); got != "manage-login:192.0.2.10" {
		t.Fatalf("unexpected rate limit key: %s", got)
	}
}

func TestRateLimitKeyFallsBackForMalformedForwardedFor(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	req.RemoteAddr = "[2001:db8::10]:12345"
	req.Header.Set("X-Forwarded-For", "not-an-address")
	router := &Router{trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}}

	if got := router.rateLimitKey(req, "manage-login"); got != "manage-login:2001:db8::10" {
		t.Fatalf("unexpected rate limit key: %s", got)
	}
}

func TestRateLimiterCleansExpiredBucketsOnAllow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	limiter := newRateLimiter(func() time.Time { return now })

	if !limiter.Allow("login:127.0.0.1", 1, time.Minute) {
		t.Fatal("first request should be allowed")
	}
	now = now.Add(2 * time.Minute)
	if !limiter.Allow("publish:127.0.0.1", 1, time.Minute) {
		t.Fatal("request for another key should be allowed")
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, ok := limiter.buckets["login:127.0.0.1"]; ok {
		t.Fatal("expected expired bucket to be removed")
	}
	if _, ok := limiter.buckets["publish:127.0.0.1"]; !ok {
		t.Fatal("expected current bucket to remain")
	}
	if len(limiter.expiry) != 1 {
		t.Fatalf("expiry heap size = %d, want 1", len(limiter.expiry))
	}
}

func TestRateLimiterEvictsNextExpiryAndAdmitsNewKeysAtCapacity(t *testing.T) {
	t.Parallel()

	now := time.Now()
	limiter := newRateLimiter(func() time.Time { return now })
	limiter.maxKeys = 2
	if !limiter.Allow("login:192.0.2.1", 2, time.Minute) || !limiter.Allow("login:192.0.2.2", 2, 2*time.Minute) {
		t.Fatal("requests up to capacity should be allowed")
	}
	if !limiter.Allow("login:192.0.2.3", 2, time.Minute) {
		t.Fatal("new key beyond capacity should be admitted after eviction")
	}
	if _, ok := limiter.buckets["login:192.0.2.1"]; ok {
		t.Fatal("bucket with the nearest reset should be evicted")
	}
	if len(limiter.buckets) != 2 || len(limiter.expiry) != 2 {
		t.Fatalf("bounded state sizes = buckets %d, heap %d; want 2, 2", len(limiter.buckets), len(limiter.expiry))
	}
}

func TestRateLimiterStateRemainsBoundedUnderKeyChurn(t *testing.T) {
	t.Parallel()

	limiter := newRateLimiter(time.Now)
	limiter.maxKeys = 8
	for index := range 1_000 {
		if !limiter.Allow("publish:"+strconv.Itoa(index), 1, time.Hour) {
			t.Fatalf("new key %d was rejected", index)
		}
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.buckets) != limiter.maxKeys || len(limiter.expiry) != limiter.maxKeys {
		t.Fatalf("bounded state sizes = buckets %d, heap %d; want %d", len(limiter.buckets), len(limiter.expiry), limiter.maxKeys)
	}
}

func TestSharedRateLimitIsAppliedAcrossRouterInstances(t *testing.T) {
	t.Parallel()

	backend, err := store.NewSQLiteStore("sqlite://" + filepath.Join(t.TempDir(), "rate-limit.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(backend.Close)
	moduleService := service.NewModuleService(backend, nil, "modules", nil)
	now := time.Now().UTC()
	first := &Router{modules: moduleService, rateLimiter: newRateLimiter(func() time.Time { return now })}
	second := &Router{modules: moduleService, rateLimiter: newRateLimiter(func() time.Time { return now })}
	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	req.RemoteAddr = "192.0.2.10:12345"

	for attempt, router := range []*Router{first, second, first} {
		allowed, err := router.allowSharedRateLimit(req, "manage-login", 2, time.Minute)
		if err != nil {
			t.Fatalf("allowSharedRateLimit(attempt %d) error = %v", attempt, err)
		}
		if allowed != (attempt < 2) {
			t.Fatalf("allowSharedRateLimit(attempt %d) = %v", attempt, allowed)
		}
	}
}

func TestSharedRateLimitFailsClosedWithoutStore(t *testing.T) {
	t.Parallel()

	router := &Router{
		modules:     service.NewModuleService(nil, nil, "modules", nil),
		rateLimiter: newRateLimiter(time.Now),
	}
	req := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	response := httptest.NewRecorder()

	if router.requireSharedRateLimit(response, req, "manage-login", 2, time.Minute, "too many requests") {
		t.Fatal("requireSharedRateLimit() allowed a request without shared storage")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}
