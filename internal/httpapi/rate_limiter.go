package httpapi

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const defaultRateLimiterMaxEntries = 10_000

type rateLimitBucket struct {
	key     string
	count   int
	resetAt time.Time
}

type rateLimitExpiryHeap []*rateLimitBucket

func (h *rateLimitExpiryHeap) Len() int { return len(*h) }

func (h *rateLimitExpiryHeap) Less(i, j int) bool {
	if (*h)[i].resetAt.Equal((*h)[j].resetAt) {
		return (*h)[i].key < (*h)[j].key
	}
	return (*h)[i].resetAt.Before((*h)[j].resetAt)
}

func (h *rateLimitExpiryHeap) Swap(i, j int) {
	(*h)[i], (*h)[j] = (*h)[j], (*h)[i]
}

func (h *rateLimitExpiryHeap) Push(value any) {
	bucket := value.(*rateLimitBucket)
	*h = append(*h, bucket)
}

func (h *rateLimitExpiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	bucket := old[last]
	old[last] = nil
	*h = old[:last]
	return bucket
}

type rateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	maxKeys int
	buckets map[string]*rateLimitBucket
	expiry  rateLimitExpiryHeap
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{
		now:     now,
		maxKeys: defaultRateLimiterMaxEntries,
		buckets: make(map[string]*rateLimitBucket),
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
	if bucket == nil {
		if l.maxKeys > 0 && len(l.buckets) >= l.maxKeys {
			l.removeNextExpiryLocked()
		}
		bucket = &rateLimitBucket{key: key, count: 1, resetAt: now.Add(window)}
		l.buckets[key] = bucket
		heap.Push(&l.expiry, bucket)
		return true
	}
	if bucket.count >= limit {
		return false
	}
	bucket.count++
	return true
}

func (l *rateLimiter) deleteExpiredLocked(now time.Time) {
	for len(l.expiry) > 0 && !now.Before(l.expiry[0].resetAt) {
		l.removeNextExpiryLocked()
	}
}

func (l *rateLimiter) removeNextExpiryLocked() {
	if len(l.expiry) == 0 {
		return
	}
	bucket := heap.Pop(&l.expiry).(*rateLimitBucket)
	delete(l.buckets, bucket.key)
}

func (r *Router) rateLimitKey(req *http.Request, scope string) string {
	return scope + ":" + clientAddress(req, r.trustedProxyCIDRs)
}

func (r *Router) rateLimited(scope string, limit int, window time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.rateLimiter.Allow(r.rateLimitKey(req, scope), limit, window) {
			writeError(w, http.StatusTooManyRequests, errors.New("too many requests"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Router) allowSharedRateLimit(req *http.Request, scope string, limit int, window time.Duration) (bool, error) {
	keyDigest := sha256.Sum256([]byte(r.rateLimitKey(req, scope)))
	key := scope + ":" + hex.EncodeToString(keyDigest[:])
	return r.modules.ConsumeRateLimit(req.Context(), key, limit, window, r.rateLimiter.now().UTC())
}

func (r *Router) requireSharedRateLimit(w http.ResponseWriter, req *http.Request, scope string, limit int, window time.Duration, message string) bool {
	allowed, err := r.allowSharedRateLimit(req, scope, limit, window)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("rate limit service is unavailable"))
		return false
	}
	if !allowed {
		writeError(w, http.StatusTooManyRequests, errors.New(message))
		return false
	}
	return true
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
