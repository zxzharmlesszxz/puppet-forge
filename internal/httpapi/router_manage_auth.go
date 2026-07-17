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
)

func (r *Router) loginPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	if !r.rateLimiter.Allow(rateLimitKey(req, "auth-login"), 60, time.Minute) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many login attempts"))
		return
	}
	r.webAuth.Login(w, req)
}

func (r *Router) callbackPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	r.webAuth.Callback(w, req)
}

func (r *Router) logoutPage(w http.ResponseWriter, req *http.Request) {
	if r.webAuth == nil {
		http.Redirect(w, req, "/", http.StatusFound)
		return
	}
	r.webAuth.Logout(w, req, "/")
}

func (r *Router) manageLoginPage(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.renderManageLogin(w, req.URL.Query().Get("error"))
	case http.MethodPost:
		if !r.rateLimiter.Allow(rateLimitKey(req, "manage-login"), 20, time.Minute) {
			r.renderManageLogin(w, "too many login attempts")
			return
		}
		token := strings.TrimSpace(req.FormValue("token"))
		var principal auth.Principal
		var ok bool
		authorizer := r.currentAuthorizer()
		if authorizer != nil {
			principal, ok = authorizer.AuthenticateToken(token)
		}
		if authorizer == nil || !authorizer.Enabled() {
			principal = auth.Principal{Team: "local", CanAdmin: true, CanRead: true, CanPublish: true}
			ok = true
		}
		if !ok || (!principal.CanPublish && !principal.CanAdmin) {
			r.renderManageLogin(w, "publish or admin token required")
			return
		}
		sessionID, err := r.manageSessions.Create(token, manageSessionTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to create manage session: %w", err))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     manageTokenCookie,
			Value:    sessionID,
			Path:     "/manage",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   httputil.ForwardedScheme(req) == "https",
			MaxAge:   int(manageSessionTTL.Seconds()),
		})
		http.Redirect(w, req, "/manage", http.StatusFound)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (r *Router) renderManageLogin(w http.ResponseWriter, errorMessage string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := manageLoginTemplate.Execute(w, manageLoginData{Error: errorMessage, HasOIDC: r.webAuth != nil}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Router) manageLogoutPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !requireManageCSRF(w, req) {
		return
	}

	hasOIDCSession := false
	if r.webAuth != nil {
		_, hasOIDCSession = r.webAuth.Session(req)
	}
	if cookie, err := req.Cookie(manageTokenCookie); err == nil {
		r.manageSessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     manageTokenCookie,
		Value:    "",
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	clearManageCSRFToken(w)
	if r.webAuth != nil {
		if hasOIDCSession {
			r.webAuth.Logout(w, req, "/manage/login")
			return
		}
		r.webAuth.ClearSessionForRequest(w, req)
	}
	http.Redirect(w, req, "/manage/login", http.StatusFound)
}

func (r *Router) managePrincipal(req *http.Request) (auth.Principal, bool) {
	authorizer := r.currentAuthorizer()
	if authorizer == nil || !authorizer.Enabled() {
		return auth.Principal{Team: "local", CanAdmin: true, CanRead: true, CanPublish: true}, true
	}

	cookie, err := req.Cookie(manageTokenCookie)
	if err == nil {
		if token, sessionOK := r.manageSessions.Token(cookie.Value, time.Now()); sessionOK {
			principal, ok := authorizer.AuthenticateToken(token)
			if ok && (principal.CanPublish || principal.CanManageTeam || principal.CanAdmin) {
				return principal, true
			}
		}
	}

	if r.webAuth != nil {
		if session, ok := r.webAuth.Session(req); ok {
			if principal, ok := authorizer.AuthenticateOIDC(session.Email, session.Sub, session.Groups); ok {
				return principal, true
			}
			slog.Default().Warn("oidc session is not mapped to team",
				"email", session.Email,
				"subject", session.Sub,
				"domain", auth.EmailDomain(session.Email),
				"groups", session.Groups,
			)
		}
	}

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
	if cookie, err := req.Cookie(manageCSRFCookie); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}

	token, err := randomBase64URL(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate CSRF token: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     manageCSRFCookie,
		Value:    token,
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   httputil.ForwardedScheme(req) == "https",
		MaxAge:   int((8 * time.Hour).Seconds()),
	})
	return token, nil
}

func clearManageCSRFToken(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     manageCSRFCookie,
		Value:    "",
		Path:     "/manage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func requireManageCSRF(w http.ResponseWriter, req *http.Request) bool {
	cookie, err := req.Cookie(manageCSRFCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
		return false
	}

	token := req.PostFormValue("csrf_token")
	if token == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
		writeError(w, http.StatusForbidden, errors.New("invalid csrf token"))
		return false
	}
	return true
}
