package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type rateLimitBucket struct {
	count   int
	resetAt time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]rateLimitBucket
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{
		now:     now,
		buckets: make(map[string]rateLimitBucket),
	}
}

func (l *rateLimiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 || window <= 0 {
		return true
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.deleteExpiredLocked(now)
	bucket := l.buckets[key]
	if bucket.resetAt.IsZero() || !now.Before(bucket.resetAt) {
		l.buckets[key] = rateLimitBucket{count: 1, resetAt: now.Add(window)}
		return true
	}
	if bucket.count >= limit {
		return false
	}
	bucket.count++
	l.buckets[key] = bucket
	return true
}

func (l *rateLimiter) deleteExpiredLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if !now.Before(bucket.resetAt) {
			delete(l.buckets, key)
		}
	}
}

func rateLimitKey(req *http.Request, scope string) string {
	host := req.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		host = parsedHost
	}
	if host == "" {
		host = "unknown"
	}
	return scope + ":" + host
}
