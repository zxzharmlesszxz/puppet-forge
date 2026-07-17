package webauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"

	"github.com/gorilla/securecookie"
	"golang.org/x/oauth2"
)

func TestStateCookieRoundTripUsesDecodablePayload(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		cookies:   securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		stateName: "puppet_forge_state",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/auth/login", nil)
	if err := auth.setStateCookie(rec, req, "state-value", "/manage"); err != nil {
		t.Fatalf("setStateCookie() error = %v", err)
	}

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one state cookie, got %d", len(cookies))
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=code&state=state-value", nil)
	callbackReq.AddCookie(cookies[0])

	stateCookie, err := callbackReq.Cookie(auth.stateName)
	if err != nil {
		t.Fatalf("state cookie missing: %v", err)
	}

	statePayload := map[string]string{}
	if err := auth.cookies.Decode(auth.stateName, stateCookie.Value, &statePayload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if statePayload["state"] != "state-value" {
		t.Fatalf("unexpected state: %q", statePayload["state"])
	}
	if statePayload["next"] != "/manage" {
		t.Fatalf("unexpected next: %q", statePayload["next"])
	}
}

func TestFailStateClearsCookieAndStopsAtLoginPage(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{stateName: "puppet_forge_state"}
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	rec := httptest.NewRecorder()

	auth.failState(rec, req, "OIDC state mismatch. Start login again.")

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/manage/login?error=") {
		t.Fatalf("unexpected redirect location: %s", location)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == auth.stateName && cookie.MaxAge != -1 {
			t.Fatalf("expected state cookie to be cleared, got MaxAge=%d", cookie.MaxAge)
		}
	}
}

func TestSessionCookieRoundTripAndLogout(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		cookies:     securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		cookieName:  "puppet_forge_web",
		sessionName: "puppet_forge_web",
		stateName:   "puppet_forge_state",
	}

	rec := httptest.NewRecorder()
	session := Session{
		Email:  "dev@example.com",
		Name:   "Dev User",
		Sub:    "subject",
		Groups: []string{"teamname-devops"},
	}
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/manage", nil)
	if err := auth.setSessionCookie(rec, req, session); err != nil {
		t.Fatalf("setSessionCookie() error = %v", err)
	}

	for _, cookie := range rec.Result().Cookies() {
		req.AddCookie(cookie)
	}

	got, ok := auth.Session(req)
	if !ok {
		t.Fatal("expected session")
	}
	if got.Email != session.Email || got.Sub != session.Sub || len(got.Groups) != 1 || got.Groups[0] != "teamname-devops" {
		t.Fatalf("unexpected session: %#v", got)
	}

	logoutRec := httptest.NewRecorder()
	auth.Logout(logoutRec, req, "/")
	resp := logoutRec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected logout redirect, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("unexpected logout location: %s", resp.Header.Get("Location"))
	}
	cleared := map[string]bool{}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge == -1 {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[auth.sessionName] || !cleared[auth.stateName] {
		t.Fatalf("logout did not clear oidc cookies: %#v", resp.Cookies())
	}
}

func TestEncryptedSessionCookieRoundTrip(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		cookies:     newSecureCookie("test-cookie-secret-32-bytes-long"),
		cookieName:  "puppet_forge_web",
		sessionName: "puppet_forge_web",
		stateName:   "puppet_forge_state",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/manage", nil)
	session := Session{Email: "dev@example.com", Name: "Dev User", Sub: "subject", Groups: []string{"teamname-devops"}}
	if err := auth.setSessionCookie(rec, req, session); err != nil {
		t.Fatalf("setSessionCookie() error = %v", err)
	}

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	if len(resp.Cookies()) != 1 {
		t.Fatalf("expected one session cookie, got %d", len(resp.Cookies()))
	}
	if strings.Contains(resp.Cookies()[0].Value, session.Email) {
		t.Fatalf("session cookie contains plaintext email: %q", resp.Cookies()[0].Value)
	}

	req.AddCookie(resp.Cookies()[0])
	got, ok := auth.Session(req)
	if !ok {
		t.Fatal("expected encrypted session cookie to decode")
	}
	if got.Email != session.Email || got.Sub != session.Sub || len(got.Groups) != 1 || got.Groups[0] != "teamname-devops" {
		t.Fatalf("unexpected encrypted session: %#v", got)
	}
}

func TestLogoutRedirectsToProviderEndSessionEndpoint(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		oauth2: oauth2.Config{
			ClientID: "forge",
		},
		cookies:     securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		cookieName:  "puppet_forge_web",
		sessionName: "puppet_forge_web",
		stateName:   "puppet_forge_state",
		logoutURL:   "https://auth.example.com/application/o/forge/end-session/",
	}

	req := httptest.NewRequest(http.MethodPost, "http://internal/manage/logout", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forge.example.com")
	rec := httptest.NewRecorder()

	auth.Logout(rec, req, "/manage/login")

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected logout redirect, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "https://auth.example.com/application/o/forge/end-session/?") {
		t.Fatalf("unexpected logout location: %s", location)
	}
	if !strings.Contains(location, "client_id=forge") {
		t.Fatalf("logout location misses client_id: %s", location)
	}
	if !strings.Contains(location, "post_logout_redirect_uri=https%3A%2F%2Fforge.example.com%2Fmanage%2Flogin") {
		t.Fatalf("logout location misses post_logout_redirect_uri: %s", location)
	}
	cleared := map[string]bool{}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge == -1 {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[auth.sessionName] || !cleared[auth.stateName] {
		t.Fatalf("provider logout did not clear local oidc cookies: %#v", resp.Cookies())
	}
}

func TestOIDCCookiesUseSecureFlagWhenRequestIsHTTPS(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		cookies:     securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		cookieName:  "puppet_forge_web",
		sessionName: "puppet_forge_web",
		stateName:   "puppet_forge_state",
	}

	req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forge.example.com")
	rec := httptest.NewRecorder()
	if err := auth.setStateCookie(rec, req, "state-value", "/manage"); err != nil {
		t.Fatalf("setStateCookie() error = %v", err)
	}
	if err := auth.setSessionCookie(rec, req, Session{Email: "dev@example.com"}); err != nil {
		t.Fatalf("setSessionCookie() error = %v", err)
	}
	auth.ClearSessionForRequest(rec, req)

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	for _, cookie := range resp.Cookies() {
		if !cookie.Secure {
			t.Fatalf("expected cookie %q to be Secure: %#v", cookie.Name, cookie)
		}
	}
}

func TestExternalBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      *http.Request
		fallback string
		want     string
	}{
		{
			name: "forwarded headers",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
				req.Header.Set("X-Forwarded-Proto", "https")
				req.Header.Set("X-Forwarded-Host", "forge.example.com")
				return req
			}(),
			want: "https://forge.example.com",
		},
		{
			name: "forwarded standard header",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
				req.Header.Set("Forwarded", `for=10.0.0.1;proto=https;host="forge.alt.example.com"`)
				return req
			}(),
			want: "https://forge.alt.example.com",
		},
		{
			name: "request host",
			req:  httptest.NewRequest(http.MethodGet, "http://forge.127.0.0.1.nip.io:8080/manage", nil),
			want: "http://forge.127.0.0.1.nip.io:8080",
		},
		{
			name: "fallback",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
				req.Host = ""
				return req
			}(),
			fallback: "https://forge.example.com/",
			want:     "https://forge.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := httputil.ExternalBaseURL(tt.req, tt.fallback)
			if got != tt.want {
				t.Fatalf("externalBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOAuth2ConfigBuildsRedirectURLFromRequest(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		oauth2: oauth2.Config{
			ClientID:     "forge",
			ClientSecret: "secret",
		},
	}
	req := httptest.NewRequest(http.MethodGet, "http://internal/auth/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forge.example.com")

	cfg := auth.oauth2Config(req)

	if cfg.RedirectURL != "https://forge.example.com/auth/callback" {
		t.Fatalf("unexpected redirect URL: %s", cfg.RedirectURL)
	}
}

func TestRequireRedirectsUnauthenticatedRequest(t *testing.T) {
	t.Parallel()

	auth := &OIDCAuth{
		cookies:     securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		cookieName:  "puppet_forge_web",
		sessionName: "puppet_forge_web",
	}

	handler := auth.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/manage?x=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/auth/login?next=") {
		t.Fatalf("unexpected redirect location: %s", location)
	}
}

func TestSafeRedirectPathRejectsProtocolRelativeURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		next string
		want string
	}{
		{name: "relative path", next: "/manage?x=1", want: "/manage?x=1"},
		{name: "empty", next: "", want: "/"},
		{name: "absolute URL", next: "https://evil.example.com", want: "/"},
		{name: "protocol-relative URL", next: "//evil.example.com/path", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := safeRedirectPath(tt.next); got != tt.want {
				t.Fatalf("safeRedirectPath(%q) = %q, want %q", tt.next, got, tt.want)
			}
		})
	}
}

func TestLoginReturns500WhenRandomGenerationFails(t *testing.T) {
	// Not parallel due to global testableRandomString mock
	auth := &OIDCAuth{
		cookies:   securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		stateName: "puppet_forge_state",
		oauth2: oauth2.Config{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			RedirectURL:  "http://localhost/auth/callback",
			Scopes:       []string{"openid", "profile", "email"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://oidc.example.com/oauth2/auth",
				TokenURL: "https://oidc.example.com/oauth2/token",
			},
		},
	}

	// Mock random generation to fail
	oldRandom := testableRandomString
	testableRandomString = func(size int) (string, error) {
		return "", errors.New("simulated random generation failure")
	}
	t.Cleanup(func() {
		testableRandomString = oldRandom
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/auth/login", nil)

	auth.Login(rec, req)

	resp := rec.Result()
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when random generation fails, got %d", resp.StatusCode)
	}
}

func TestRandomStringRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	if _, err := randomString(-1); err == nil {
		t.Fatal("expected error for negative random string size")
	}
}
