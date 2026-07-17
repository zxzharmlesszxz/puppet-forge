package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
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
	authorizer := r.currentAuthorizer(req.Context())
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return false
	}
	_, ok := authorizer.RequireRead(w, req)
	return ok
}

func (r *Router) currentAuthorizer(ctx context.Context) *auth.Authorizer {
	if err := r.refreshAuthorizer(ctx, false); err != nil {
		slog.Default().Warn("access config refresh failed", "err", err)
	}
	return r.authorizerSnapshot()
}

func (r *Router) authorizerSnapshot() *auth.Authorizer {
	r.authorizerMu.RLock()
	defer r.authorizerMu.RUnlock()
	return r.authorizer
}

func (r *Router) setAuthorizer(authorizer *auth.Authorizer) {
	r.authorizerMu.Lock()
	defer r.authorizerMu.Unlock()
	r.authorizer = authorizer
}

func (r *Router) refreshManageAuthorizer(ctx context.Context) {
	if err := r.refreshAuthorizer(ctx, true); err != nil {
		slog.Default().Warn("manage access config refresh failed", "err", err)
	}
}

func (r *Router) refreshAuthorizer(ctx context.Context, force bool) error {
	if !r.refreshAccessConfig || r.modules == nil {
		return nil
	}

	now := time.Now()
	r.authorizerRefreshMu.Lock()
	defer r.authorizerRefreshMu.Unlock()
	if !force && !r.authorizerRefreshed.IsZero() && now.Sub(r.authorizerRefreshed) < accessConfigRefreshTTL {
		return nil
	}

	configs, err := r.modules.LoadTeamConfigs(ctx)
	if err != nil {
		return err
	}
	authorizer, err := auth.NewAuthorizer(auth.AccessConfigsWithRuntimeAdmin(configs, r.adminToken))
	if err != nil {
		return err
	}
	r.setAuthorizer(authorizer)
	r.authorizerRefreshed = now
	return nil
}
