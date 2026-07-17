package proxy

import (
	"testing"
	"time"
)

func TestResponseCacheGetReturnsEntryBeforeExpiry(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	entry := CacheEntry{
		StatusCode: 200,
		Body:       []byte("ok"),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	cache.Set("key", entry)

	got, ok := cache.Get("key", time.Now())
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.StatusCode != 200 || string(got.Body) != "ok" {
		t.Fatalf("unexpected entry: %+v", got)
	}
}

func TestResponseCacheGetReturnsFalseForExpiredEntry(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	cache.Set("key", CacheEntry{
		StatusCode: 200,
		Body:       []byte("stale"),
		ExpiresAt:  time.Now().Add(-time.Second),
	})

	_, ok := cache.Get("key", time.Now())
	if ok {
		t.Fatal("expected cache miss for expired entry")
	}
}

func TestResponseCacheGetReturnsFalseForMissingKey(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	_, ok := cache.Get("nonexistent", time.Now())
	if ok {
		t.Fatal("expected cache miss for missing key")
	}
}

func TestResponseCacheGetStaleReturnsRecentlyExpiredEntry(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	now := time.Now()
	cache.Set("key", CacheEntry{
		StatusCode: 200,
		Body:       []byte("stale"),
		ExpiresAt:  now.Add(-time.Second),
	})

	_, ok := cache.GetStale("key", now, time.Minute)
	if !ok {
		t.Fatal("expected stale entry to be returned")
	}
}

func TestResponseCacheGetStaleRejectsTooOldEntry(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	now := time.Now()
	cache.Set("key", CacheEntry{
		StatusCode: 200,
		Body:       []byte("stale"),
		ExpiresAt:  now.Add(-time.Hour),
	})

	_, ok := cache.GetStale("key", now, time.Minute)
	if ok {
		t.Fatal("expected too-old stale entry to be rejected")
	}
}

func TestResponseCacheGetStaleDisabledByZeroTTL(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	now := time.Now()
	cache.Set("key", CacheEntry{
		StatusCode: 200,
		Body:       []byte("stale"),
		ExpiresAt:  now.Add(-time.Second),
	})

	_, ok := cache.GetStale("key", now, 0)
	if ok {
		t.Fatal("expected stale fallback to be disabled")
	}
}

func TestResponseCacheEvictsExpiredEntriesFirst(t *testing.T) {
	t.Parallel()

	cache := &ResponseCache{
		entries:    make(map[string]CacheEntry),
		maxEntries: 2,
	}

	cache.Set("fresh", CacheEntry{
		Body:      []byte("fresh"),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	cache.Set("expired", CacheEntry{
		Body:      []byte("expired"),
		ExpiresAt: time.Now().Add(-time.Second),
	})

	// third Set triggers eviction; expired entry should be removed
	cache.Set("new", CacheEntry{
		Body:      []byte("new"),
		ExpiresAt: time.Now().Add(time.Hour),
	})

	if _, ok := cache.Get("expired", time.Now()); ok {
		t.Fatal("expected expired entry to be evicted")
	}
	if _, ok := cache.Get("fresh", time.Now()); !ok {
		t.Fatal("expected fresh entry to remain")
	}
	if _, ok := cache.Get("new", time.Now()); !ok {
		t.Fatal("expected new entry to be present")
	}
}

func TestResponseCacheEvictsEarliestExpiryWhenAllAreValid(t *testing.T) {
	t.Parallel()

	cache := &ResponseCache{
		entries:    make(map[string]CacheEntry),
		maxEntries: 2,
	}

	now := time.Now()
	cache.Set("newer", CacheEntry{Body: []byte("newer"), ExpiresAt: now.Add(time.Hour)})
	cache.Set("older", CacheEntry{Body: []byte("older"), ExpiresAt: now.Add(30 * time.Minute)})

	// third Set triggers eviction; "older" (earliest ExpiresAt) should be removed
	cache.Set("latest", CacheEntry{Body: []byte("latest"), ExpiresAt: now.Add(90 * time.Minute)})

	if _, ok := cache.Get("older", time.Now()); ok {
		t.Fatal("expected oldest-expiring entry to be evicted")
	}
	if _, ok := cache.Get("newer", time.Now()); !ok {
		t.Fatal("expected newer entry to remain")
	}
	if _, ok := cache.Get("latest", time.Now()); !ok {
		t.Fatal("expected latest entry to remain")
	}
}

func TestResponseCacheEvictsByByteLimit(t *testing.T) {
	t.Parallel()

	cache := &ResponseCache{
		entries:    make(map[string]CacheEntry),
		maxEntries: 10,
		maxBytes:   5,
	}
	now := time.Now()

	cache.Set("a", CacheEntry{Body: []byte("1234"), ExpiresAt: now.Add(time.Hour)})
	cache.Set("b", CacheEntry{Body: []byte("5678"), ExpiresAt: now.Add(2 * time.Hour)})

	if _, ok := cache.Get("a", time.Now()); ok {
		t.Fatal("expected earliest entry to be evicted by byte limit")
	}
	if _, ok := cache.Get("b", time.Now()); !ok {
		t.Fatal("expected newer entry to remain")
	}
}

func TestResponseCacheRejectsEntryLargerThanByteLimit(t *testing.T) {
	t.Parallel()

	cache := &ResponseCache{
		entries:    make(map[string]CacheEntry),
		maxEntries: 10,
		maxBytes:   5,
	}
	now := time.Now()

	cache.Set("too-large", CacheEntry{Body: []byte("123456"), ExpiresAt: now.Add(time.Hour)})

	if _, ok := cache.Get("too-large", time.Now()); ok {
		t.Fatal("expected oversized entry to be rejected")
	}
}

func TestResponseCacheSetOverwritesExistingKey(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	cache.Set("key", CacheEntry{Body: []byte("first"), ExpiresAt: time.Now().Add(time.Hour)})
	cache.Set("key", CacheEntry{Body: []byte("second"), ExpiresAt: time.Now().Add(time.Hour)})

	got, ok := cache.Get("key", time.Now())
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(got.Body) != "second" {
		t.Fatalf("expected overwritten value, got %q", string(got.Body))
	}
}

func TestResponseCacheGetIsSafeUnderConcurrentAccess(t *testing.T) {
	t.Parallel()

	cache := NewResponseCache()
	cache.Set("key", CacheEntry{Body: []byte("data"), ExpiresAt: time.Now().Add(time.Hour)})

	done := make(chan struct{})
	go func() {
		cache.Set("key", CacheEntry{Body: []byte("updated"), ExpiresAt: time.Now().Add(time.Hour)})
		close(done)
	}()

	if _, ok := cache.Get("key", time.Now()); !ok {
		t.Fatal("expected concurrent read to succeed")
	}
	<-done
}
