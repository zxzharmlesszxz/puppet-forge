package throttle

import (
	"testing"
	"time"
)

func TestExpirySetExpiresForgetsAndBoundsEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	set := NewExpirySet(2, time.Minute)
	if !set.Record("a", now) || !set.Record("b", now) {
		t.Fatal("first records must be accepted")
	}
	if set.Record("a", now) {
		t.Fatal("duplicate record within the interval must be suppressed")
	}
	if !set.Record("c", now) || set.Contains("a") {
		t.Fatal("capacity must evict the deterministic oldest entry")
	}
	set.Forget("b")
	if set.Contains("b") || set.Len() != 1 {
		t.Fatal("forgotten entry remains recorded")
	}
	if !set.Record("c", now.Add(time.Minute)) {
		t.Fatal("expired entry must be accepted again")
	}
}
