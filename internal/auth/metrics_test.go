package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestAuthorizationGateMetrics(t *testing.T) {
	authorizer, err := NewAuthorizer([]TeamConfig{{Team: "metricteam", ReadTokens: []string{"read-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		authorizer *Authorizer
		token      string
		result     string
		status     int
	}{
		{"accepted", authorizer, "read-token", "success", http.StatusOK},
		{"denied", authorizer, "wrong-token", "rejected", http.StatusUnauthorized},
		{"unconfigured", nil, "", "error", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{"scope": "read", "result": tc.result}
			before := testutil.MetricCounterValue(t, "puppet_forge_authorization_checks_total", labels)
			req := httptest.NewRequest(http.MethodGet, "/v3/modules/demo", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response := httptest.NewRecorder()
			_, allowed := tc.authorizer.RequireRead(response, req)
			if allowed != (tc.result == "success") || response.Code != tc.status {
				t.Fatalf("decision=%v status=%d", allowed, response.Code)
			}
			if got := testutil.MetricCounterValue(t, "puppet_forge_authorization_checks_total", labels); got < before+1 {
				t.Fatal("authorization gate outcome was not counted")
			}
		})
	}
}
