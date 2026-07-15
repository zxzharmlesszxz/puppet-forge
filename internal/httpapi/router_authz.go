package httpapi

import (
	"errors"
	"net/http"

	"puppet-forge/internal/auth"
)

func (r *Router) requireRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.requireReadAccess(w, req) {
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Router) requireReadAccess(w http.ResponseWriter, req *http.Request) bool {
	if r.publicModuleAccess {
		return true
	}
	authorizer := r.currentAuthorizer()
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return false
	}
	_, ok := authorizer.RequireRead(w, req)
	return ok
}

func (r *Router) currentAuthorizer() *auth.Authorizer {
	r.authorizerMu.RLock()
	defer r.authorizerMu.RUnlock()
	return r.authorizer
}

func (r *Router) setAuthorizer(authorizer *auth.Authorizer) {
	r.authorizerMu.Lock()
	defer r.authorizerMu.Unlock()
	r.authorizer = authorizer
}
