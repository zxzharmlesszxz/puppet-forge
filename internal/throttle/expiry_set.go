package throttle

import (
	"container/heap"
	"sync"
	"time"
)

type expiryEntry struct {
	key       string
	expiresAt time.Time
	index     int
}

type expiryHeap []*expiryEntry

func (h *expiryHeap) Len() int { return len(*h) }

func (h *expiryHeap) Less(i, j int) bool {
	if (*h)[i].expiresAt.Equal((*h)[j].expiresAt) {
		return (*h)[i].key < (*h)[j].key
	}
	return (*h)[i].expiresAt.Before((*h)[j].expiresAt)
}

func (h *expiryHeap) Swap(i, j int) {
	(*h)[i], (*h)[j] = (*h)[j], (*h)[i]
	(*h)[i].index = i
	(*h)[j].index = j
}

func (h *expiryHeap) Push(value any) {
	entry := value.(*expiryEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}

func (h *expiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	entry := old[last]
	old[last] = nil
	entry.index = -1
	*h = old[:last]
	return entry
}

// ExpirySet suppresses repeated keys for a fixed interval while bounding memory.
type ExpirySet struct {
	mu         sync.Mutex
	maxEntries int
	interval   time.Duration
	recorded   map[string]*expiryEntry
	expiry     expiryHeap
}

func NewExpirySet(maxEntries int, interval time.Duration) *ExpirySet {
	return &ExpirySet{
		maxEntries: maxEntries,
		interval:   interval,
		recorded:   make(map[string]*expiryEntry),
	}
}

func (s *ExpirySet) Record(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(now)
	if s.recorded[key] != nil {
		return false
	}
	if s.maxEntries > 0 && len(s.recorded) >= s.maxEntries {
		s.removeNextExpiryLocked()
	}
	entry := &expiryEntry{key: key, expiresAt: now.Add(s.interval)}
	s.recorded[key] = entry
	heap.Push(&s.expiry, entry)
	return true
}

func (s *ExpirySet) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.recorded[key]
	if entry == nil {
		return
	}
	heap.Remove(&s.expiry, entry.index)
	delete(s.recorded, key)
}

func (s *ExpirySet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recorded)
}

func (s *ExpirySet) Contains(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recorded[key] != nil
}

func (s *ExpirySet) removeExpiredLocked(now time.Time) {
	for len(s.expiry) > 0 && !now.Before(s.expiry[0].expiresAt) {
		s.removeNextExpiryLocked()
	}
}

func (s *ExpirySet) removeNextExpiryLocked() {
	if len(s.expiry) == 0 {
		return
	}
	entry := heap.Pop(&s.expiry).(*expiryEntry)
	delete(s.recorded, entry.key)
}
