package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestPublishAllowedSources(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/v1/modules", "/v3/releases", "/manage/modules"} {
		for _, tc := range []struct {
			name, remote, forwarded string
			trust, unrestricted     bool
			allowed                 bool
			async                   bool
		}{
			{name: "disabled", remote: "198.51.100.1:1234", unrestricted: true, allowed: true},
			{name: "direct IPv4", remote: "192.0.2.7:1234", allowed: true},
			{name: "direct IPv6", remote: "[2001:db8::7]:1234", allowed: true},
			{name: "mapped IPv4", remote: "[::ffff:192.0.2.7]:1234", allowed: true},
			{name: "denied", remote: "198.51.100.1:1234"},
			{name: "async allowed", remote: "192.0.2.7:1234", allowed: true, async: true},
			{name: "async denied", remote: "198.51.100.1:1234", async: true},
			{name: "invalid peer", remote: "invalid"},
			{name: "forged header", remote: "198.51.100.1:1234", forwarded: "192.0.2.7", trust: true},
			{name: "forwarding disabled", remote: "10.0.0.1:1234", forwarded: "192.0.2.7"},
			{name: "trusted proxy", remote: "10.0.0.1:1234", forwarded: "192.0.2.7", trust: true, allowed: true},
			{name: "spoofed first hop", remote: "10.0.0.1:1234", forwarded: "192.0.2.7, 198.51.100.1", trust: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				st := newHTTPAPITestStore(t)
				authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", PublishTokens: []string{"publish-token"}})
				handler := newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL, func(r *Router) {
					if !tc.unrestricted {
						r.publishAllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}
					}
					r.trustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
					r.trustForwardedHeaders = tc.trust
				})
				csrf := ""
				var cookies []*http.Cookie
				if path == "/manage/modules" {
					server := httptest.NewServer(handler)
					t.Cleanup(server.Close)
					client := server.Client()
					client.Jar = newCookieJar(t)
					postManageToken(t, client, server.URL, "admin-token")
					csrf = manageCSRFToken(t, client, server.URL)
					target, err := url.Parse(server.URL + "/manage")
					if err != nil {
						t.Fatal(err)
					}
					cookies = client.Jar.Cookies(target)
				}
				archive := buildV3PublishArchive(t, "teamname", "module", "1.0.0")
				req := publishSourceRequest(t, path, archive, csrf)
				req.RemoteAddr = tc.remote
				req.Header.Set("Authorization", "Bearer publish-token")
				if path == "/manage/modules" {
					req.Header.Set("Authorization", "Bearer admin-token")
				}
				req.Header.Set("X-Forwarded-For", tc.forwarded)
				if tc.async {
					req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
				}
				for _, cookie := range cookies {
					req.AddCookie(cookie)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				want := http.StatusForbidden
				if tc.allowed {
					want = http.StatusCreated
					if path == "/manage/modules" {
						want = http.StatusFound
						if tc.async {
							want = http.StatusNoContent
						}
					}
				}
				if rec.Code != want {
					t.Fatalf("status=%d, want %d: %s", rec.Code, want, rec.Body.String())
				}
				_, err := st.GetRelease(t.Context(), "teamname", "module", "1.0.0")
				if tc.allowed && err != nil || !tc.allowed && !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("release after publish: %v", err)
				}
				if !tc.allowed {
					events, total, err := st.ListAuditEvents(t.Context(), nil, "publish_source_forbidden", 10, 0)
					if err != nil || total != 1 || len(events) != 1 {
						t.Fatalf("denial audit: total=%d events=%v err=%v", total, events, err)
					}
					if events[0].Result != "failure" || strings.Contains(rec.Body.String(), "publish-token") {
						t.Fatal("invalid denial response or audit result")
					}
				}
				// The publish allowlist must not restrict public reads.
				read := httptest.NewRequest(http.MethodGet, "/api/v1/modules", nil)
				read.RemoteAddr = tc.remote
				readRec := httptest.NewRecorder()
				handler.ServeHTTP(readRec, read)
				if readRec.Code != http.StatusOK {
					t.Fatalf("read status=%d", readRec.Code)
				}
			})
		}
	}
}

type unreadPublishBody struct{ t *testing.T }

func (b unreadPublishBody) Read([]byte) (int, error) {
	b.t.Error("denied publish read the request body")
	return 0, io.EOF
}

func TestPublishSourceRejectsBeforeReadingBody(t *testing.T) {
	st := newHTTPAPITestStore(t)
	authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", PublishTokens: []string{"publish-token"}})
	handler := newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "", authorizer, nil, "admin-token", false, defaultActiveReleaseTTL, func(r *Router) {
		r.publishAllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Jar = newCookieJar(t)
	postManageToken(t, client, server.URL, "admin-token")
	target, err := url.Parse(server.URL + "/manage")
	if err != nil {
		t.Fatal(err)
	}
	for index, path := range []string{"/api/v1/modules", "/v3/releases", "/manage/modules"} {
		req := httptest.NewRequest(http.MethodPost, path, unreadPublishBody{t})
		req.RemoteAddr = "198.51.100.1:1234"
		req.Header.Set("Authorization", "Bearer publish-token")
		if path == "/manage/modules" {
			req.Header.Set("Authorization", "Bearer admin-token")
		}
		for _, cookie := range client.Jar.Cookies(target) {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d: %s", path, rec.Code, rec.Body.String())
		}
		events, total, err := st.ListAuditEvents(t.Context(), nil, "publish_source_forbidden", 10, 0)
		if err != nil || total != index+1 || len(events) != index+1 {
			t.Fatalf("%s source denial audit: total=%d events=%v err=%v", path, total, events, err)
		}
	}
}

func TestAllowedPublishSourceStillRequiresAuthorization(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/v1/modules", "/v3/releases"} {
		for _, token := range []string{"", "publish-token"} {
			t.Run(path+"/"+token, func(t *testing.T) {
				t.Parallel()
				st := newHTTPAPITestStore(t)
				authorizer := newAdminAuthorizer(t, auth.TeamConfig{Team: "teamname", PublishTokens: []string{"publish-token"}})
				handler := newTestRouter(service.NewModuleService(st, testArtifactStorage{}, "modules", nil), nil, "", authorizer, nil, "admin-token", true, defaultActiveReleaseTTL, func(r *Router) {
					r.publishAllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
				})
				archive := buildV3PublishArchive(t, "otherteam", "module", "1.0.0")
				req := publishSourceRequest(t, path, archive, "")
				req.RemoteAddr = "192.0.2.7:1234"
				want := http.StatusUnauthorized
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
					want = http.StatusForbidden
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != want {
					t.Fatalf("status=%d, want %d: %s", rec.Code, want, rec.Body.String())
				}
				if _, err := st.GetRelease(t.Context(), "otherteam", "module", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("unauthorized release persisted: %v", err)
				}
			})
		}
	}
}

func publishSourceRequest(t *testing.T, path string, archive []byte, csrf string) *http.Request {
	t.Helper()
	body, contentType := buildPublishMultipart(t, "", "module", "1.0.0", archive, csrf)
	if path == "/v3/releases" {
		payload, err := json.Marshal(map[string][]byte{"file": archive})
		if err != nil {
			t.Fatal(err)
		}
		body, contentType = bytes.NewBuffer(payload), "application/json"
	}
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	return req
}
