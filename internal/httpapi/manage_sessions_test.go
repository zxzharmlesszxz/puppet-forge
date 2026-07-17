package httpapi

import (
	"testing"
	"time"
)

func TestManageSessionStoreEvictsOldestWhenSessionLimitIsExceeded(t *testing.T) {
	t.Parallel()

	store := newManageSessionStore()
	store.maxSessions = 2

	firstID, err := store.Create("first-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	secondID, err := store.Create("second-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	thirdID, err := store.Create("third-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(third) error = %v", err)
	}

	if _, ok := store.Token(firstID, time.Now()); ok {
		t.Fatal("expected oldest session to be evicted")
	}
	if token, ok := store.Token(secondID, time.Now()); !ok || token != "second-token" {
		t.Fatalf("expected second session to remain, got token=%q ok=%v", token, ok)
	}
	if token, ok := store.Token(thirdID, time.Now()); !ok || token != "third-token" {
		t.Fatalf("expected third session to remain, got token=%q ok=%v", token, ok)
	}
}

func TestManageSessionStoreCompactsDeletedSessionOrder(t *testing.T) {
	t.Parallel()

	store := newManageSessionStore()
	store.maxSessions = 2

	firstID, err := store.Create("first-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	store.Delete(firstID)
	secondID, err := store.Create("second-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	thirdID, err := store.Create("third-token", time.Minute)
	if err != nil {
		t.Fatalf("Create(third) error = %v", err)
	}

	if token, ok := store.Token(secondID, time.Now()); !ok || token != "second-token" {
		t.Fatalf("expected second session to remain, got token=%q ok=%v", token, ok)
	}
	if token, ok := store.Token(thirdID, time.Now()); !ok || token != "third-token" {
		t.Fatalf("expected third session to remain, got token=%q ok=%v", token, ok)
	}
}

func TestManageSessionStoreTokenDeletesExpiredSession(t *testing.T) {
	t.Parallel()

	store := newManageSessionStore()
	sessionID, err := store.Create("expired-token", time.Nanosecond)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if token, ok := store.Token(sessionID, time.Now().Add(time.Minute)); ok || token != "" {
		t.Fatalf("expected expired session to be rejected, got token=%q ok=%v", token, ok)
	}
	store.mu.RLock()
	_, exists := store.sessions[sessionID]
	store.mu.RUnlock()
	if exists {
		t.Fatal("expected expired session to be deleted")
	}
}
