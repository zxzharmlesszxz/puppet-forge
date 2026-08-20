package webauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	internalAuth "github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"

	"github.com/go-jose/go-jose/v4"
	"github.com/gorilla/securecookie"
	"golang.org/x/oauth2"
)

type oidcProviderFixture struct {
	server         *httptest.Server
	privateKey     *rsa.PrivateKey
	mu             sync.Mutex
	expectedNonce  string
	issuerOverride string
	audience       string
	nonceOverride  string
	tokenCalls     int
	codeVerifier   string
}

func newOIDCProviderFixture(t *testing.T) *oidcProviderFixture {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	fixture := &oidcProviderFixture{privateKey: privateKey, audience: "forge-client"}
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"issuer":                                fixture.server.URL,
			"authorization_endpoint":                fixture.server.URL + "/authorize",
			"token_endpoint":                        fixture.server.URL + "/token",
			"jwks_uri":                              fixture.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &privateKey.PublicKey,
			KeyID:     "test-key",
			Algorithm: "RS256",
			Use:       "sig",
		}}})
	})
	mux.HandleFunc("/token", fixture.handleToken)
	return fixture
}

func (f *oidcProviderFixture) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.tokenCalls++
	f.codeVerifier = r.Form.Get("code_verifier")
	issuer := f.server.URL
	if f.issuerOverride != "" {
		issuer = f.issuerOverride
	}
	nonce := f.expectedNonce
	if f.nonceOverride != "" {
		nonce = f.nonceOverride
	}
	audience := f.audience
	f.mu.Unlock()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	claims, err := json.Marshal(map[string]any{
		"iss": issuer, "aud": audience, "sub": "user-subject", "email": "user@example.com",
		"name": "Test User", "groups": []string{"teamname-admins"}, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	signed, err := signer.Sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rawIDToken, err := signed.CompactSerialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeTestJSONResponse(w, map[string]any{
		"access_token": "access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": rawIDToken,
	})
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	writeTestJSONResponse(w, value)
}

func writeTestJSONResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func newOIDCTestAuth(t *testing.T, fixture *oidcProviderFixture) (*OIDCAuth, store.Store) {
	t.Helper()
	hasher, err := internalAuth.NewTokenHasher("test-access-token-pepper-32-bytes")
	if err != nil {
		t.Fatalf("NewTokenHasher() error = %v", err)
	}
	stateStore, err := store.NewSQLiteStore("sqlite://:memory:", hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(stateStore.Close)
	oidcAuth, err := New(context.Background(), Config{
		IssuerURL:         fixture.server.URL,
		ClientID:          "forge-client",
		ClientSecret:      "client-secret",
		RedirectURL:       "https://forge.example.com/auth/callback",
		CookieSecret:      "test-oidc-cookie-secret-32-bytes",
		SigningAlgorithms: []string{"RS256"},
		StateStore:        stateStore,
		SessionStore:      stateStore,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return oidcAuth, stateStore
}

func newOIDCSessionTestAuth(t *testing.T) *OIDCAuth {
	t.Helper()
	hasher, err := internalAuth.NewTokenHasher("test-access-token-pepper-32-bytes")
	if err != nil {
		t.Fatalf("NewTokenHasher() error = %v", err)
	}
	backend, err := store.NewSQLiteStore("sqlite://:memory:", hasher)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(backend.Close)
	sessionKey := sha256.Sum256([]byte("test-cookie-secret|oidc-session-id"))
	return &OIDCAuth{
		cookies:    securecookie.New([]byte("test-cookie-secret-32-bytes-long"), nil),
		cookieName: "puppet_forge_web", sessionName: "puppet_forge_web", stateName: "puppet_forge_state",
		sessionStore: backend, sessionKey: sessionKey[:],
	}
}

func setOIDCTestSession(t *testing.T, oidcAuth *OIDCAuth, rec *httptest.ResponseRecorder, req *http.Request, session Session) {
	t.Helper()
	sessionID, err := oidcAuth.createSession(req.Context(), session)
	if err != nil {
		t.Fatalf("createSession() error = %v", err)
	}
	oidcAuth.setSessionCookie(rec, req, sessionID)
}

// Plaintext HTTP is required to verify non-TLS cookie behavior.
//
//goland:noinspection HttpUrlsUsage
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

func TestOIDCFlowUsesNoncePKCEAndRejectsStateReplay(t *testing.T) {
	t.Parallel()

	fixture := newOIDCProviderFixture(t)
	oidcAuth, _ := newOIDCTestAuth(t, fixture)
	loginReq := httptest.NewRequest(http.MethodGet, "https://forge.example.com/auth/login?next=/manage", nil)
	loginRec := httptest.NewRecorder()
	oidcAuth.Login(loginRec, loginReq)
	loginResp := loginRec.Result()
	defer func() { _ = loginResp.Body.Close() }()
	if loginResp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("Login() status = %d body=%s", loginResp.StatusCode, body)
	}
	authorizeURL, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := authorizeURL.Query()
	state := query.Get("state")
	nonce := query.Get("nonce")
	challenge := query.Get("code_challenge")
	if state == "" || nonce == "" || challenge == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL misses state/nonce/PKCE: %s", authorizeURL.String())
	}
	fixture.mu.Lock()
	fixture.expectedNonce = nonce
	fixture.mu.Unlock()
	stateCookie := findResponseCookie(t, loginResp, oidcAuth.stateName)

	callbackPath := "/auth/callback?code=authorization-code&state=" + url.QueryEscape(state)
	callbackReq := httptest.NewRequest(http.MethodGet, callbackPath, nil)
	callbackReq.AddCookie(stateCookie)
	callbackRec := httptest.NewRecorder()
	oidcAuth.Callback(callbackRec, callbackReq)
	callbackResp := callbackRec.Result()
	defer func() { _ = callbackResp.Body.Close() }()
	if callbackResp.StatusCode != http.StatusFound || callbackResp.Header.Get("Location") != "/manage" {
		body, _ := io.ReadAll(callbackResp.Body)
		t.Fatalf("Callback() status=%d location=%q body=%s", callbackResp.StatusCode, callbackResp.Header.Get("Location"), body)
	}

	fixture.mu.Lock()
	verifier := fixture.codeVerifier
	tokenCalls := fixture.tokenCalls
	fixture.mu.Unlock()
	if tokenCalls != 1 || verifier == "" || oauth2.S256ChallengeFromVerifier(verifier) != challenge {
		t.Fatalf("token exchange verifier=%q calls=%d challenge=%q", verifier, tokenCalls, challenge)
	}

	replayReq := httptest.NewRequest(http.MethodGet, callbackPath, nil)
	replayReq.AddCookie(stateCookie)
	replayRec := httptest.NewRecorder()
	oidcAuth.Callback(replayRec, replayReq)
	replayResp := replayRec.Result()
	defer func() { _ = replayResp.Body.Close() }()
	if replayResp.StatusCode != http.StatusFound || !strings.HasPrefix(replayResp.Header.Get("Location"), "/manage/login?error=") {
		t.Fatalf("replayed callback status=%d location=%q", replayResp.StatusCode, replayResp.Header.Get("Location"))
	}
	fixture.mu.Lock()
	tokenCalls = fixture.tokenCalls
	fixture.mu.Unlock()
	if tokenCalls != 1 {
		t.Fatalf("replayed callback reached token endpoint: calls=%d", tokenCalls)
	}
}

func TestOIDCCallbackRejectsInvalidIDTokenBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*oidcProviderFixture)
	}{
		{name: "wrong issuer", configure: func(f *oidcProviderFixture) { f.issuerOverride = "https://wrong-issuer.example.com" }},
		{name: "wrong audience", configure: func(f *oidcProviderFixture) { f.audience = "other-client" }},
		{name: "mismatched nonce", configure: func(f *oidcProviderFixture) { f.nonceOverride = "wrong-nonce" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newOIDCProviderFixture(t)
			test.configure(fixture)
			oidcAuth, _ := newOIDCTestAuth(t, fixture)
			loginReq := httptest.NewRequest(http.MethodGet, "https://forge.example.com/auth/login?next=/manage", nil)
			loginRec := httptest.NewRecorder()
			oidcAuth.Login(loginRec, loginReq)
			loginResp := loginRec.Result()
			defer func() { _ = loginResp.Body.Close() }()
			authorizeURL, err := url.Parse(loginResp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse authorization URL: %v", err)
			}
			fixture.mu.Lock()
			fixture.expectedNonce = authorizeURL.Query().Get("nonce")
			fixture.mu.Unlock()
			callbackReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authorization-code&state="+url.QueryEscape(authorizeURL.Query().Get("state")), nil)
			callbackReq.AddCookie(findResponseCookie(t, loginResp, oidcAuth.stateName))
			callbackRec := httptest.NewRecorder()
			oidcAuth.Callback(callbackRec, callbackReq)
			callbackResp := callbackRec.Result()
			defer func() { _ = callbackResp.Body.Close() }()
			if callbackResp.StatusCode != http.StatusUnauthorized {
				body, _ := io.ReadAll(callbackResp.Body)
				t.Fatalf("Callback() status=%d body=%s", callbackResp.StatusCode, body)
			}
		})
	}
}

func TestOIDCCallbackRejectsExpiredStateBeforeTokenExchange(t *testing.T) {
	t.Parallel()

	fixture := newOIDCProviderFixture(t)
	oidcAuth, stateStore := newOIDCTestAuth(t, fixture)
	state := "expired-state"
	now := time.Now().UTC()
	if err := stateStore.CreateOIDCState(context.Background(), store.OIDCState{
		StateHash: oidcAuth.hashState(state), Nonce: "nonce", PKCEVerifier: "verifier", NextPath: "/manage",
		CreatedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateOIDCState() error = %v", err)
	}
	stateCookieRec := httptest.NewRecorder()
	stateCookieReq := httptest.NewRequest(http.MethodGet, "https://forge.example.com/auth/login", nil)
	if err := oidcAuth.setStateCookie(stateCookieRec, stateCookieReq, state, "/manage"); err != nil {
		t.Fatalf("setStateCookie() error = %v", err)
	}
	stateCookieResp := stateCookieRec.Result()
	defer func() { _ = stateCookieResp.Body.Close() }()
	callbackReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=authorization-code&state="+state, nil)
	callbackReq.AddCookie(findResponseCookie(t, stateCookieResp, oidcAuth.stateName))
	callbackRec := httptest.NewRecorder()
	oidcAuth.Callback(callbackRec, callbackReq)
	callbackResp := callbackRec.Result()
	defer func() { _ = callbackResp.Body.Close() }()
	if callbackResp.StatusCode != http.StatusFound || !strings.HasPrefix(callbackResp.Header.Get("Location"), "/manage/login?error=") {
		t.Fatalf("expired callback status=%d location=%q", callbackResp.StatusCode, callbackResp.Header.Get("Location"))
	}
	fixture.mu.Lock()
	tokenCalls := fixture.tokenCalls
	fixture.mu.Unlock()
	if tokenCalls != 0 {
		t.Fatalf("expired callback reached token endpoint: calls=%d", tokenCalls)
	}
}

func findResponseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.MaxAge >= 0 {
			responseCookie := *cookie
			return &responseCookie
		}
	}
	t.Fatalf("response cookie %q not found", name)
	return nil
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

// Plaintext HTTP is required to verify non-TLS cookie behavior.
//
//goland:noinspection HttpUrlsUsage
func TestSessionCookieRoundTripAndLogout(t *testing.T) {
	t.Parallel()

	auth := newOIDCSessionTestAuth(t)

	rec := httptest.NewRecorder()
	session := Session{
		Email:  "dev@example.com",
		Name:   "Dev User",
		Sub:    "subject",
		Groups: []string{"teamname-devops"},
	}
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/manage", nil)
	setOIDCTestSession(t, auth, rec, req, session)

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
	if _, ok := auth.Session(req); ok {
		t.Fatal("logout did not revoke copied OIDC session cookie")
	}
}

// Plaintext HTTP is required to verify non-TLS cookie behavior.
//
//goland:noinspection HttpUrlsUsage
func TestOIDCSessionCookieIsOpaqueAndContainsNoPII(t *testing.T) {
	t.Parallel()

	auth := newOIDCSessionTestAuth(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://forge.example.com/manage", nil)
	session := Session{Email: "dev@example.com", Name: "Dev User", Sub: "subject", Groups: []string{"teamname-devops"}}
	setOIDCTestSession(t, auth, rec, req, session)

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
		t.Fatal("expected opaque session cookie to resolve")
	}
	if got.Email != session.Email || got.Sub != session.Sub || len(got.Groups) != 1 || got.Groups[0] != "teamname-devops" {
		t.Fatalf("unexpected encrypted session: %#v", got)
	}
}

func TestNormalizeScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		scopes []string
		want   []string
	}{
		{name: "defaults", want: []string{"openid", "profile", "email"}},
		{name: "adds openid", scopes: []string{"profile", "groups"}, want: []string{"openid", "profile", "groups"}},
		{name: "deduplicates", scopes: []string{"openid", "groups", "openid", " groups "}, want: []string{"openid", "groups"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeScopes(test.scopes)
			if strings.Join(got, " ") != strings.Join(test.want, " ") {
				t.Fatalf("normalizeScopes() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// Plaintext HTTP models the internal listener behind a TLS-terminating proxy.
//
//goland:noinspection HttpUrlsUsage
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

func TestLogoutOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		logoutURL string
		want      string
	}{
		{name: "https", logoutURL: "https://auth.example.com/application/o/forge/end-session/?tenant=one", want: "https://auth.example.com"},
		{name: "port", logoutURL: "http://auth.example.com:8080/logout", want: "http://auth.example.com:8080"},
		{name: "empty"},
		{name: "relative", logoutURL: "/logout"},
		{name: "credentials", logoutURL: "https://user:secret@auth.example.com/logout"},
		{name: "unsupported scheme", logoutURL: "javascript:alert(1)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			auth := &OIDCAuth{logoutURL: test.logoutURL}
			if got := auth.LogoutOrigin(); got != test.want {
				t.Fatalf("LogoutOrigin() = %q, want %q", got, test.want)
			}
		})
	}
}

// Plaintext HTTP models the internal listener behind a TLS-terminating proxy.
//
//goland:noinspection HttpUrlsUsage
func TestOIDCCookiesUseSecureFlagWhenRequestIsHTTPS(t *testing.T) {
	t.Parallel()

	auth := newOIDCSessionTestAuth(t)

	req := httptest.NewRequest(http.MethodGet, "http://internal/manage", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "forge.example.com")
	rec := httptest.NewRecorder()
	if err := auth.setStateCookie(rec, req, "state-value", "/manage"); err != nil {
		t.Fatalf("setStateCookie() error = %v", err)
	}
	setOIDCTestSession(t, auth, rec, req, Session{Sub: "subject", Email: "dev@example.com"})
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

// Plaintext HTTP is the scheme-sensitive behavior under test.
//
//goland:noinspection HttpUrlsUsage
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

// Plaintext HTTP models the internal listener behind a TLS-terminating proxy.
//
//goland:noinspection HttpUrlsUsage
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
		{name: "backslash protocol-relative URL", next: `/\evil.example.com/path`, want: "/"},
		{name: "backslash in local path", next: `/manage\settings`, want: "/"},
		{name: "encoded protocol-relative URL", next: `/%2f%2fevil.example.com/path`, want: "/"},
		{name: "encoded backslash URL", next: `/%255c%255cevil.example.com/path`, want: "/"},
		{name: "encoded control character", next: `/manage%0dlocation`, want: "/"},
		{name: "invalid escape", next: `/manage%zz`, want: "/"},
		{name: "encoded local path", next: `/%6danage`, want: `/manage`},
		{name: "raw local path", next: `/manage`, want: `/manage`},
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

// Plaintext localhost redirects are permitted for this isolated failure-path test.
//
//goland:noinspection HttpUrlsUsage
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
