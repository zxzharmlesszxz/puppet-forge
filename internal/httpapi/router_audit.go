package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

func (r *Router) auditOIDCLogin(req *http.Request, session webauth.Session) {
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	var teams []string
	if err == nil {
		teams, err = oidcAuditTeams(configs, session)
	}
	if err != nil {
		slog.Error("resolve OIDC login audit teams", "request_id", observability.RequestID(req.Context()), "err", err)
	}
	r.auditWithOIDCSession(req, auth.Principal{}, &session, "oidc_login", "success", "none", "actor_teams", strings.Join(teams, ","))
}

func oidcAuditTeams(configs []auth.TeamConfig, session webauth.Session) ([]string, error) {
	teams := make([]string, 0, len(configs))
	for _, cfg := range configs {
		if auth.IsGlobalAdminConfig(cfg) {
			continue
		}
		authorizer, err := auth.NewAuthorizer([]auth.TeamConfig{cfg})
		if err != nil {
			return nil, err
		}
		principal, ok := authorizer.AuthenticateOIDC(session.AuthorizationEmail(), session.Sub, session.Groups)
		if ok && (principal.CanPublish || principal.CanManageTeam) {
			teams = append(teams, cfg.Team)
		}
	}
	sort.Strings(teams)
	return teams, nil
}

func (r *Router) audit(req *http.Request, principal auth.Principal, action, result, reason string, attrs ...any) {
	r.auditWithOIDCSession(req, principal, nil, action, result, reason, attrs...)
}

func (r *Router) auditWithOIDCSession(req *http.Request, principal auth.Principal, session *webauth.Session, action, result, reason string, attrs ...any) {
	recorded := result == "success" && store.MutationAuditRecorded(req.Context())
	var event store.AuditEvent
	if recorded {
		event, _ = store.PendingMutationAudit(req.Context())
	} else {
		event = r.newAuditEvent(req, principal, session, action, result, reason, attrs...)
	}
	fields := []any{
		"event", "security_audit", "request_id", event.RequestID, "actor", event.Actor,
		"auth_method", event.AuthMethod, "action", action, "source_ip", event.SourceIP,
		"result", result, "reason", reason,
	}
	for key, value := range event.Attributes {
		fields = append(fields, key, value)
	}
	slog.Default().Info("security audit", fields...)
	if recorded {
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 5*time.Second)
	defer cancel()
	if err := r.modules.RecordAuditEvent(auditCtx, event); err != nil {
		slog.Default().Error("record security audit event", "request_id", event.RequestID, "action", action, "err", err)
	}
}

func (r *Router) prepareMutationAudit(req *http.Request, principal auth.Principal, action string, attrs ...any) {
	event := r.newAuditEvent(req, principal, nil, action, "success", "none", attrs...)
	*req = *req.WithContext(store.ContextWithMutationAudit(req.Context(), event))
}

func (r *Router) prepareDeleteMutationAudit(req *http.Request, principal auth.Principal, owner, name, version, justification string, force bool) string {
	action := "delete_module"
	attrs := []any{"space", owner, "module", owner + "/" + name, "justification", justification}
	if version != "" {
		action = "delete_release"
		attrs = append(attrs, "release", version)
	}
	if force {
		action = "force_" + action
	}
	r.prepareMutationAudit(req, principal, action, attrs...)
	return action
}

func (r *Router) newAuditEvent(req *http.Request, principal auth.Principal, session *webauth.Session, action, result, reason string, attrs ...any) store.AuditEvent {
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
	} else {
		if session == nil && r.webAuth != nil {
			if current, ok := r.webAuth.Session(req); ok {
				session = &current
			}
		}
		if session != nil {
			authMethod = "oidc"
			actor = auditOIDCActor(session.Name, session.Email, session.Sub)
			identityAttrs = append(identityAttrs, "oidc_subject", session.Sub)
		}
	}
	if actor == "anonymous" && principal.Team != "" {
		authMethod = "token"
		actor = "team:" + principal.Team
	}
	return store.AuditEvent{
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
