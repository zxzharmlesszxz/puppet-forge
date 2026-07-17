package httpapi

import (
	"sync"
	"time"
)

const defaultMaxManageSessions = 4096

type manageSession struct {
	token     string
	expiresAt time.Time
}

type manageSessionStore struct {
	mu          sync.RWMutex
	sessions    map[string]manageSession
	order       []string
	maxSessions int
}

func newManageSessionStore() *manageSessionStore {
	return &manageSessionStore{
		sessions:    make(map[string]manageSession),
		maxSessions: defaultMaxManageSessions,
	}
}

func (s *manageSessionStore) Create(token string, ttl time.Duration) (string, error) {
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
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
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
