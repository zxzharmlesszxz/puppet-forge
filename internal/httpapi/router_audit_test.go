package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

func TestAuditOIDCLoginUsesVerifiedCallbackIdentityWithoutCookie(t *testing.T) {
	t.Parallel()
	ctx := observability.ContextWithRequestID(t.Context(), "oidc-login-request")
	st := newHTTPAPITestStore(t)
	router := &Router{modules: service.NewModuleService(st, nil, "modules", nil)}
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil).WithContext(ctx)
	session := webauth.Session{Sub: "stable-subject", Name: "Admin User", Email: "admin@example.com"}
	router.auditWithOIDCSession(req, auth.Principal{}, &session, "oidc_login", "success", "none")
	events, total, err := st.ListAuditEvents(ctx, nil, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("audit count=%d, events=%d, want one", total, len(events))
	}
	event := events[0]
	if event.Actor != "Admin User <admin@example.com>" || event.AuthMethod != "oidc" || event.Attributes["oidc_subject"] != "stable-subject" {
		t.Fatalf("audit misses verified identity: %#v", event)
	}
	if event.Action != "oidc_login" || event.Result != "success" || event.RequestID != "oidc-login-request" {
		t.Fatalf("unexpected login audit: %#v", event)
	}
}

func TestOIDCAuditTeamsUsesActualMemberships(t *testing.T) {
	t.Parallel()
	configs := []auth.TeamConfig{
		{Team: auth.GlobalAdminTeam, OIDCAdminGroups: []string{"global-admins"}},
		{Team: "teamtwo", OIDCTeamAdminGroups: []string{"team-admins"}},
		{Team: "teamone", OIDCEmails: []string{"member@example.com"}},
		{Team: "unrelated", PublishOwners: []string{"teamone"}},
	}
	for _, tc := range []struct {
		session webauth.Session
		want    []string
	}{
		{webauth.Session{Email: "member@example.com", Groups: []string{"global-admins", "team-admins"}}, []string{"teamone", "teamtwo"}},
		{webauth.Session{Groups: []string{"global-admins"}}, nil},
		{webauth.Session{Groups: []string{"unknown"}}, nil},
	} {
		teams, err := oidcAuditTeams(configs, tc.session)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(teams, tc.want) {
			t.Errorf("audit teams=%v, want %v", teams, tc.want)
		}
	}
}

func TestOIDCLoginAuditAppearsInEveryMemberTeam(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := newHTTPAPITestStore(t)
	if err := st.ReplaceTeamConfigs(ctx, []auth.TeamConfig{
		{Team: "teamone", OIDCGroups: []string{"members"}},
		{Team: "teamtwo", OIDCTeamAdminGroups: []string{"members"}},
	}); err != nil {
		t.Fatal(err)
	}
	router := &Router{modules: service.NewModuleService(st, nil, "modules", nil)}
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil).WithContext(ctx)
	router.auditOIDCLogin(req, webauth.Session{Sub: "member", Email: "member@example.com", Groups: []string{"members"}})
	for _, scopes := range [][]string{nil, {"teamone"}, {"teamtwo"}, {"teamone", "teamtwo"}, {"unrelated"}, {"team"}} {
		events, total, err := st.ListAuditEvents(ctx, scopes, "oidc_login", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if slices.Contains(scopes, "unrelated") || slices.Contains(scopes, "team") {
			want = 0
		}
		if total != want || len(events) != want {
			t.Errorf("scopes=%v: total=%d events=%d, want %d", scopes, total, len(events), want)
		}
	}
}
