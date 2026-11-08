package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

func TestModerationAuditDisplaysDecisionAndComment(t *testing.T) {
	for _, decision := range []struct{ action, from, to string }{
		{"approve", "pending", "available"}, {"block", "available", "blocked"},
		{"unblock", "blocked", "pending"}, {"deprecate", "available", "deprecated"},
	} {
		for _, commentKey := range []string{"reason", "justification"} {
			t.Run(decision.action+"/"+commentKey, func(t *testing.T) {
				event := store.AuditEvent{Action: "moderate_resource", Result: "success", Reason: "none", Attributes: map[string]string{
					"moderation_action": decision.action, "from_state": decision.from, "to_state": decision.to,
					"replacement": "<script>replacement</script>", commentKey: "<script>comment</script>", "sha256": "reviewed-sha", "module": "teamname/demo", "release": "1.0.0", "token_hash": "must-not-render",
				}}
				rows := manageAuditRows([]store.AuditEvent{event})
				if len(rows) != 1 || !strings.Contains(rows[0].Details, "Decision: "+decision.action) || !strings.Contains(rows[0].Details, decision.from+" -> "+decision.to) || rows[0].Justification != event.Attributes[commentKey] {
					t.Fatalf("incomplete moderation audit: %#v", rows)
				}
				for _, title := range []string{"Audit log", "Team audit log"} {
					rec := httptest.NewRecorder()
					executeHTMLTemplate(rec, manageAuditTemplate, manageAuditData{Title: title, Events: rows})
					body := rec.Body.String()
					for _, expected := range []string{"Decision: " + decision.action, "reviewed-sha", "&lt;script&gt;comment&lt;/script&gt;", "&lt;script&gt;replacement&lt;/script&gt;"} {
						if !strings.Contains(body, expected) {
							t.Fatalf("%s misses %q: %s", title, expected, body)
						}
					}
					if strings.Contains(body, "<script>comment") || strings.Contains(body, "must-not-render") {
						t.Fatal("audit rendered unsafe content or hidden credentials")
					}
				}
			})
		}
	}
}

type cancelAfterModuleDeleteStore struct {
	*store.SQLiteStore
	cancel context.CancelFunc
}

func (s *cancelAfterModuleDeleteStore) DeleteModule(ctx context.Context, owner, name string) error {
	if err := s.SQLiteStore.DeleteModule(ctx, owner, name); err != nil {
		return err
	}
	s.cancel()
	return nil
}

func TestDeleteAuditSurvivesPostCommitCancellation(t *testing.T) {
	st := newHTTPAPITestStore(t)
	createV3CollectionRelease(t, st, "teamname", "module", "1.0.0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := &cancelAfterModuleDeleteStore{SQLiteStore: st, cancel: cancel}
	modules := service.NewModuleService(backend, testArtifactStorage{}, "modules", nil)
	handler := newTestRouter(modules, nil, "http://example.test", newAdminAuthorizer(t), nil, "admin-token", false, defaultActiveReleaseTTL)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/modules/teamname/module?force=true&reason=broken&confirmation=teamname%2Fmodule", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	events, total, err := st.ListAuditEvents(t.Context(), nil, "force_delete_module", 10, 0)
	if err != nil || total != 1 || len(events) != 1 || events[0].Attributes["justification"] != "broken" || events[0].Actor == "anonymous" {
		t.Fatalf("audit after canceled request = %#v total=%d err=%v", events, total, err)
	}
}

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
		{Team: "teamone", OIDCEmails: []string{"member@example.com"}, OIDCReaderGroups: []string{"readers"}},
		{Team: "unrelated"},
	}
	for _, tc := range []struct {
		session webauth.Session
		want    []string
	}{
		{webauth.Session{Email: "member@example.com", EmailVerified: true, Groups: []string{"global-admins", "team-admins"}}, []string{"teamone", "teamtwo"}},
		{webauth.Session{Email: "member@example.com", Groups: []string{"team-admins"}}, []string{"teamtwo"}},
		{webauth.Session{Groups: []string{"readers"}}, []string{"teamone"}},
		{webauth.Session{Groups: []string{"global-admins", "readers"}}, []string{"teamone"}},
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
		{Team: "teamone", OIDCReaderGroups: []string{"members"}},
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
