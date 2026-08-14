package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const minAccessTokenPepperBytes = 32

type AccessTokenRecord struct {
	ID          string
	Prefix      string
	Digest      string
	Description string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	LastUsedAt  *time.Time
}

func (t AccessTokenRecord) Active(now time.Time) bool {
	return t.Digest != "" && t.RevokedAt == nil && (t.ExpiresAt == nil || now.Before(*t.ExpiresAt))
}

type TokenHasher struct {
	key []byte
}

func NewTokenHasher(pepper string) (*TokenHasher, error) {
	pepper = strings.TrimSpace(pepper)
	if len(pepper) < minAccessTokenPepperBytes {
		return nil, fmt.Errorf("access token pepper must contain at least %d bytes", minAccessTokenPepperBytes)
	}
	return &TokenHasher{key: []byte(pepper)}, nil
}

func (h *TokenHasher) Digest(token string) string {
	if h == nil {
		return ""
	}
	mac := hmac.New(sha256.New, h.key)
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func GenerateAccessToken(kind string, now time.Time) (string, AccessTokenRecord, error) {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind != "read" && kind != "publish" {
		return "", AccessTokenRecord{}, errors.New("access token type must be read or publish")
	}

	prefixBytes := make([]byte, 5)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(prefixBytes); err != nil {
		return "", AccessTokenRecord{}, fmt.Errorf("generate access token prefix: %w", err)
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", AccessTokenRecord{}, fmt.Errorf("generate access token secret: %w", err)
	}
	prefix := strings.ToLower(base64.RawURLEncoding.EncodeToString(prefixBytes))
	raw := fmt.Sprintf("pf_%s_%s_%s", kind, prefix, base64.RawURLEncoding.EncodeToString(secretBytes))
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", AccessTokenRecord{}, fmt.Errorf("generate access token id: %w", err)
	}

	return raw, AccessTokenRecord{
		ID:        hex.EncodeToString(idBytes),
		Prefix:    fmt.Sprintf("pf_%s_%s", kind, prefix),
		CreatedAt: now.UTC(),
	}, nil
}

func (h *TokenHasher) Record(raw string, record AccessTokenRecord) (AccessTokenRecord, error) {
	if h == nil {
		return AccessTokenRecord{}, errors.New("access token hasher is not configured")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AccessTokenRecord{}, errors.New("access token is required")
	}
	record.Digest = h.Digest(raw)
	return record, nil
}
