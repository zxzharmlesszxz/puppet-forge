package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (r *Router) audit(req *http.Request, principal auth.Principal, action, result, reason string, attrs ...any) {
	authMethod := "anonymous"
	actor := "anonymous"
	identityAttrs := make([]any, 0, 4)
	if principal.TokenID != "" {
		authMethod = "token"
		actor = "token:" + principal.TokenID
		if principal.TokenName != "" {
			actor = "token:" + principal.TokenName
		}
		identityAttrs = append(identityAttrs, "actor_token_id", principal.TokenID)
	} else if r.webAuth != nil {
		if session, ok := r.webAuth.Session(req); ok {
			authMethod = "oidc"
			actor = auditOIDCActor(session.Name, session.Email, session.Sub)
			identityAttrs = append(identityAttrs, "oidc_subject", session.Sub)
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
	fields = append(fields, identityAttrs...)
	fields = append(fields, attrs...)
	slog.Default().Info("security audit", fields...)
	event := store.AuditEvent{
		OccurredAt: time.Now().UTC(),
		RequestID:  observability.RequestID(req.Context()),
		Actor:      actor,
		AuthMethod: authMethod,
		Action:     action,
		Result:     result,
		Reason:     reason,
		SourceIP:   clientAddress(req, r.trustedProxyCIDRs),
		Attributes: auditAttributes(append(identityAttrs, attrs...)),
	}
	if err := r.modules.RecordAuditEvent(req.Context(), event); err != nil {
		slog.Default().Error("record security audit event", "request_id", event.RequestID, "action", action, "err", err)
	}
}

func auditOIDCActor(name, email, subject string) string {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name != "" && email != "" {
		return name + " <" + email + ">"
	}
	if email != "" {
		return email
	}
	if name != "" {
		return name
	}
	return "subject:" + strings.TrimSpace(subject)
}

func auditAttributes(attrs []any) map[string]string {
	result := make(map[string]string, len(attrs)/2)
	for index := 0; index+1 < len(attrs); index += 2 {
		key, ok := attrs[index].(string)
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		result[key] = strings.TrimSpace(fmt.Sprint(attrs[index+1]))
	}
	return result
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
