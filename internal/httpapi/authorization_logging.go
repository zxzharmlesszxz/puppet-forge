package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func logReadAuthorization(req *http.Request, authorizer *auth.Authorizer, failure string) {
	if !slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		return
	}
	header := req.Header.Get("Authorization")
	scheme, reason := "other", "unsupported_authorization_scheme"
	if header == "" {
		scheme, reason = "none", "authorization_missing"
	} else if strings.EqualFold(header, "Bearer") {
		scheme, reason = "bearer", "bearer_token_missing"
	} else if prefix, token, ok := strings.Cut(header, " "); ok && strings.EqualFold(prefix, "Bearer") {
		scheme = "bearer"
		token = strings.TrimSpace(token)
		switch {
		case token == "":
			reason = "bearer_token_missing"
		case strings.ContainsAny(token, " \t\r\n"):
			reason = "bearer_token_contains_whitespace"
		default:
			reason = "token_unknown_or_inactive_in_snapshot"
			if authorizer != nil && authorizer.Enabled() {
				if principal, matched := authorizer.AuthenticateToken(token); matched && !principal.CanRead {
					reason = "read_permission_missing"
				}
			}
		}
	}
	if failure != "" {
		reason = failure
	} else if !authorizer.Enabled() {
		reason = "access_control_unconfigured"
	}
	slog.DebugContext(req.Context(), "read authorization rejected",
		"request_id", observability.RequestID(req.Context()),
		"authorization_present", header != "",
		"authorization_multiple", len(req.Header.Values("Authorization")) > 1,
		"authorization_scheme", scheme,
		"reason", reason,
	)
}
