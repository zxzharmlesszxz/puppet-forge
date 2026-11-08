package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
)

func TestTeamAccessPreservesHiddenMappingsForAllAdministrators(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, clearFields := range []bool{false, true} {
			t.Run(fmt.Sprintf("global=%v/clear=%v", global, clearFields), func(t *testing.T) {
				st := newHTTPAPITestStore(t)
				if err := st.ReplaceTeamConfigs(t.Context(), []auth.TeamConfig{{Team: "teamname", OIDCSubjects: []string{"stable-subject"}, OIDCDomains: []string{"example.com"}, OIDCReaderGroups: []string{"old-readers"}, OIDCEmails: []string{"old@example.com"}, OIDCTeamAdminGroups: []string{"old-admins"}, ReadTokens: []string{"read-token"}, PublishTokens: []string{"publish-token"}}}); err != nil {
					t.Fatal(err)
				}
				original, err := st.LoadTeamConfigs(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				expected := original[0]
				expected.OIDCEmails, expected.OIDCReaderGroups, expected.OIDCTeamAdminGroups = []string{}, []string{}, []string{}
				expected.OIDCTeamAdminEmails = []string{}
				form := url.Values{"team": {"teamname"}, "original_team": {"teamname"}, "oidc_emails": {""}, "oidc_reader_groups": {""}, "oidc_team_admin_groups": {""}, "oidc_subjects": {"injected"}, "oidc_domains": {"attacker.example"}}
				if !clearFields {
					expected.OIDCEmails, expected.OIDCReaderGroups, expected.OIDCTeamAdminGroups = []string{"new@example.com"}, []string{"new-readers"}, []string{"new-admins"}
					form.Set("oidc_emails", "new@example.com")
					form.Set("oidc_reader_groups", "new-readers")
					form.Set("oidc_team_admin_groups", "new-admins")
				}
				req := httptest.NewRequest(http.MethodPost, "/manage/access", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
				configs, _, err := router.accessConfigsWithUpsertedTeam(req, auth.Principal{CanAdmin: global, CanManageTeam: true, Team: "teamname"})
				if err != nil {
					t.Fatal(err)
				}
				if cfg := findTeamConfig(configs, "teamname"); cfg == nil || !reflect.DeepEqual(*cfg, expected) {
					t.Fatalf("unexpected access edit: got %#v want %#v", cfg, expected)
				}
			})
		}
	}
}

func TestManageTeamReaderEmailsRoundTrip(t *testing.T) {
	t.Parallel()
	st := newHTTPAPITestStore(t)
	client, base := newAccessManageClient(t, st, t.Context(), nil)
	for _, tc := range []struct {
		name, path, original, emails string
		omit                         bool
		want                         []string
	}{
		{name: "create", path: "/manage/teams/new", emails: " reader@example.com \n\n second@example.com ", want: []string{"reader@example.com", "second@example.com"}},
		{name: "replace", path: "/manage/access", original: "readerteam", emails: "new@example.com", want: []string{"new@example.com"}},
		{name: "preserve omitted field", path: "/manage/access", original: "readerteam", omit: true, want: []string{"new@example.com"}},
		{name: "clear", path: "/manage/access", original: "readerteam"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"action": {"save_team"}, "team": {"readerteam"}, "original_team": {tc.original}, "next": {"/manage/teams/readerteam/access"}}
			if !tc.omit {
				form.Set("oidc_emails", tc.emails)
			}
			resp, err := client.PostForm(base+tc.path, manageFormValues(t, client, base, form))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("save status=%d", resp.StatusCode)
			}
			configs, err := st.LoadTeamConfigs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cfg := findTeamConfig(configs, "readerteam")
			if cfg == nil || !slices.Equal(cfg.OIDCEmails, tc.want) {
				t.Fatalf("stored reader emails: %#v, want %v", cfg, tc.want)
			}
			body := getBody(t, client, base+"/manage/teams/readerteam/access")
			if !strings.Contains(body, `name="oidc_emails"`) || !strings.Contains(body, "OIDC team readers (emails)") {
				t.Fatal("access page misses reader email field")
			}
			for _, email := range tc.want {
				if !strings.Contains(body, email) {
					t.Fatalf("saved email %q not rendered", email)
				}
			}
		})
	}
}

func TestTeamAdminCanEditReaderEmailsOnlyInOwnTeam(t *testing.T) {
	t.Parallel()
	st := newHTTPAPITestStore(t)
	if err := st.ReplaceTeamConfigs(t.Context(), []auth.TeamConfig{
		{Team: "teamname", OIDCEmails: []string{"old@example.com"}, OIDCSubjects: []string{"stable-subject"}},
		{Team: "otherteam"},
	}); err != nil {
		t.Fatal(err)
	}
	router := &Router{modules: service.NewModuleService(st, testArtifactStorage{}, "modules", nil)}
	principal := auth.Principal{Team: "teamname", CanManageTeam: true, ManagedTeams: map[string]struct{}{"teamname": {}}}
	for _, tc := range []struct {
		team, emails string
		omit, denied bool
		want         []string
	}{
		{team: "teamname", emails: "new@example.com", want: []string{"new@example.com"}},
		{team: "teamname", omit: true, want: []string{"old@example.com"}},
		{team: "teamname"},
		{team: "otherteam", emails: "intruder@example.com", denied: true},
	} {
		form := url.Values{"team": {tc.team}, "original_team": {tc.team}}
		if !tc.omit {
			form.Set("oidc_emails", tc.emails)
		}
		req := httptest.NewRequest(http.MethodPost, "/manage/access", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		configs, _, err := router.accessConfigsWithUpsertedTeam(req, principal)
		if tc.denied {
			if err == nil {
				t.Fatal("team admin edited another team's reader emails")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		cfg := findTeamConfig(configs, tc.team)
		if cfg == nil || !slices.Equal(cfg.OIDCEmails, tc.want) || !slices.Equal(cfg.OIDCSubjects, []string{"stable-subject"}) {
			t.Fatalf("unexpected team edit: %#v", cfg)
		}
	}
}
