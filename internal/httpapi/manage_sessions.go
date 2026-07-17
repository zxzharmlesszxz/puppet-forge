package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/securecookie"
)

const defaultMaxManageSessions = 4096

type manageSession struct {
	token     string
	expiresAt time.Time
}

type manageSessionStore struct {
	cookies     *securecookie.SecureCookie
	mu          sync.RWMutex
	sessions    map[string]manageSession
	order       []string
	maxSessions int
}

func newManageSessionStore(secret string) *manageSessionStore {
	return &manageSessionStore{
		cookies:     newManageSessionCookie(secret),
		sessions:    make(map[string]manageSession),
		maxSessions: defaultMaxManageSessions,
	}
}

func (s *manageSessionStore) Create(token string, ttl time.Duration) (string, error) {
	if s.cookies != nil {
		encoded, err := s.cookies.Encode(manageTokenCookie, manageCookieSession{
			Token:     token,
			ExpiresAt: time.Now().Add(ttl),
		})
		if err != nil {
			return "", err
		}
		return encoded, nil
	}

	sessionID, err := randomBase64URL(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	session := manageSession{
		token:     token,
		expiresAt: now.Add(ttl),
	}

	s.mu.Lock()
	s.sessions[sessionID] = session
	s.order = append(s.order, sessionID)
	s.deleteExpiredLocked(now)
	s.evictOldestLocked()
	s.compactOrderLocked()
	s.mu.Unlock()

	return sessionID, nil
}

func (s *manageSessionStore) Token(sessionID string, now time.Time) (string, bool) {
	if s.cookies != nil {
		var session manageCookieSession
		if err := s.cookies.Decode(manageTokenCookie, sessionID, &session); err != nil {
			return "", false
		}
		if session.Token == "" || now.After(session.ExpiresAt) {
			return "", false
		}
		return session.Token, true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return "", false
	}
	if now.After(session.expiresAt) {
		delete(s.sessions, sessionID)
		return "", false
	}
	return session.token, true
}

func (s *manageSessionStore) Delete(sessionID string) {
	if s.cookies != nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

type manageCookieSession struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newManageSessionCookie(secret string) *securecookie.SecureCookie {
	if secret == "" {
		return nil
	}
	hashKey := sha256.Sum256([]byte(secret + "|manage-session-hash"))
	blockKey := sha256.Sum256([]byte(secret + "|manage-session-block"))
	return securecookie.New(hashKey[:], blockKey[:])
}

func randomManageSessionSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (s *manageSessionStore) deleteExpiredLocked(now time.Time) {
	for sessionID, session := range s.sessions {
		if now.After(session.expiresAt) {
			delete(s.sessions, sessionID)
		}
	}
}

func (s *manageSessionStore) evictOldestLocked() {
	if s.maxSessions <= 0 {
		return
	}
	for len(s.sessions) > s.maxSessions && len(s.order) > 0 {
		sessionID := s.order[0]
		s.order = s.order[1:]
		delete(s.sessions, sessionID)
	}
}

func (s *manageSessionStore) compactOrderLocked() {
	if len(s.order) <= len(s.sessions)+s.maxSessions {
		return
	}
	compacted := s.order[:0]
	for _, sessionID := range s.order {
		if _, ok := s.sessions[sessionID]; ok {
			compacted = append(compacted, sessionID)
		}
	}
	s.order = compacted
}
