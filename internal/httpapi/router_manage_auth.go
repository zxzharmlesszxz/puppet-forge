package httpapi

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func (r *Router) loginPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	if !r.requireSharedRateLimit(w, req, "auth-login", 60, time.Minute, "too many login attempts") {
		return
	}
	clearManageTokenCookie(w, req)
	clearManageCSRFToken(w, req)
	r.audit(req, auth.Principal{}, "oidc_login_start", "success", "none")
	r.webAuth.Login(w, req)
}

func (r *Router) callbackPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	if !r.requireSharedRateLimit(w, req, "auth-callback", 60, time.Minute, "too many login callback attempts") {
		return
	}
	r.webAuth.Callback(w, req)
}

func (r *Router) logoutPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	r.audit(req, auth.Principal{}, "oidc_logout", "success", "none")
	r.webAuth.Logout(w, req, "/")
}

func (r *Router) manageLoginPage(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.renderManageLogin(w, req, req.URL.Query().Get("error"))
	case http.MethodPost:
		allowed, err := r.allowSharedRateLimit(req, "manage-login", 20, time.Minute)
		if err != nil {
			r.renderManageLogin(w, req, "login rate limit service is unavailable")
			return
		}
		if !allowed {
			r.renderManageLogin(w, req, "too many login attempts")
			return
		}
		token := strings.TrimSpace(req.FormValue("token"))
		var principal auth.Principal
		var ok bool
		r.refreshManageAuthorizer(req.Context())
		authorizer := r.authorizerSnapshot()
		if authorizer != nil {
			principal, ok = authorizer.AuthenticateToken(token)
		}
		if !ok || (!principal.CanPublish && !principal.CanAdmin) {
			r.audit(req, principal, "token_login", "failure", "unauthorized")
			r.renderManageLogin(w, req, "publish or admin token required")
			return
		}
		r.recordAccessTokenUsed(req.Context(), principal)
		if r.tokenHasher == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("access token hashing is not configured"))
			return
		}
		if r.webAuth != nil {
			r.webAuth.ClearSessionForRequest(w, req)
		}
		sessionID, csrfSecret, err := r.manageSessions.Create(req.Context(), r.tokenHasher.Digest(token), principal.TokenID, manageSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to create manage session: %w", err))
			return
		}
		// #nosec G124 -- Secure is derived from the validated external HTTPS scheme so local HTTP development remains usable.
		http.SetCookie(w, &http.Cookie{
			Name:     manageTokenCookie,
			Value:    sessionID,
			Path:     "/manage",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   httputil.ForwardedScheme(req) == "https",
			MaxAge:   int(manageSessionTTL.Seconds()),
		})
		setManageCSRFTokenCookie(w, req, csrfSecret)
		r.audit(req, principal, "token_login", "success", "none", "team", principal.Team)
		http.Redirect(w, req, "/manage", http.StatusFound)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (r *Router) renderManageLogin(w http.ResponseWriter, req *http.Request, errorMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageLoginTemplate, manageLoginData{CSPNonce: cspNonce(req), Error: errorMessage, HasOIDC: r.webAuth != nil})
}

func (r *Router) manageLogoutPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !r.requireManageCSRF(w, req) {
		return
	}

	principal, _ := r.managePrincipal(req)
	hasOIDCSession := false
	if r.webAuth != nil {
		_, hasOIDCSession = r.webAuth.Session(req)
	}
	if cookie, err := req.Cookie(manageTokenCookie); err == nil {
		if err := r.manageSessions.Delete(req.Context(), cookie.Value, time.Now()); err != nil {
			slog.Default().Warn("revoke manage session failed", "err", err)
		}
	}
	clearManageTokenCookie(w, req)
	clearManageCSRFToken(w, req)
	r.audit(req, principal, "manage_logout", "success", "none", "team", principal.Team)
	if r.webAuth != nil {
		if hasOIDCSession {
			r.webAuth.Logout(w, req, "/manage/login")
			return
		}
		r.webAuth.ClearSessionForRequest(w, req)
	}
	http.Redirect(w, req, "/manage/login", http.StatusFound)
}

func clearManageTokenCookie(w http.ResponseWriter, req *http.Request) {
	// #nosec G124 -- deletion preserves the request scheme-dependent Secure attribute.
	http.SetCookie(w, &http.Cookie{
		Name:     manageTokenCookie,
		Value:    "",
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   httputil.ForwardedScheme(req) == "https",
		MaxAge:   -1,
	})
}

func (r *Router) managePrincipal(req *http.Request) (auth.Principal, bool) {
	r.refreshManageAuthorizer(req.Context())
	authorizer := r.authorizerSnapshot()
	if authorizer == nil || !authorizer.Enabled() {
		return auth.Principal{}, false
	}

	cookie, err := req.Cookie(manageTokenCookie)
	if err == nil {
		if session, sessionOK := r.manageSessions.Session(req.Context(), cookie.Value, time.Now()); sessionOK && session.AuthMethod == "token" {
			principal, ok := authorizer.AuthenticateTokenDigest(session.CredentialHash)
			if ok && (principal.CanPublish || principal.CanManageTeam || principal.CanAdmin) {
				if session.CredentialID != "" && session.CredentialID != principal.TokenID {
					slog.Debug("manage token session rejected",
						"request_id", observability.RequestID(req.Context()),
						"reason", "credential_id_mismatch",
					)
					return auth.Principal{}, false
				}
				slog.Debug("manage principal authenticated",
					"request_id", observability.RequestID(req.Context()),
					"auth_method", "token",
					"team", principal.Team,
					"can_publish", principal.CanPublish,
					"can_manage_team", principal.CanManageTeam,
					"can_admin", principal.CanAdmin,
					"publish_spaces", len(principal.PublishOwners),
					"managed_teams", len(principal.ManagedTeams),
				)
				r.recordAccessTokenUsed(req.Context(), principal)
				return principal, true
			}
			slog.Debug("manage token session rejected",
				"request_id", observability.RequestID(req.Context()),
				"reason", "credential_not_authorized",
			)
		} else {
			slog.Debug("manage token session rejected",
				"request_id", observability.RequestID(req.Context()),
				"reason", "session_missing_expired_or_wrong_method",
			)
		}
	}

	if r.webAuth != nil {
		if session, ok := r.webAuth.Session(req); ok {
			if principal, ok := authorizer.AuthenticateOIDC(session.Email, session.Sub, session.Groups); ok {
				slog.Debug("manage principal authenticated",
					"request_id", observability.RequestID(req.Context()),
					"auth_method", "oidc",
					"team", principal.Team,
					"can_publish", principal.CanPublish,
					"can_manage_team", principal.CanManageTeam,
					"can_admin", principal.CanAdmin,
					"publish_spaces", len(principal.PublishOwners),
					"managed_teams", len(principal.ManagedTeams),
					"oidc_groups", len(session.Groups),
				)
				return principal, true
			}
			slog.Default().Warn("oidc session is not mapped to team",
				"request_id", observability.RequestID(req.Context()),
				"reason", "identity_not_mapped",
				"oidc_groups", len(session.Groups),
			)
		}
	}

	slog.Debug("manage principal unavailable", "request_id", observability.RequestID(req.Context()))
	return auth.Principal{}, false
}

func (r *Router) requireManage(w http.ResponseWriter, req *http.Request) (auth.Principal, bool) {
	principal, ok := r.managePrincipal(req)
	if !ok {
		if r.webAuth != nil {
			if _, hasSession := r.webAuth.Session(req); !hasSession {
				http.Redirect(w, req, "/manage/login", http.StatusFound)
				return auth.Principal{}, false
			}
			http.Redirect(w, req, "/manage/login?error="+url.QueryEscape("OIDC account is not allowed to manage modules"), http.StatusFound)
			return auth.Principal{}, false
		}
		http.Redirect(w, req, "/manage/login", http.StatusFound)
		return auth.Principal{}, false
	}
	return principal, true
}

func (r *Router) ensureManageCSRFToken(w http.ResponseWriter, req *http.Request) (string, error) {
	if cookie, err := req.Cookie(manageTokenCookie); err == nil {
		if session, ok := r.manageSessions.Session(req.Context(), cookie.Value, time.Now()); ok {
			setManageCSRFTokenCookie(w, req, session.CSRFSecret)
			return session.CSRFSecret, nil
		}
	}
	if r.webAuth != nil {
		if session, ok := r.webAuth.Session(req); ok && session.CSRFSecret != "" {
			setManageCSRFTokenCookie(w, req, session.CSRFSecret)
			return session.CSRFSecret, nil
		}
	}
	if cookie, err := req.Cookie(manageCSRFCookie); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}

	token, err := randomBase64URL(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate CSRF token: %w", err)
	}
	setManageCSRFTokenCookie(w, req, token)
	return token, nil
}

func setManageCSRFTokenCookie(w http.ResponseWriter, req *http.Request, token string) {
	// #nosec G124 -- Secure is derived from the validated external HTTPS scheme so local HTTP development remains usable.
	http.SetCookie(w, &http.Cookie{
		Name:     manageCSRFCookie,
		Value:    token,
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   httputil.ForwardedScheme(req) == "https",
		MaxAge:   int((8 * time.Hour).Seconds()),
	})
}

func clearManageCSRFToken(w http.ResponseWriter, req *http.Request) {
	// #nosec G124 -- deletion preserves the request scheme-dependent Secure attribute.
	http.SetCookie(w, &http.Cookie{
		Name:     manageCSRFCookie,
		Value:    "",
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   httputil.ForwardedScheme(req) == "https",
		MaxAge:   -1,
	})
}

func (r *Router) requireManageCSRF(w http.ResponseWriter, req *http.Request) bool {
	if allowed, reason := checkManageRequestSourceForCSRF(req, r.publicBaseURL); !allowed {
		slog.Warn("manage csrf source rejected",
			"request_id", w.Header().Get(requestIDHeader),
			"reason", reason,
			"fetch_site", boundedCSRFLogValue(req.Header.Get("Sec-Fetch-Site")),
			"origin", csrfLogOrigin(req.Header.Get("Origin")),
			"referer_origin", csrfLogOrigin(req.Header.Get("Referer")),
			"request_host", boundedCSRFLogValue(req.Host),
			"request_scheme", httputil.ForwardedScheme(req),
			"expected_origin", csrfLogOrigin(httputil.ExternalBaseURL(req, r.publicBaseURL)),
		)
		writeError(w, http.StatusForbidden, errors.New("cross-origin request rejected"))
		return false
	}
	slog.Debug("manage csrf source accepted",
		"request_id", observability.RequestID(req.Context()),
		"fetch_site", boundedCSRFLogValue(req.Header.Get("Sec-Fetch-Site")),
		"origin", csrfLogOrigin(req.Header.Get("Origin")),
		"referer_origin", csrfLogOrigin(req.Header.Get("Referer")),
		"request_host", boundedCSRFLogValue(req.Host),
		"request_scheme", httputil.ForwardedScheme(req),
	)
	cookie, err := req.Cookie(manageCSRFCookie)
	if err != nil || cookie.Value == "" {
		slog.Debug("manage csrf token rejected", "request_id", observability.RequestID(req.Context()), "reason", "cookie_missing")
		writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
		return false
	}

	token := req.PostFormValue("csrf_token")
	if token == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
		slog.Debug("manage csrf token rejected", "request_id", observability.RequestID(req.Context()), "reason", "form_cookie_mismatch")
		writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
		return false
	}
	if sessionCookie, err := req.Cookie(manageTokenCookie); err == nil {
		session, ok := r.manageSessions.Session(req.Context(), sessionCookie.Value, time.Now())
		if !ok || subtle.ConstantTimeCompare([]byte(session.CSRFSecret), []byte(token)) != 1 {
			slog.Debug("manage csrf token rejected", "request_id", observability.RequestID(req.Context()), "reason", "manage_session_mismatch")
			writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
			return false
		}
		slog.Debug("manage csrf token accepted", "request_id", observability.RequestID(req.Context()), "auth_method", "token")
		return true
	}
	if r.webAuth != nil {
		session, ok := r.webAuth.Session(req)
		if !ok || subtle.ConstantTimeCompare([]byte(session.CSRFSecret), []byte(token)) != 1 {
			slog.Debug("manage csrf token rejected", "request_id", observability.RequestID(req.Context()), "reason", "oidc_session_mismatch")
			writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
			return false
		}
		slog.Debug("manage csrf token accepted", "request_id", observability.RequestID(req.Context()), "auth_method", "oidc")
		return true
	}
	slog.Debug("manage csrf token accepted", "request_id", observability.RequestID(req.Context()), "auth_method", "csrf_cookie_only")
	return true
}

func manageRequestSourceCompatibleWithCSRF(req *http.Request, publicBaseURL string) bool {
	allowed, _ := checkManageRequestSourceForCSRF(req, publicBaseURL)
	return allowed
}

func checkManageRequestSourceForCSRF(req *http.Request, publicBaseURL string) (bool, string) {
	fetchSite := strings.ToLower(strings.TrimSpace(req.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" {
		return false, "fetch_site_cross_site"
	}
	source := strings.TrimSpace(req.Header.Get("Origin"))
	if source == "" {
		source = strings.TrimSpace(req.Header.Get("Referer"))
	}
	if source == "" {
		return true, ""
	}
	// Safari can send the serialized opaque origin "null" for a same-page form
	// submission. The session-bound cookie/form secret remains mandatory, while
	// Fetch Metadata still rejects requests explicitly identified as cross-site.
	if strings.EqualFold(source, "null") {
		return true, ""
	}
	sourceURL, err := url.Parse(source)
	if err != nil || sourceURL.Scheme == "" || sourceURL.Host == "" {
		return false, "source_origin_invalid"
	}
	sourceScheme := strings.ToLower(sourceURL.Scheme)
	if sourceScheme != "http" && sourceScheme != "https" {
		return false, "source_scheme_invalid"
	}
	// Fetch Metadata reflects the origin comparison performed by the browser
	// before ingress host or scheme rewriting. The session-bound CSRF secret is
	// still required below, so accepting this signal does not bypass CSRF.
	if fetchSite == "same-origin" {
		return true, ""
	}
	expectedURL, err := url.Parse(httputil.ExternalBaseURL(req, publicBaseURL))
	if err != nil || expectedURL.Scheme == "" || expectedURL.Host == "" {
		return false, "expected_origin_invalid"
	}
	expectedScheme := strings.ToLower(expectedURL.Scheme)
	if sourceScheme == "http" && expectedScheme == "https" {
		return false, "source_scheme_downgrade"
	}
	if !strings.EqualFold(sourceURL.Host, expectedURL.Host) {
		// Ingresses may rewrite Host or expose the same service through several
		// public aliases. The session-bound token checked by requireManageCSRF is
		// the authorization boundary for that ambiguous case.
		return true, ""
	}
	if sourceScheme == expectedScheme {
		return true, ""
	}
	// TLS-terminating proxies commonly preserve Host while forwarding the
	// request over HTTP. Accept only that secure-to-internal downgrade; an
	// HTTP browser origin must never be accepted for an HTTPS request.
	if sourceScheme == "https" && expectedScheme == "http" {
		return true, ""
	}
	return false, "source_origin_mismatch"
}

func csrfLogOrigin(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "null") {
		return "opaque"
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if strings.TrimSpace(raw) == "" {
			return ""
		}
		return "invalid"
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

func boundedCSRFLogValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 256 {
		return raw[:256]
	}
	return raw
}
