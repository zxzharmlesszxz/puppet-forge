package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func TestRequestIDTrustBoundary(t *testing.T) {
	t.Parallel()

	router := &Router{
		trustForwardedHeaders: true,
		trustedProxyCIDRs:     []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}
	handler := router.requestID(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(observability.RequestID(req.Context())))
	}))

	tests := []struct {
		name       string
		remoteAddr string
		requestID  string
		want       string
	}{
		{name: "trusted normalized id", remoteAddr: "10.1.2.3:1234", requestID: "edge-request_123", want: "edge-request_123"},
		{name: "untrusted supplied id", remoteAddr: "192.0.2.10:1234", requestID: "attacker-request", want: ""},
		{name: "trusted malformed id", remoteAddr: "10.1.2.3:1234", requestID: "bad request id", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "http://forge.example.test/", nil)
			req.RemoteAddr = test.remoteAddr
			req.Header.Set(requestIDHeader, test.requestID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get(requestIDHeader)
			if got == "" || rec.Body.String() != got {
				t.Fatalf("header=%q body=%q", got, rec.Body.String())
			}
			if test.want != "" && got != test.want {
				t.Fatalf("request ID = %q, want %q", got, test.want)
			}
			if test.want == "" && got == test.requestID {
				t.Fatalf("untrusted or malformed request ID was accepted: %q", got)
			}
		})
	}
}
