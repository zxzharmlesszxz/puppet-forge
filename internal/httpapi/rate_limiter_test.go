package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

	if got := rateLimitKey(req, "manage-login"); got != "manage-login:10.0.0.10" {
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
}
