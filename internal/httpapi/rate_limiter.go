package httpapi

import (
	"net/http"
	"net/netip"
	"strings"
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

func (r *Router) rateLimitKey(req *http.Request, scope string) string {
	return scope + ":" + clientAddress(req, r.trustedProxyCIDRs)
}

func clientAddress(req *http.Request, trustedProxies []netip.Prefix) string {
	remote, ok := parseRequestAddress(req.RemoteAddr)
	if !ok {
		return "unknown"
	}
	if !addressInPrefixes(remote, trustedProxies) {
		return remote.String()
	}

	forwarded := strings.Split(req.Header.Get("X-Forwarded-For"), ",")
	if len(forwarded) == 1 && strings.TrimSpace(forwarded[0]) == "" {
		return remote.String()
	}

	addresses := make([]netip.Addr, 0, len(forwarded))
	for _, raw := range forwarded {
		address, valid := parseRequestAddress(strings.TrimSpace(raw))
		if !valid {
			return remote.String()
		}
		addresses = append(addresses, address)
	}
	for index := len(addresses) - 1; index >= 0; index-- {
		if !addressInPrefixes(addresses[index], trustedProxies) {
			return addresses[index].String()
		}
	}
	return addresses[0].String()
}

func parseRequestAddress(raw string) (netip.Addr, bool) {
	if address, err := netip.ParseAddr(raw); err == nil {
		return address.Unmap(), true
	}
	addressPort, err := netip.ParseAddrPort(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return addressPort.Addr().Unmap(), true
}

func addressInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
