package proxy

import (
	"sort"
	"sync"
	"time"
)

const defaultMaxCacheEntries = 1000
const defaultMaxCacheBytes = 16 << 20

type CacheEntry struct {
	StatusCode int
	Header     map[string][]string
	Body       []byte
	ExpiresAt  time.Time
}

type cacheEntryRef struct {
	key       string
	expiresAt time.Time
}

type ResponseCache struct {
	mu         sync.RWMutex
	entries    map[string]CacheEntry
	maxEntries int
	maxBytes   int
	bytes      int
}

func NewResponseCache() *ResponseCache {
	return &ResponseCache{
		entries:    make(map[string]CacheEntry),
		maxEntries: defaultMaxCacheEntries,
		maxBytes:   defaultMaxCacheBytes,
	}
}

func (c *ResponseCache) Get(key string, now time.Time) (CacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return CacheEntry{}, false
	}
	if now.After(entry.ExpiresAt) {
		return CacheEntry{}, false
	}
	return entry, true
}

func (c *ResponseCache) GetStale(key string, now time.Time, maxStaleAge time.Duration) (CacheEntry, bool) {
	if maxStaleAge <= 0 {
		return CacheEntry{}, false
	}
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return CacheEntry{}, false
	}
	if now.After(entry.ExpiresAt.Add(maxStaleAge)) {
		return CacheEntry{}, false
	}
	return entry, true
}

func (c *ResponseCache) Set(key string, entry CacheEntry) {
	c.mu.Lock()
	if c.maxBytes > 0 && len(entry.Body) > c.maxBytes {
		if old, ok := c.entries[key]; ok {
			delete(c.entries, key)
			c.bytes -= len(old.Body)
		}
		c.mu.Unlock()
		return
	}
	if old, ok := c.entries[key]; ok {
		c.bytes -= len(old.Body)
	}
	c.entries[key] = entry
	c.bytes += len(entry.Body)
	overCapacity := c.maxEntries > 0 && len(c.entries) > c.maxEntries
	overBytes := c.maxBytes > 0 && c.bytes > c.maxBytes
	c.mu.Unlock()

	if overCapacity || overBytes {
		c.evict()
	}
}

func (c *ResponseCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.ExpiresAt) {
			delete(c.entries, k)
			c.bytes -= len(e.Body)
		}
	}
	if !c.overLimitLocked() {
		return
	}
	entries := make([]cacheEntryRef, 0, len(c.entries))
	for k, e := range c.entries {
		entries = append(entries, cacheEntryRef{key: k, expiresAt: e.ExpiresAt})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].expiresAt.Before(entries[j].expiresAt)
	})
	for i := 0; i < len(entries) && c.overLimitLocked(); i++ {
		entry := c.entries[entries[i].key]
		delete(c.entries, entries[i].key)
		c.bytes -= len(entry.Body)
	}
}

func (c *ResponseCache) overLimitLocked() bool {
	return (c.maxEntries > 0 && len(c.entries) > c.maxEntries) || (c.maxBytes > 0 && c.bytes > c.maxBytes)
}
