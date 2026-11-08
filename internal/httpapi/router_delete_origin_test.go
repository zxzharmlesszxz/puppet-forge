package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

func newDeleteOriginServer(t *testing.T, source string, global bool) (*store.SQLiteStore, *httptest.Server, *http.Client) {
	t.Helper()
	st := newHTTPAPITestStore(t)
	createModuleRelease(t, st, "teamname", "demo", "1.0.0")
	release, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	release.Source = source
	if _, err := st.CreateRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	configs := []auth.TeamConfig{
		{Team: "teamname", PublishTokens: []string{"publish-token"}, OIDCTeamAdminGroups: []string{"team-admins"}},
		{Team: auth.GlobalAdminTeam, OIDCAdminGroups: []string{"global-admins"}},
	}
	if err := st.ReplaceTeamConfigs(t.Context(), configs); err != nil {
		t.Fatal(err)
	}
	var issuerURL string
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"issuer": issuerURL, "authorization_endpoint": issuerURL + "/authorize", "token_endpoint": issuerURL + "/token", "jwks_uri": issuerURL + "/keys"}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(issuer.Close)
	issuerURL = issuer.URL
	secret := strings.Repeat("s", 32)
	webAuth, err := webauth.New(t.Context(), webauth.Config{IssuerURL: issuer.URL, ClientID: "test", CookieSecret: secret, StateStore: st, SessionStore: st, HTTPClient: issuer.Client()})
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(secret + "|oidc-session-id"))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("delete-session"))
	groups := []string{"team-admins"}
	if global {
		groups = append(groups, "global-admins")
	}
	now := time.Now().UTC()
	if err := st.CreateOIDCSession(t.Context(), store.OIDCSession{SessionHash: hex.EncodeToString(mac.Sum(nil)), Subject: "delete-admin", Groups: groups, CSRFSecret: "delete-csrf", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	svc := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(svc, nil, "", newAdminAuthorizer(t, configs...), webAuth, "admin-token", true, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Jar = newCookieJar(t)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar.SetCookies(target, []*http.Cookie{{Name: "puppet_forge_web", Value: "delete-session", Path: "/"}})
	return st, server, client
}

func TestManageDeletionRequiresGlobalAdminForUpstreamOrigin(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"local", "upstream"} {
		for _, global := range []bool{false, true} {
			for _, version := range []string{"", "1.0.0"} {
				for _, force := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/global=%v/module=%v/force=%v", source, global, version == "", force), func(t *testing.T) {
						t.Parallel()
						st, server, client := newDeleteOriginServer(t, source, global)
						path := "/manage/modules/teamname/demo"
						identity := "teamname/demo"
						action := "delete_module"
						if version != "" {
							path += "/versions/" + version
							identity += "@" + version
							action = "delete_release"
						}
						values := url.Values{}
						if force {
							values = url.Values{"force": {"true"}, "reason": {"broken archive"}, "confirmation": {identity}}
							action = "force_" + action
						}
						values = manageFormValues(t, client, server.URL, values)
						req, err := http.NewRequest(http.MethodPost, server.URL+path+"/delete", strings.NewReader(values.Encode()))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
						resp, err := client.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						body, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						allowed := global || source == "local"
						want := http.StatusNoContent
						if !allowed {
							want = http.StatusForbidden
							if _, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0"); err != nil {
								t.Fatalf("denied deletion changed release: %v", err)
							}
						} else if _, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
							t.Fatalf("allowed deletion left release: %v", err)
						}
						if resp.StatusCode != want {
							t.Fatalf("delete status=%d want=%d: %s", resp.StatusCode, want, body)
						}
						events, count, err := st.ListAuditEvents(t.Context(), nil, action, 10, 0)
						if err != nil || count != 1 || len(events) != 1 {
							t.Fatalf("delete audit count=%d events=%v err=%v", count, events, err)
						}
						if !allowed && (events[0].Result != "failure" || events[0].Reason != "forbidden") {
							t.Fatalf("denial audit = %#v", events[0])
						}
					})
				}
			}
		}
	}
}

func TestManageUpstreamDeletionControlsAndConfirmationAreRestricted(t *testing.T) {
	t.Parallel()
	_, server, client := newDeleteOriginServer(t, "upstream", false)
	for _, path := range []string{"/manage/modules", "/manage/modules/teamname/demo/card"} {
		body := getBody(t, client, server.URL+path)
		for _, forbidden := range []string{"Delete module", "/force-delete", "/versions/1.0.0/delete"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("upstream card exposes %q: %s", forbidden, body)
			}
		}
	}
	for _, path := range []string{"/manage/modules/teamname/demo/force-delete", "/manage/modules/teamname/demo/versions/1.0.0/force-delete"} {
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("upstream confirmation status=%d, want 403", resp.StatusCode)
		}
	}
}

func TestAPIGlobalAdminCanDeleteUpstreamResources(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "1.0.0"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("module=%v/force=%v", version == "", force), func(t *testing.T) {
				t.Parallel()
				st, server, client := newDeleteOriginServer(t, "upstream", true)
				path, identity := "/api/v1/modules/teamname/demo", "teamname/demo"
				if version != "" {
					path += "/versions/" + version
					identity += "@" + version
				}
				if force {
					path += "?" + url.Values{"force": {"true"}, "reason": {"broken archive"}, "confirmation": {identity}}.Encode()
				}
				req, err := http.NewRequest(http.MethodDelete, server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer publish-token")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("publish token upstream deletion status=%d, want 403", resp.StatusCode)
				}
				if _, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0"); err != nil {
					t.Fatalf("forbidden API deletion changed release: %v", err)
				}
				req.Header.Set("Authorization", "Bearer admin-token")
				resp, err = client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("global upstream deletion status=%d: %s", resp.StatusCode, body)
				}
			})
		}
	}
}
