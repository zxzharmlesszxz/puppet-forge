package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRequirePublishUsesTeamAsDefaultOwner(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"publish-token"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", nil)
	req.Header.Set("Authorization", "Bearer publish-token")
	rec := httptest.NewRecorder()

	principal, ok := authorizer.RequirePublish(rec, req, "teamname")
	if !ok {
		t.Fatalf("RequirePublish() denied request: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if principal.Team != "teamname" || !principal.CanPublish || !principal.CanRead {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	ctxPrincipal, ok := PrincipalFromContext(req.Context())
	if !ok {
		t.Fatal("expected principal in request context")
	}
	if ctxPrincipal.Team != "teamname" {
		t.Fatalf("unexpected context principal team: %s", ctxPrincipal.Team)
	}
}

func TestTeamConfigJSONUsesExtraPublishSpacesWithLegacyFallback(t *testing.T) {
	t.Parallel()

	var cfg TeamConfig
	if err := json.Unmarshal([]byte(`{"team":"teamname","extra_publish_spaces":["teamname","shared"]}`), &cfg); err != nil {
		t.Fatalf("UnmarshalJSON(extra_publish_spaces) error = %v", err)
	}
	if !reflect.DeepEqual(cfg.PublishOwners, []string{"teamname", "shared"}) {
		t.Fatalf("unexpected extra publish spaces: %#v", cfg.PublishOwners)
	}

	cfg = TeamConfig{}
	if err := json.Unmarshal([]byte(`{"team":"teamname","publish_owners":["legacy"]}`), &cfg); err != nil {
		t.Fatalf("UnmarshalJSON(publish_owners) error = %v", err)
	}
	if !reflect.DeepEqual(cfg.PublishOwners, []string{"legacy"}) {
		t.Fatalf("unexpected legacy publish owners: %#v", cfg.PublishOwners)
	}

	cfg = TeamConfig{Team: "teamname", PublishOwners: []string{"teamname", "shared"}}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	if !json.Valid(body) || !strings.Contains(string(body), "extra_publish_spaces") {
		t.Fatalf("expected marshaled config to use extra_publish_spaces, got %s", string(body))
	}
}

func TestRequirePublishAnyRejectsReadToken(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			ReadTokens:    []string{"read-token"},
			PublishTokens: []string{"publish-token"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	readReq := httptest.NewRequest(http.MethodPost, "/api/v1/modules", nil)
	readReq.Header.Set("Authorization", "Bearer read-token")
	readRec := httptest.NewRecorder()
	if _, ok := authorizer.RequirePublishAny(readRec, readReq); ok {
		t.Fatal("expected read token to be rejected for publish")
	}
	if readRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected read token to get 401, got %d", readRec.Code)
	}

	publishReq := httptest.NewRequest(http.MethodPost, "/api/v1/modules", nil)
	publishReq.Header.Set("Authorization", "Bearer publish-token")
	publishRec := httptest.NewRecorder()
	principal, ok := authorizer.RequirePublishAny(publishRec, publishReq)
	if !ok {
		t.Fatalf("RequirePublishAny() denied publish token: status=%d body=%s", publishRec.Code, publishRec.Body.String())
	}
	if !principal.CanPublish || !principal.CanRead {
		t.Fatalf("unexpected principal: %#v", principal)
	}
}

func TestRequireDeleteRejectsPublisher(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			PublishTokens: []string{"publish-token"},
			PublishOwners: []string{"teamname"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/modules/teamname/apache", nil)
	req.Header.Set("Authorization", "Bearer publish-token")
	rec := httptest.NewRecorder()

	if _, ok := authorizer.RequireDelete(rec, req, "teamname"); ok {
		t.Fatal("expected delete to be rejected")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if rec.Body.String() != "{\"error\":\"admin or team admin access required\"}" {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}

func TestRequireDeleteAllowsAdminAcrossOwners(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:        "platform-admin",
			AdminTokens: []string{"admin-token"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/modules/acme/apache", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()

	principal, ok := authorizer.RequireDelete(rec, req, "acme")
	if !ok {
		t.Fatalf("RequireDelete() denied admin request: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !principal.CanAdmin || principal.Team != "platform-admin" {
		t.Fatalf("unexpected principal: %#v", principal)
	}
}

func TestPrincipalCanDeleteOwnerUsesManagedTeamsNotPublishOwners(t *testing.T) {
	t.Parallel()

	principal := Principal{
		Team:          "teamname",
		CanManageTeam: true,
		PublishOwners: map[string]struct{}{"teamname": {}, "shared": {}},
		ManagedTeams:  map[string]struct{}{"teamname": {}},
	}
	if !principal.CanDeleteOwner("teamname") {
		t.Fatal("team admin cannot delete in managed primary team space")
	}
	if principal.CanDeleteOwner("shared") {
		t.Fatal("team admin can delete in extra publish space")
	}
	if principal.CanDeleteOwner("carbon") {
		t.Fatal("team admin can delete outside managed teams")
	}

	admin := Principal{Team: "platform-admin", CanAdmin: true}
	if !admin.CanDeleteOwner("shared") {
		t.Fatal("global admin cannot delete across spaces")
	}
}

func TestAuthenticateOIDCMapsEmailSubjectAndDomain(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			PublishOwners: []string{"teamname", "platform"},
			OIDCEmails:    []string{"Dev@Example.COM"},
		},
		{
			Team:          "carbon",
			PublishOwners: []string{"carbon"},
			OIDCSubjects:  []string{"subject-123"},
		},
		{
			Team:          "infra",
			PublishOwners: []string{"infra"},
			OIDCDomains:   []string{"ops.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("dev@example.com", "", nil)
	if !ok || principal.Team != "teamname" || !principal.CanPublish {
		t.Fatalf("unexpected email principal: %#v ok=%v", principal, ok)
	}
	if _, ok := principal.PublishOwners["platform"]; !ok {
		t.Fatalf("expected platform owner in principal: %#v", principal.PublishOwners)
	}

	principal, ok = authorizer.AuthenticateOIDC("", "subject-123", nil)
	if !ok || principal.Team != "carbon" {
		t.Fatalf("unexpected subject principal: %#v ok=%v", principal, ok)
	}

	principal, ok = authorizer.AuthenticateOIDC("person@ops.example.com", "", nil)
	if !ok || principal.Team != "infra" {
		t.Fatalf("unexpected domain principal: %#v ok=%v", principal, ok)
	}
}

func TestAuthenticateOIDCMapsGroup(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			PublishOwners: []string{"teamname"},
			OIDCGroups:    []string{"Forge Publishers"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("", "", []string{"forge publishers"})
	if !ok || principal.Team != "teamname" || !principal.CanPublish {
		t.Fatalf("unexpected group principal: %#v ok=%v", principal, ok)
	}
}

func TestAuthenticateOIDCMapsAdminGroup(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:            "platform-admin",
			OIDCAdminGroups: []string{"forge-admins"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("", "", []string{"forge-admins"})
	if !ok || principal.Team != "platform-admin" || !principal.CanAdmin || principal.CanPublish {
		t.Fatalf("unexpected admin principal: %#v ok=%v", principal, ok)
	}
}

func TestAuthenticateOIDCMapsTeamAdminGroup(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:                "teamname",
			PublishOwners:       []string{"teamname"},
			OIDCGroups:          []string{"teamname-devops"},
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"teamname-admins"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("", "", []string{"teamname-admins", "teamname-devops"})
	if !ok {
		t.Fatal("expected OIDC principal")
	}
	if principal.Team != "teamname" || !principal.CanManageTeam || !principal.CanPublish || principal.CanAdmin {
		t.Fatalf("unexpected team admin principal: %#v", principal)
	}

	principal, ok = authorizer.AuthenticateOIDC("OWNER@example.com", "", nil)
	if !ok || principal.Team != "teamname" || !principal.CanManageTeam || !principal.CanPublish || principal.CanAdmin {
		t.Fatalf("unexpected team admin email principal: %#v ok=%v", principal, ok)
	}
}

func TestAuthenticateOIDCMergesMultipleTeamAdminMappings(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:                "teamname",
			PublishOwners:       []string{"teamname", "shared"},
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"teamname-admins"},
		},
		{
			Team:                "carbon",
			PublishOwners:       []string{"carbon"},
			OIDCTeamAdminEmails: []string{"owner@example.com"},
			OIDCTeamAdminGroups: []string{"carbon-admins"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("OWNER@example.com", "", []string{"teamname-admins", "carbon-admins"})
	if !ok {
		t.Fatal("expected OIDC principal")
	}
	if !principal.CanManageTeam || !principal.CanPublish || principal.CanAdmin {
		t.Fatalf("unexpected principal flags: %#v", principal)
	}
	if _, ok := principal.ManagedTeams["teamname"]; !ok {
		t.Fatalf("principal cannot manage teamname: %#v", principal.ManagedTeams)
	}
	if _, ok := principal.ManagedTeams["carbon"]; !ok {
		t.Fatalf("principal cannot manage carbon: %#v", principal.ManagedTeams)
	}
	for _, owner := range []string{"teamname", "shared", "carbon"} {
		if _, ok := principal.PublishOwners[owner]; !ok {
			t.Fatalf("principal cannot publish to %s: %#v", owner, principal.PublishOwners)
		}
	}
}

func TestAuthenticateOIDCPrefersAdminMappingOverTeamGroup(t *testing.T) {
	t.Parallel()

	authorizer, err := NewAuthorizer([]TeamConfig{
		{
			Team:          "teamname",
			PublishOwners: []string{"teamname"},
			OIDCGroups:    []string{"teamname-devops"},
		},
		{
			Team:            "platform-admin",
			OIDCAdminEmails: []string{"admin@example.com"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}

	principal, ok := authorizer.AuthenticateOIDC("admin@example.com", "", []string{"teamname-devops"})
	if !ok {
		t.Fatal("expected OIDC principal")
	}
	if principal.Team != "platform-admin" || !principal.CanAdmin || principal.CanPublish {
		t.Fatalf("expected admin principal to win over team group, got %#v", principal)
	}
}

func TestAuthenticateOIDCPrefersAdminMappingOverSameTeamAdminIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		configs []TeamConfig
	}{
		{
			name: "team-admin-before-admin",
			configs: []TeamConfig{
				{
					Team:                "teamname",
					OIDCTeamAdminEmails: []string{"owner@example.com"},
					OIDCTeamAdminGroups: []string{"forge-owners"},
				},
				{
					Team:            "platform-admin",
					OIDCAdminEmails: []string{"owner@example.com"},
					OIDCAdminGroups: []string{"forge-owners"},
				},
			},
		},
		{
			name: "admin-before-team-admin",
			configs: []TeamConfig{
				{
					Team:            "platform-admin",
					OIDCAdminEmails: []string{"owner@example.com"},
					OIDCAdminGroups: []string{"forge-owners"},
				},
				{
					Team:                "teamname",
					OIDCTeamAdminEmails: []string{"owner@example.com"},
					OIDCTeamAdminGroups: []string{"forge-owners"},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			authorizer, err := NewAuthorizer(tc.configs)
			if err != nil {
				t.Fatalf("NewAuthorizer() error = %v", err)
			}

			principal, ok := authorizer.AuthenticateOIDC("owner@example.com", "", []string{"forge-owners"})
			if !ok {
				t.Fatal("expected OIDC principal")
			}
			if principal.Team != "platform-admin" || !principal.CanAdmin || principal.CanPublish || principal.CanManageTeam {
				t.Fatalf("expected global admin principal to win, got %#v", principal)
			}
		})
	}
}

func TestNewAuthorizerRejectsDuplicateOIDCEmail(t *testing.T) {
	t.Parallel()

	_, err := NewAuthorizer([]TeamConfig{
		{
			Team:       "teamname",
			OIDCEmails: []string{"dev@example.com"},
		},
		{
			Team:       "carbon",
			OIDCEmails: []string{"DEV@example.com"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate OIDC email error")
	}
}

func TestNewAuthorizerRejectsDuplicateOIDCGroup(t *testing.T) {
	t.Parallel()

	_, err := NewAuthorizer([]TeamConfig{
		{
			Team:       "teamname",
			OIDCGroups: []string{"forge-publishers"},
		},
		{
			Team:       "carbon",
			OIDCGroups: []string{"FORGE-PUBLISHERS"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate OIDC group error")
	}
}

func TestNewAuthorizerRejectsOIDCGroupUsedForPublishAndTeamAdmin(t *testing.T) {
	t.Parallel()

	_, err := NewAuthorizer([]TeamConfig{
		{
			Team:                "teamname",
			OIDCGroups:          []string{"Forge-Team"},
			OIDCTeamAdminGroups: []string{" forge-team "},
		},
	})
	if err == nil {
		t.Fatal("expected OIDC group role conflict error")
	}
	if !strings.Contains(err.Error(), "cannot be configured as both publish group and team admin group") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBearerTokenParsesAuthorizationHeader(t *testing.T) {
	t.Parallel()

	if token := bearerToken("Bearer secret-token"); token != "secret-token" {
		t.Fatalf("expected token to be parsed, got %q", token)
	}
	if token := bearerToken("Basic secret-token"); token != "" {
		t.Fatalf("expected empty token for non-bearer auth, got %q", token)
	}
}
