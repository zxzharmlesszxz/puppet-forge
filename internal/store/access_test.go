package store

import (
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func TestAccessOIDCMappings(t *testing.T) {
	t.Parallel()

	mappings := accessOIDCMappings(auth.TeamConfig{
		OIDCEmails:          []string{"dev@example.com", ""},
		OIDCSubjects:        []string{"subject-1"},
		OIDCDomains:         []string{"example.com"},
		OIDCGroups:          []string{"teamname-devops"},
		OIDCTeamAdminEmails: []string{"admin@example.com"},
		OIDCTeamAdminGroups: []string{"teamname-admins"},
		OIDCAdminEmails:     []string{"super@example.com"},
		OIDCAdminSubjects:   []string{"admin-subject"},
		OIDCAdminGroups:     []string{"global-admins"},
	})

	if len(mappings) != 9 {
		t.Fatalf("expected 9 non-empty mappings, got %d", len(mappings))
	}

	found := map[string]bool{}
	for _, m := range mappings {
		found[m.kind+":"+m.value] = true
	}

	expected := []string{
		"email:dev@example.com",
		"subject:subject-1",
		"domain:example.com",
		"group:teamname-devops",
		"team_admin_email:admin@example.com",
		"team_admin_group:teamname-admins",
		"admin_email:super@example.com",
		"admin_subject:admin-subject",
		"admin_group:global-admins",
	}
	for _, e := range expected {
		if !found[e] {
			t.Errorf("expected mapping %q not found", e)
		}
	}
}

func TestAccessOIDCMappingsSkipsEmptyValues(t *testing.T) {
	t.Parallel()

	mappings := accessOIDCMappings(auth.TeamConfig{
		OIDCEmails: []string{"", " ", ""},
	})
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings for empty/whitespace values, got %d", len(mappings))
	}
}

func TestApplyAccessToken(t *testing.T) {
	t.Parallel()

	cfg := &auth.TeamConfig{}
	applyAccessToken(cfg, "read", "read-token")
	if len(cfg.ReadTokens) != 1 || cfg.ReadTokens[0] != "read-token" {
		t.Fatalf("unexpected read tokens: %v", cfg.ReadTokens)
	}

	applyAccessToken(cfg, "publish", "publish-token")
	if len(cfg.PublishTokens) != 1 || cfg.PublishTokens[0] != "publish-token" {
		t.Fatalf("unexpected publish tokens: %v", cfg.PublishTokens)
	}

	applyAccessToken(cfg, "unknown", "ignored")
	if len(cfg.ReadTokens) != 1 || len(cfg.PublishTokens) != 1 {
		t.Fatalf("unknown token type should be ignored")
	}
}

func TestApplyAccessOIDCMapping(t *testing.T) {
	t.Parallel()

	cfg := &auth.TeamConfig{}
	applyAccessOIDCMapping(cfg, "email", "dev@example.com")
	if len(cfg.OIDCEmails) != 1 || cfg.OIDCEmails[0] != "dev@example.com" {
		t.Fatalf("unexpected emails: %v", cfg.OIDCEmails)
	}

	applyAccessOIDCMapping(cfg, "subject", "sub-123")
	if len(cfg.OIDCSubjects) != 1 || cfg.OIDCSubjects[0] != "sub-123" {
		t.Fatalf("unexpected subjects: %v", cfg.OIDCSubjects)
	}

	applyAccessOIDCMapping(cfg, "group", "teamname-devops")
	if len(cfg.OIDCGroups) != 1 || cfg.OIDCGroups[0] != "teamname-devops" {
		t.Fatalf("unexpected groups: %v", cfg.OIDCGroups)
	}

	applyAccessOIDCMapping(cfg, "admin_email", "admin@example.com")
	if len(cfg.OIDCAdminEmails) != 1 || cfg.OIDCAdminEmails[0] != "admin@example.com" {
		t.Fatalf("unexpected admin emails: %v", cfg.OIDCAdminEmails)
	}

	applyAccessOIDCMapping(cfg, "team_admin_group", "teamname-admins")
	if len(cfg.OIDCTeamAdminGroups) != 1 || cfg.OIDCTeamAdminGroups[0] != "teamname-admins" {
		t.Fatalf("unexpected team admin groups: %v", cfg.OIDCTeamAdminGroups)
	}
}

func TestApplyAccessOIDCMappingIgnoresUnknownType(t *testing.T) {
	t.Parallel()

	cfg := &auth.TeamConfig{}
	applyAccessOIDCMapping(cfg, "unknown", "value")
	if cfg.OIDCEmails != nil || cfg.OIDCSubjects != nil {
		t.Fatal("unknown mapping type should be ignored")
	}
}
