package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type manageSessionBackend interface {
	CreateManageSession(ctx context.Context, session store.ManageSession) error
	GetManageSession(ctx context.Context, sessionHash string, now time.Time) (store.ManageSession, error)
	RevokeManageSession(ctx context.Context, sessionHash string, revokedAt time.Time) error
}

type manageSessionStore struct {
	backend manageSessionBackend
	key     []byte
}

func newManageSessionStore(backend manageSessionBackend, secret string) *manageSessionStore {
	key := sha256.Sum256([]byte(secret + "|manage-session-id"))
	return &manageSessionStore{backend: backend, key: key[:]}
}

func (s *manageSessionStore) Create(ctx context.Context, credentialHash, credentialID string, ttl time.Duration) (string, string, error) {
	if s == nil || s.backend == nil {
		return "", "", errors.New("manage session store is not configured")
	}
	if credentialHash == "" {
		return "", "", errors.New("manage session credential is required")
	}
	sessionID, err := randomBase64URL(32)
	if err != nil {
		return "", "", err
	}
	csrfSecret, err := randomBase64URL(32)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	if err := s.backend.CreateManageSession(ctx, store.ManageSession{
		SessionHash:    s.hashSessionID(sessionID),
		CredentialHash: credentialHash,
		CredentialID:   credentialID,
		AuthMethod:     "token",
		CSRFSecret:     csrfSecret,
		CreatedAt:      now,
		ExpiresAt:      now.Add(ttl),
		LastSeenAt:     now,
	}); err != nil {
		return "", "", err
	}
	return sessionID, csrfSecret, nil
}

func (s *manageSessionStore) Session(ctx context.Context, sessionID string, now time.Time) (store.ManageSession, bool) {
	if s == nil || s.backend == nil || sessionID == "" {
		return store.ManageSession{}, false
	}
	session, err := s.backend.GetManageSession(ctx, s.hashSessionID(sessionID), now.UTC())
	return session, err == nil
}

func (s *manageSessionStore) Delete(ctx context.Context, sessionID string, now time.Time) error {
	if s == nil || s.backend == nil || sessionID == "" {
		return nil
	}
	return s.backend.RevokeManageSession(ctx, s.hashSessionID(sessionID), now.UTC())
}

func (s *manageSessionStore) hashSessionID(sessionID string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}
