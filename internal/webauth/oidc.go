package webauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"golang.org/x/oauth2"
)

const oidcStateTTL = 5 * time.Minute

// testableRandomString allows tests to inject a mock function.
var testableRandomString = func(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secure random generation failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type Config struct {
	IssuerURL         string
	ClientID          string
	ClientSecret      string
	RedirectURL       string
	LogoutURL         string
	CookieSecret      string
	PublicBaseURL     string
	Scopes            []string
	SigningAlgorithms []string
	StateStore        store.OIDCStateStore
	SessionStore      store.OIDCSessionStore
	HTTPClient        *http.Client
}

type OIDCAuth struct {
	provider      *oidc.Provider
	verifier      *oidc.IDTokenVerifier
	oauth2        oauth2.Config
	cookies       *securecookie.SecureCookie
	cookieName    string
	stateName     string
	sessionName   string
	logoutURL     string
	publicBaseURL string
	stateStore    store.OIDCStateStore
	stateKey      []byte
	sessionStore  store.OIDCSessionStore
	sessionKey    []byte
	httpClient    *http.Client
}

type Session struct {
	Email      string
	Name       string
	Sub        string
	Groups     []string
	CSRFSecret string
}

const oidcSessionTTL = 8 * time.Hour

func New(ctx context.Context, cfg Config) (*OIDCAuth, error) {
	if cfg.StateStore == nil {
		return nil, fmt.Errorf("oidc state store is required")
	}
	if cfg.SessionStore == nil {
		return nil, fmt.Errorf("oidc session store is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = newOIDCHTTPClient()
	}
	oidcContext := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	provider, err := oidc.NewProvider(oidcContext, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider: %w", err)
	}
	logoutURL := strings.TrimSpace(cfg.LogoutURL)
	if logoutURL == "" {
		var discovery struct {
			EndSessionEndpoint string `json:"end_session_endpoint"`
		}
		if err := provider.Claims(&discovery); err == nil {
			logoutURL = strings.TrimSpace(discovery.EndSessionEndpoint)
		}
	}

	stateKey := sha256.Sum256([]byte(cfg.CookieSecret + "|oidc-state-id"))
	sessionKey := sha256.Sum256([]byte(cfg.CookieSecret + "|oidc-session-id"))
	auth := &OIDCAuth{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{
			ClientID:             cfg.ClientID,
			SupportedSigningAlgs: append([]string(nil), cfg.SigningAlgorithms...),
		}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       normalizeScopes(cfg.Scopes),
		},
		cookies:       newSecureCookie(cfg.CookieSecret),
		cookieName:    "puppet_forge_web",
		stateName:     "puppet_forge_state",
		sessionName:   "puppet_forge_web",
		logoutURL:     logoutURL,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		stateStore:    cfg.StateStore,
		stateKey:      stateKey[:],
		sessionStore:  cfg.SessionStore,
		sessionKey:    sessionKey[:],
		httpClient:    httpClient,
	}

	return auth, nil
}

func normalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	normalized := []string{oidc.ScopeOpenID}
	seen := map[string]struct{}{oidc.ScopeOpenID: {}}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		normalized = append(normalized, scope)
	}
	return normalized
}

func newSecureCookie(secret string) *securecookie.SecureCookie {
	hashKey := sha256.Sum256([]byte(secret + "|hash"))
	blockKey := sha256.Sum256([]byte(secret + "|block"))
	return securecookie.New(hashKey[:], blockKey[:])
}

func (a *OIDCAuth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.Session(r); ok {
			next.ServeHTTP(w, r)
			return
		}

		target := r.URL.RequestURI()
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(target), http.StatusFound)
	})
}

func (a *OIDCAuth) Session(r *http.Request) (Session, bool) {
	if a == nil || a.sessionStore == nil {
		return Session{}, false
	}
	cookie, err := r.Cookie(a.sessionName)
	if err != nil {
		return Session{}, false
	}
	persisted, err := a.sessionStore.GetOIDCSession(r.Context(), a.hashSessionID(cookie.Value), time.Now().UTC())
	if err != nil {
		return Session{}, false
	}
	return Session{
		Email: persisted.Email, Name: persisted.Name, Sub: persisted.Subject,
		Groups: append([]string(nil), persisted.Groups...), CSRFSecret: persisted.CSRFSecret,
	}, true
}

func (a *OIDCAuth) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomString(32)
	if err != nil {
		slog.Error("generate OIDC state token", "err", err)
		http.Error(w, "failed to start OIDC login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString(32)
	if err != nil {
		slog.Error("generate OIDC nonce", "err", err)
		http.Error(w, "failed to start OIDC login", http.StatusInternalServerError)
		return
	}
	pkceVerifier, err := randomString(32)
	if err != nil {
		slog.Error("generate OIDC PKCE verifier", "err", err)
		http.Error(w, "failed to start OIDC login", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	if err := a.stateStore.CreateOIDCState(r.Context(), store.OIDCState{
		StateHash:    a.hashState(state),
		Nonce:        nonce,
		PKCEVerifier: pkceVerifier,
		NextPath:     safeRedirectPath(r.URL.Query().Get("next")),
		CreatedAt:    now,
		ExpiresAt:    now.Add(oidcStateTTL),
	}); err != nil {
		http.Error(w, "failed to persist OIDC state", http.StatusInternalServerError)
		return
	}
	if err := a.setStateCookie(w, r, state, r.URL.Query().Get("next")); err != nil {
		slog.Error("encode OIDC state cookie", "err", err)
		http.Error(w, "failed to start OIDC login", http.StatusInternalServerError)
		return
	}

	oauth2Config := a.oauth2Config(r)
	slog.Debug("oidc login started",
		"next", safeRedirectPath(r.URL.Query().Get("next")),
		"redirect_url", oauth2Config.RedirectURL,
		"state_ttl", oidcStateTTL,
	)
	// #nosec G710 -- the provider authorization endpoint comes from validated OIDC discovery over the hardened client.
	http.Redirect(w, r, oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier)), http.StatusFound)
}

func (a *OIDCAuth) Callback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(a.stateName)
	if err != nil {
		a.failState(w, r, "OIDC session expired. Start login again.")
		return
	}

	statePayload := map[string]string{}
	if err := a.cookies.Decode(a.stateName, stateCookie.Value, &statePayload); err != nil {
		slog.Default().Warn("decode oidc state cookie failed",
			"err", err,
			"host", r.Host,
			"path", r.URL.Path,
			"cookie_len", len(stateCookie.Value),
			"has_code", r.URL.Query().Get("code") != "",
			"has_state", r.URL.Query().Get("state") != "",
			"forwarded_proto", r.Header.Get("X-Forwarded-Proto"),
		)
		a.failState(w, r, "OIDC session cookie is invalid. Start login again.")
		return
	}
	callbackState := r.URL.Query().Get("state")
	if !hmac.Equal([]byte(callbackState), []byte(statePayload["state"])) {
		a.failState(w, r, "OIDC state mismatch. Start login again.")
		return
	}
	state, err := a.stateStore.ConsumeOIDCState(r.Context(), a.hashState(callbackState), time.Now().UTC())
	if err != nil {
		a.failState(w, r, "OIDC session expired or was already used. Start login again.")
		return
	}
	slog.Debug("oidc callback state accepted", "next", state.NextPath)
	a.clearCookie(w, a.stateName, requestIsHTTPS(r, a.publicBaseURL))

	oauth2Config := a.oauth2Config(r)
	oidcContext := context.WithValue(r.Context(), oauth2.HTTPClient, a.httpClient)
	token, err := oauth2Config.Exchange(oidcContext, r.URL.Query().Get("code"), oauth2.VerifierOption(state.PKCEVerifier))
	if err != nil {
		http.Error(w, "oidc exchange failed", http.StatusBadGateway)
		return
	}
	slog.Debug("oidc code exchange succeeded")

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusBadGateway)
		return
	}

	idToken, err := a.verifier.Verify(oidcContext, rawIDToken)
	if err != nil {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	if !hmac.Equal([]byte(idToken.Nonce), []byte(state.Nonce)) {
		http.Error(w, "invalid id_token nonce", http.StatusUnauthorized)
		return
	}

	var claims struct {
		Sub    string   `json:"sub"`
		Email  string   `json:"email"`
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "invalid claims", http.StatusUnauthorized)
		return
	}
	slog.Debug("oidc id token verified", "groups", len(claims.Groups), "has_email", claims.Email != "", "has_name", claims.Name != "")

	sessionID, err := a.createSession(r.Context(), Session{
		Sub: claims.Sub, Email: claims.Email, Name: claims.Name, Groups: claims.Groups,
	})
	if err != nil {
		http.Error(w, "failed to persist session", http.StatusInternalServerError)
		return
	}
	a.setSessionCookie(w, r, sessionID)
	slog.Debug("oidc session created", "next", state.NextPath, "groups", len(claims.Groups))

	http.Redirect(w, r, state.NextPath, http.StatusFound)
}

func (a *OIDCAuth) hashState(state string) string {
	mac := hmac.New(sha256.New, a.stateKey)
	_, _ = mac.Write([]byte(state))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *OIDCAuth) createSession(ctx context.Context, session Session) (string, error) {
	if a == nil || a.sessionStore == nil {
		return "", errors.New("oidc session store is not configured")
	}
	sessionID, err := randomString(32)
	if err != nil {
		return "", err
	}
	csrfSecret, err := randomString(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if err := a.sessionStore.CreateOIDCSession(ctx, store.OIDCSession{
		SessionHash: a.hashSessionID(sessionID), Subject: session.Sub, Email: session.Email, Name: session.Name,
		Groups: append([]string(nil), session.Groups...), CSRFSecret: csrfSecret,
		CreatedAt: now, ExpiresAt: now.Add(oidcSessionTTL), LastSeenAt: now,
	}); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (a *OIDCAuth) hashSessionID(sessionID string) string {
	mac := hmac.New(sha256.New, a.sessionKey)
	_, _ = mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func safeRedirectPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	candidate := next
	for {
		decoded, err := url.PathUnescape(candidate)
		if err != nil {
			return "/"
		}
		if decoded == candidate {
			break
		}
		candidate = decoded
	}
	if strings.Contains(candidate, `\`) || strings.HasPrefix(candidate, "//") {
		return "/"
	}
	for _, value := range candidate {
		if value < ' ' || value == '\x7f' {
			return "/"
		}
	}
	return candidate
}

func (a *OIDCAuth) failState(w http.ResponseWriter, r *http.Request, message string) {
	a.clearCookie(w, a.stateName, requestIsHTTPS(r, a.publicBaseURL))
	http.Redirect(w, r, "/manage/login?error="+url.QueryEscape(message), http.StatusFound)
}

func (a *OIDCAuth) ClearSessionForRequest(w http.ResponseWriter, r *http.Request) {
	a.clearSession(w, r)
}

func (a *OIDCAuth) Logout(w http.ResponseWriter, r *http.Request, next string) {
	a.clearSession(w, r)
	if next == "" {
		next = "/"
	}
	redirectTo := next
	if a.logoutURL != "" {
		redirectTo = a.providerLogoutURL(r, next)
	}
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// LogoutOrigin returns the provider origin that browser form redirects must be
// allowed to reach under Content Security Policy.
func (a *OIDCAuth) LogoutOrigin() string {
	if a == nil {
		return ""
	}
	logoutURL, err := url.Parse(a.logoutURL)
	if err != nil || logoutURL.User != nil || logoutURL.Host == "" {
		return ""
	}
	switch logoutURL.Scheme {
	case "http", "https":
		return logoutURL.Scheme + "://" + logoutURL.Host
	default:
		return ""
	}
}

func (a *OIDCAuth) providerLogoutURL(r *http.Request, next string) string {
	logoutURL, err := url.Parse(a.logoutURL)
	if err != nil {
		return next
	}
	values := logoutURL.Query()
	if a.oauth2.ClientID != "" {
		values.Set("client_id", a.oauth2.ClientID)
	}
	if postLogout := a.absoluteURL(r, next); postLogout != "" {
		values.Set("post_logout_redirect_uri", postLogout)
	}
	logoutURL.RawQuery = values.Encode()
	return logoutURL.String()
}

func (a *OIDCAuth) absoluteURL(r *http.Request, path string) string {
	if parsed, err := url.Parse(path); err == nil && parsed.IsAbs() {
		return path
	}
	baseURL := httputil.ExternalBaseURL(r, a.publicBaseURL)
	if baseURL == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
}

func (a *OIDCAuth) setStateCookie(w http.ResponseWriter, r *http.Request, state, next string) error {
	encoded, err := a.cookies.Encode(a.stateName, map[string]string{
		"state": state,
		"next":  next,
	})
	if err != nil {
		return err
	}

	// #nosec G124 -- Secure is derived from the validated external HTTPS scheme so local HTTP development remains usable.
	http.SetCookie(w, &http.Cookie{
		Name:     a.stateName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, a.publicBaseURL),
		MaxAge:   300,
	})

	return nil
}

func (a *OIDCAuth) setSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	// #nosec G124 -- Secure is derived from the validated external HTTPS scheme so local HTTP development remains usable.
	http.SetCookie(w, &http.Cookie{
		Name:     a.sessionName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, a.publicBaseURL),
		MaxAge:   int(oidcSessionTTL.Seconds()),
	})
}

func (a *OIDCAuth) clearSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(a.sessionName); err == nil && a.sessionStore != nil {
		if err := a.sessionStore.RevokeOIDCSession(r.Context(), a.hashSessionID(cookie.Value), time.Now().UTC()); err != nil {
			slog.Default().Warn("revoke oidc session failed", "err", err)
		}
	}
	a.clearCookie(w, a.sessionName, requestIsHTTPS(r, a.publicBaseURL))
	a.clearCookie(w, a.stateName, requestIsHTTPS(r, a.publicBaseURL))
}

func (a *OIDCAuth) clearCookie(w http.ResponseWriter, name string, secure bool) {
	// #nosec G124 -- deletion preserves the original scheme-dependent Secure attribute.
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func (a *OIDCAuth) oauth2Config(r *http.Request) oauth2.Config {
	cfg := a.oauth2
	if cfg.RedirectURL == "" {
		baseURL := httputil.ExternalBaseURL(r, a.publicBaseURL)
		if baseURL != "" {
			cfg.RedirectURL = baseURL + "/auth/callback"
		}
	}
	return cfg
}

func requestIsHTTPS(r *http.Request, fallback string) bool {
	parsed, err := url.Parse(httputil.ExternalBaseURL(r, fallback))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https")
}

func randomString(size int) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("random string size must be non-negative: %d", size)
	}
	return testableRandomString(size)
}
