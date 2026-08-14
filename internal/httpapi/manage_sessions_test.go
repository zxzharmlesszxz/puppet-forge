package httpapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type testManageSessionBackend struct {
	mu       sync.Mutex
	sessions map[string]store.ManageSession
}

func newTestManageSessionBackend() *testManageSessionBackend {
	return &testManageSessionBackend{sessions: make(map[string]store.ManageSession)}
}

func (b *testManageSessionBackend) CreateManageSession(_ context.Context, session store.ManageSession) error {
	b.mu.Lock()
	b.sessions[session.SessionHash] = session
	b.mu.Unlock()
	return nil
}

func (b *testManageSessionBackend) GetManageSession(_ context.Context, sessionHash string, now time.Time) (store.ManageSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessions[sessionHash]
	if !ok || session.RevokedAt != nil || !now.Before(session.ExpiresAt) {
		return store.ManageSession{}, store.ErrNotFound
	}
	return session, nil
}

func (b *testManageSessionBackend) RevokeManageSession(_ context.Context, sessionHash string, revokedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessions[sessionHash]
	if !ok {
		return nil
	}
	session.RevokedAt = &revokedAt
	b.sessions[sessionHash] = session
	return nil
}

func TestManageSessionStoreWorksAcrossReplicasWithSharedSecret(t *testing.T) {
	t.Parallel()

	backend := newTestManageSessionBackend()
	firstStore := newManageSessionStore(backend, "shared-secret")
	secondStore := newManageSessionStore(backend, "shared-secret")
	sessionID, csrfSecret, err := firstStore.Create(context.Background(), "credential-digest", "token-id", time.Minute)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if sessionID == "credential-digest" || csrfSecret == "" {
		t.Fatalf("unexpected browser session values session=%q csrf=%q", sessionID, csrfSecret)
	}
	session, ok := secondStore.Session(context.Background(), sessionID, time.Now())
	if !ok || session.CredentialHash != "credential-digest" || session.CredentialID != "token-id" || session.CSRFSecret != csrfSecret {
		t.Fatalf("second replica session = %#v ok=%v", session, ok)
	}
	backend.mu.Lock()
	for hash, persisted := range backend.sessions {
		if hash == sessionID || persisted.SessionHash == sessionID {
			t.Fatal("backend persisted the raw browser session ID")
		}
	}
	backend.mu.Unlock()
}

func TestManageSessionStoreRejectsDifferentSecret(t *testing.T) {
	t.Parallel()

	backend := newTestManageSessionBackend()
	firstStore := newManageSessionStore(backend, "shared-secret")
	secondStore := newManageSessionStore(backend, "other-secret")
	sessionID, _, err := firstStore.Create(context.Background(), "credential-digest", "", time.Minute)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, ok := secondStore.Session(context.Background(), sessionID, time.Now()); ok {
		t.Fatal("different session secret accepted copied cookie")
	}
}

func TestManageSessionStoreRejectsExpiredSession(t *testing.T) {
	t.Parallel()

	backend := newTestManageSessionBackend()
	sessions := newManageSessionStore(backend, "shared-secret")
	sessionID, _, err := sessions.Create(context.Background(), "credential-digest", "", time.Nanosecond)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, ok := sessions.Session(context.Background(), sessionID, time.Now().Add(time.Minute)); ok {
		t.Fatal("expired session was accepted")
	}
}

func TestManageSessionStoreDeleteRevokesCopiedCookie(t *testing.T) {
	t.Parallel()

	backend := newTestManageSessionBackend()
	firstStore := newManageSessionStore(backend, "shared-secret")
	secondStore := newManageSessionStore(backend, "shared-secret")
	sessionID, _, err := firstStore.Create(context.Background(), "credential-digest", "", time.Minute)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := firstStore.Delete(context.Background(), sessionID, time.Now()); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok := secondStore.Session(context.Background(), sessionID, time.Now()); ok {
		t.Fatal("revoked copied cookie was accepted by another replica")
	}
}
