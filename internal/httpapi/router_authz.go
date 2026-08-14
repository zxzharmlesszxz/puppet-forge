package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
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
		slog.Debug("read access allowed", "request_id", observability.RequestID(req.Context()), "reason", "public_module_access")
		return true
	}
	authorizer := r.currentAuthorizer(req.Context())
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return false
	}
	principal, ok := authorizer.RequireRead(w, req)
	if !ok {
		slog.Debug("read access denied", "request_id", observability.RequestID(req.Context()), "reason", "authorization_failed")
		return false
	}
	if !r.requireActiveAccessToken(w, req, principal) {
		slog.Debug("read access denied", "request_id", observability.RequestID(req.Context()), "reason", "token_inactive")
		return false
	}
	slog.Debug("read access allowed",
		"request_id", observability.RequestID(req.Context()),
		"auth_method", "token",
		"team", principal.Team,
		"token_id", principal.TokenID,
	)
	r.recordAccessTokenUsed(req.Context(), principal)
	return true
}

func (r *Router) requireActiveAccessToken(w http.ResponseWriter, req *http.Request, principal auth.Principal) bool {
	if principal.TokenID == "" {
		return true
	}
	if r.modules == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("access token store is not available"))
		return false
	}
	active, err := r.modules.IsAccessTokenActive(req.Context(), principal.TokenID, time.Now())
	if err != nil {
		slog.Default().Warn("validate access token failed", "err", err, "token_id", principal.TokenID)
		writeError(w, http.StatusServiceUnavailable, errors.New("access token validation failed"))
		return false
	}
	if !active {
		writeError(w, http.StatusUnauthorized, errors.New("access token is inactive"))
		return false
	}
	return true
}

func (r *Router) recordAccessTokenUsed(ctx context.Context, principal auth.Principal) {
	if principal.TokenID == "" || r.modules == nil {
		return
	}
	now := time.Now().UTC()
	if !r.tokenUsage.Record(principal.TokenID, now) {
		return
	}

	if err := r.modules.MarkAccessTokenUsed(ctx, principal.TokenID, now); err != nil {
		r.tokenUsage.Forget(principal.TokenID)
		slog.Default().Warn("mark access token used failed", "err", err, "token_id", principal.TokenID)
	}
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

func (r *Router) authorizerSnapshotState() (*auth.Authorizer, time.Time) {
	r.authorizerMu.RLock()
	defer r.authorizerMu.RUnlock()
	return r.authorizer, r.authorizerRefreshed
}

func (r *Router) setAuthorizer(authorizer *auth.Authorizer, refreshedAt time.Time) {
	r.authorizerMu.Lock()
	defer r.authorizerMu.Unlock()
	r.authorizer = authorizer
	r.authorizerRefreshed = refreshedAt
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
	if force {
		r.authorizerRefreshMu.Lock()
	} else if !r.authorizerRefreshMu.TryLock() {
		slog.Debug("access config refresh skipped", "reason", "refresh_in_progress")
		return nil
	}
	defer r.authorizerRefreshMu.Unlock()
	_, refreshedAt := r.authorizerSnapshotState()
	if !force && !refreshedAt.IsZero() && now.Sub(refreshedAt) < accessConfigRefreshTTL {
		slog.Debug("access config refresh skipped", "reason", "refresh_ttl", "age", now.Sub(refreshedAt).Round(time.Millisecond))
		return nil
	}

	configs, err := r.modules.LoadTeamConfigs(ctx)
	if err != nil {
		return err
	}
	var authorizer *auth.Authorizer
	if r.tokenHasher != nil {
		authorizer, err = auth.NewAuthorizerWithTokenHasher(auth.AccessConfigsWithRuntimeAdmin(configs, r.adminToken), r.tokenHasher)
	} else {
		authorizer, err = auth.NewAuthorizer(auth.AccessConfigsWithRuntimeAdmin(configs, r.adminToken))
	}
	if err != nil {
		return err
	}
	r.setAuthorizer(authorizer, now)
	slog.Debug("access config refreshed", "teams", len(configs), "forced", force)
	return nil
}
