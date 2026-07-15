package webauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"puppet-forge/internal/httputil"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"golang.org/x/oauth2"
)

// testableRandomString allows tests to inject a mock function.
var testableRandomString = func(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secure random generation failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	LogoutURL     string
	CookieSecret  string
	PublicBaseURL string
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
}

type Session struct {
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Sub    string   `json:"sub"`
	Groups []string `json:"groups"`
}

func New(ctx context.Context, cfg Config) (*OIDCAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
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

	auth := &OIDCAuth{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		cookies:       newSecureCookie(cfg.CookieSecret),
		cookieName:    "puppet_forge_web",
		stateName:     "puppet_forge_state",
		sessionName:   "puppet_forge_web",
		logoutURL:     logoutURL,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}

	return auth, nil
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
	cookie, err := r.Cookie(a.sessionName)
	if err != nil {
		return Session{}, false
	}

	var session Session
	if err := a.cookies.Decode(a.cookieName, cookie.Value, &session); err != nil {
		return Session{}, false
	}

	return session, true
}

func (a *OIDCAuth) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomString(32)
	if err != nil {
		http.Error(w, "failed to generate OIDC state token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.setStateCookie(w, r, state, r.URL.Query().Get("next")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	oauth2Config := a.oauth2Config(r)
	http.Redirect(w, r, oauth2Config.AuthCodeURL(state), http.StatusFound)
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
	if r.URL.Query().Get("state") != statePayload["state"] {
		a.failState(w, r, "OIDC state mismatch. Start login again.")
		return
	}

	oauth2Config := a.oauth2Config(r)
	token, err := oauth2Config.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "oidc exchange failed", http.StatusBadGateway)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusBadGateway)
		return
	}

	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
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

	if err := a.setSessionCookie(w, r, Session{
		Sub:    claims.Sub,
		Email:  claims.Email,
		Name:   claims.Name,
		Groups: claims.Groups,
	}); err != nil {
		http.Error(w, "failed to persist session", http.StatusInternalServerError)
		return
	}

	a.clearCookie(w, a.stateName, requestIsHTTPS(r, a.publicBaseURL))
	http.Redirect(w, r, safeRedirectPath(statePayload["next"]), http.StatusFound)
}

func safeRedirectPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
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

func (a *OIDCAuth) setSessionCookie(w http.ResponseWriter, r *http.Request, session Session) error {
	encoded, err := a.cookies.Encode(a.cookieName, session)
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     a.sessionName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r, a.publicBaseURL),
		MaxAge:   int((8 * time.Hour).Seconds()),
	})

	return nil
}

func (a *OIDCAuth) clearSession(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, a.sessionName, requestIsHTTPS(r, a.publicBaseURL))
	a.clearCookie(w, a.stateName, requestIsHTTPS(r, a.publicBaseURL))
}

func (a *OIDCAuth) clearCookie(w http.ResponseWriter, name string, secure bool) {
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
