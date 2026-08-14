package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func (r *Router) audit(req *http.Request, principal auth.Principal, action, result, reason string, attrs ...any) {
	authMethod := "anonymous"
	actor := "anonymous"
	if principal.TokenID != "" {
		authMethod = "token"
		actor = "token:" + principal.TokenID
	} else if r.webAuth != nil {
		if session, ok := r.webAuth.Session(req); ok {
			authMethod = "oidc"
			actor = "subject:" + session.Sub
		}
	}
	if actor == "anonymous" && principal.Team != "" {
		authMethod = "token"
		actor = "team:" + principal.Team
	}
	fields := []any{
		"event", "security_audit",
		"request_id", observability.RequestID(req.Context()),
		"actor", actor,
		"auth_method", authMethod,
		"action", action,
		"source_ip", clientAddress(req, r.trustedProxyCIDRs),
		"result", result,
		"reason", reason,
	}
	fields = append(fields, attrs...)
	slog.Default().Info("security audit", fields...)
}

func auditReason(err error) string {
	if err == nil {
		return "none"
	}
	if strings.Contains(strings.ToLower(err.Error()), "too many") {
		return "rate_limited"
	}
	if code := errorCode(http.StatusInternalServerError, err); code != "internal_error" {
		return code
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"), strings.Contains(message, "parse"):
		return "validation_error"
	case strings.Contains(message, "not allowed"), strings.Contains(message, "admin access"):
		return "forbidden"
	case strings.Contains(message, "not found"):
		return "not_found"
	case strings.Contains(message, "active"), strings.Contains(message, "latest"), strings.Contains(message, "conflict"):
		return "protected_resource"
	default:
		return "internal_error"
	}
}
