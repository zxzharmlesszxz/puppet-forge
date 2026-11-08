package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func TestReadAuthorizationDiagnosticsDoNotExposeCredentials(t *testing.T) {
	authorizer := mustNewAuthorizer(t, "consumer", "read", "valid-private-token")
	for _, tc := range []struct {
		name, header, scheme, failure, reason string
	}{
		{name: "missing", scheme: "none", reason: "authorization_missing"},
		{name: "empty bearer", header: "Bearer", scheme: "bearer", reason: "bearer_token_missing"},
		{name: "blank bearer", header: "Bearer   ", scheme: "bearer", reason: "bearer_token_missing"},
		{name: "raw token", header: "unknown-private-token", scheme: "other", reason: "unsupported_authorization_scheme"},
		{name: "basic", header: "Basic private-credentials", scheme: "other", reason: "unsupported_authorization_scheme"},
		{name: "unknown", header: "bEaReR unknown-private-token", scheme: "bearer", reason: "token_unknown_or_inactive_in_snapshot"},
		{name: "double bearer", header: "Bearer Bearer unknown-private-token", scheme: "bearer", reason: "bearer_token_contains_whitespace"},
		{name: "inactive SQL", header: "Bearer valid-private-token", scheme: "bearer", failure: "token_inactive_in_store", reason: "token_inactive_in_store"},
		{name: "unavailable", header: "Bearer unknown-private-token", scheme: "bearer", failure: "access_config_unavailable", reason: "access_config_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureHTTPLogs(t, slog.LevelDebug)
			req := httptest.NewRequest(http.MethodGet, "/v3/modules/puppetlabs-stdlib?secret=private-query", nil)
			req.Header.Set("Authorization", tc.header)
			req.Header.Set("Cookie", "private-cookie")
			req = req.WithContext(observability.ContextWithRequestID(req.Context(), "auth-request"))
			logReadAuthorization(req, authorizer, tc.failure)
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["reason"] != tc.reason || event["authorization_scheme"] != tc.scheme || event["authorization_present"] != (tc.header != "") || event["request_id"] != "auth-request" {
				t.Fatalf("unexpected safe authorization diagnostic: %v", event)
			}
			for _, secret := range []string{"valid-private-token", "unknown-private-token", "private-credentials", "private-cookie", "private-query"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("authorization diagnostic exposed sensitive data")
				}
			}
		})
	}
}

func TestReadAuthorizationDiagnosticIsDebugOnly(t *testing.T) {
	logs := captureHTTPLogs(t)
	logReadAuthorization(httptest.NewRequest(http.MethodGet, "/", nil), nil, "authorizer_unconfigured")
	if logs.Len() != 0 {
		t.Fatal("authorization diagnostic was emitted at info level")
	}
}

func TestReadAccessDenialEmitsSafeDiagnostic(t *testing.T) {
	logs := captureHTTPLogs(t, slog.LevelDebug)
	router := &Router{authorizer: mustNewAuthorizer(t, "consumer", "read", "valid-private-token")}
	req := httptest.NewRequest(http.MethodGet, "/v3/releases", nil)
	req.Header.Set("Authorization", "Bearer unknown-private-token")
	rec := httptest.NewRecorder()
	if router.requireReadAccess(rec, req) || rec.Code != http.StatusUnauthorized {
		t.Fatalf("read access status = %d, want 401", rec.Code)
	}
	if !strings.Contains(logs.String(), "token_unknown_or_inactive_in_snapshot") || strings.Contains(logs.String(), "unknown-private-token") {
		t.Fatal("missing or unsafe read authorization diagnostic")
	}
}
