package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTokenHasherAuthenticatesByDigest(t *testing.T) {
	t.Parallel()

	hasher, err := NewTokenHasher(strings.Repeat("p", 32))
	if err != nil {
		t.Fatalf("NewTokenHasher() error = %v", err)
	}
	raw, record, err := GenerateAccessToken("publish", time.Now())
	if err != nil {
		t.Fatalf("GenerateAccessToken() error = %v", err)
	}
	record, err = hasher.Record(raw, record)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	authorizer, err := NewAuthorizerWithTokenHasher([]TeamConfig{{
		Team:                "teamname",
		PublishTokenRecords: []AccessTokenRecord{record},
	}}, hasher)
	if err != nil {
		t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateToken(raw)
	if !ok || !principal.CanPublishOwner("teamname") {
		t.Fatalf("generated token was not authenticated: principal=%#v ok=%v", principal, ok)
	}
	if principal.TokenID != record.ID {
		t.Fatalf("authenticated principal token ID = %q, want %q", principal.TokenID, record.ID)
	}
	if _, ok := authorizer.tokens[raw]; ok {
		t.Fatal("authorizer retained the raw access token")
	}
}

func TestTokenHasherRejectsExpiredAndRevokedRecords(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	hasher, err := NewTokenHasher(strings.Repeat("p", 32))
	if err != nil {
		t.Fatalf("NewTokenHasher() error = %v", err)
	}
	for name, mutate := range map[string]func(*AccessTokenRecord){
		"expired": func(record *AccessTokenRecord) {
			expiresAt := now.Add(-time.Minute)
			record.ExpiresAt = &expiresAt
		},
		"revoked": func(record *AccessTokenRecord) {
			revokedAt := now
			record.RevokedAt = &revokedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, record, err := GenerateAccessToken("read", now)
			if err != nil {
				t.Fatalf("GenerateAccessToken() error = %v", err)
			}
			record, err = hasher.Record(raw, record)
			if err != nil {
				t.Fatalf("Record() error = %v", err)
			}
			mutate(&record)
			authorizer, err := NewAuthorizerWithTokenHasher([]TeamConfig{{
				Team:             "teamname",
				ReadTokenRecords: []AccessTokenRecord{record},
			}}, hasher)
			if err != nil {
				t.Fatalf("NewAuthorizerWithTokenHasher() error = %v", err)
			}
			if _, ok := authorizer.AuthenticateToken(raw); ok {
				t.Fatalf("%s token was accepted", name)
			}
		})
	}
}

func TestGenerateAccessTokenUsesOpaqueTypedFormat(t *testing.T) {
	t.Parallel()

	raw, record, err := GenerateAccessToken("read", time.Now())
	if err != nil {
		t.Fatalf("GenerateAccessToken() error = %v", err)
	}
	if !strings.HasPrefix(raw, "pf_read_") || !strings.HasPrefix(record.Prefix, "pf_read_") {
		t.Fatalf("unexpected token format raw=%q prefix=%q", raw, record.Prefix)
	}
	if record.ID == "" || record.Digest != "" {
		t.Fatalf("unexpected generated record: %#v", record)
	}
}
