package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
)

func TestManageLoginSourceValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, origin, referer, fetchSite string
		allowed                          bool
	}{
		{name: "same origin", origin: "https://forge.example", allowed: true},
		{name: "referer fallback", referer: "https://forge.example/manage/login", allowed: true},
		{name: "cross origin", origin: "https://attacker.example"},
		{name: "same site is not same origin", origin: "https://other.forge.example", fetchSite: "same-site"},
		{name: "foreign referer", referer: "https://attacker.example/login"},
		{name: "origin overrides referer", origin: "https://attacker.example", referer: "https://forge.example/manage/login"},
		{name: "cross site metadata overrides origin", origin: "https://forge.example", fetchSite: "cross-site"},
		{name: "missing source"},
		{name: "opaque origin", origin: "null"},
		{name: "opaque cross site", origin: "null", fetchSite: "cross-site"},
		{name: "malformed origin", origin: ":invalid"},
		{name: "insecure origin", origin: "http://forge.example"},
		{name: "same origin metadata", fetchSite: "same-origin", allowed: true},
		{name: "opaque same origin metadata", origin: "null", fetchSite: "same-origin", allowed: true},
		{name: "proxy rewritten origin", origin: "https://public.example", fetchSite: "same-origin", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://forge.example/manage/login", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Referer", tc.referer)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			allowed, reason := checkManageLoginRequestSource(req, "")
			if allowed != tc.allowed || (!allowed && reason == "") {
				t.Fatalf("source allowed=%v reason=%q, want allowed=%v", allowed, reason, tc.allowed)
			}
		})
	}
}

func TestManageLoginRejectsCSRFWithoutReplacingSession(t *testing.T) {
	t.Parallel()
	st := newHTTPAPITestStore(t)
	modules := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(modules, nil, "", newAdminAuthorizer(t), nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: server.Client().Transport, Jar: newCookieJar(t)}
	postManageToken(t, client, server.URL, "admin-token")
	for _, tc := range []struct{ name, origin, referer, fetchSite string }{
		{name: "cross origin", origin: "https://attacker.example"},
		{name: "cross site", origin: server.URL, fetchSite: "cross-site"},
		{name: "opaque origin", origin: "null"},
		{name: "missing source"},
		{name: "foreign referer", referer: "https://attacker.example/login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, jar := range []http.CookieJar{nil, client.Jar} {
				noRedirect := &http.Client{Transport: server.Client().Transport, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/manage/login", strings.NewReader(url.Values{"token": {"admin-token"}}.Encode()))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Origin", tc.origin)
				req.Header.Set("Referer", tc.referer)
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
				resp, err := noRedirect.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" || len(resp.Cookies()) != 0 {
					t.Fatalf("rejected login: status=%d redirect=%q cookies=%d", resp.StatusCode, resp.Header.Get("Location"), len(resp.Cookies()))
				}
				if strings.Contains(string(body), "admin-token") {
					t.Fatal("rejected login exposed submitted credentials")
				}
			}
		})
	}
	resp, err := client.Get(server.URL + "/manage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/manage" {
		t.Fatal("rejected login replaced or revoked the existing session")
	}
}

func TestManageLoginOpaqueOriginRequiresFormSecret(t *testing.T) {
	t.Parallel()
	st := newHTTPAPITestStore(t)
	modules := service.NewModuleService(st, testArtifactStorage{}, "modules", nil)
	server := httptest.NewServer(newTestRouter(modules, nil, "", newAdminAuthorizer(t), nil, "admin-token", false, defaultActiveReleaseTTL))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name, origin, fetchSite string
		omitCookie, omitToken   bool
		wrongToken              bool
		want                    int
	}{
		{name: "Safari opaque origin", origin: "null", want: http.StatusFound},
		{name: "privacy headers omitted", want: http.StatusFound},
		{name: "opaque missing cookie", origin: "null", omitCookie: true, want: http.StatusForbidden},
		{name: "opaque missing form secret", origin: "null", omitToken: true, want: http.StatusForbidden},
		{name: "opaque wrong form secret", origin: "null", wrongToken: true, want: http.StatusForbidden},
		{name: "foreign origin with valid secret", origin: "https://attacker.example", want: http.StatusForbidden},
		{name: "cross site with valid secret", origin: "null", fetchSite: "cross-site", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: server.Client().Transport, Jar: newCookieJar(t), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			page := getBody(t, client, server.URL+"/manage/login")
			match := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(page)
			if len(match) != 2 || len(match[1]) < 32 {
				t.Fatal("login form is missing a CSRF secret")
			}
			form := url.Values{"token": {"admin-token"}, "csrf_token": {match[1]}}
			if tc.omitToken {
				form.Del("csrf_token")
			}
			if tc.wrongToken {
				form.Set("csrf_token", strings.Repeat("x", 43))
			}
			if tc.omitCookie {
				client.Jar = nil
			}
			req, err := http.NewRequest(http.MethodPost, server.URL+"/manage/login", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("login status=%d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusFound {
				resp, err = client.Get(server.URL + "/manage")
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("manage status=%d after successful login", resp.StatusCode)
				}
			} else if len(resp.Cookies()) != 0 {
				t.Fatal("rejected login changed cookies")
			}
		})
	}
}
