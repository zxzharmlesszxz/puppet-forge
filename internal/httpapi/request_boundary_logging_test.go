package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

func TestExternalRequestBoundaryLogsClientAddress(t *testing.T) {
	for _, tc := range []struct {
		name, remote, forwarded, clientIP string
		proto                             string
		trust, trusted, rejected          bool
	}{
		{name: "direct", remote: "192.0.2.7:1234", clientIP: "192.0.2.7"},
		{name: "untrusted spoof", remote: "198.51.100.7:1234", forwarded: "192.0.2.7", clientIP: "198.51.100.7", trust: true},
		{name: "trust disabled", remote: "10.1.2.3:1234", forwarded: "192.0.2.7", clientIP: "10.1.2.3"},
		{name: "trusted chain", remote: "10.1.2.3:1234", forwarded: "192.0.2.99, 198.51.100.7, 10.2.3.4", clientIP: "198.51.100.7", trust: true, trusted: true},
		{name: "trusted without header", remote: "10.1.2.3:1234", clientIP: "10.1.2.3", trust: true, trusted: true},
		{name: "IPv6", remote: "[2001:db8::7]:1234", clientIP: "2001:db8::7"},
		{name: "forwarded IPv6", remote: "10.1.2.3:1234", forwarded: "2001:db8::7", clientIP: "2001:db8::7", trust: true, trusted: true},
		{name: "invalid header", remote: "10.1.2.3:1234", forwarded: "invalid", clientIP: "10.1.2.3", trust: true, trusted: true, rejected: true},
		{name: "rejected proto", remote: "10.1.2.3:1234", forwarded: "192.0.2.7", proto: "javascript", clientIP: "10.1.2.3", trust: true, trusted: true, rejected: true},
		{name: "invalid peer", remote: "invalid", forwarded: "192.0.2.7", clientIP: "unknown", trust: true},
		{name: "bounded header", remote: "198.51.100.7:1234", forwarded: strings.Repeat("192.0.2.7, ", 100), clientIP: "198.51.100.7", trust: true},
		{name: "trusted long chain", remote: "10.1.2.3:1234", forwarded: strings.Repeat("192.0.2.7, ", 100) + "198.51.100.7", clientIP: "198.51.100.7", trust: true, trusted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureHTTPLogs(t, slog.LevelDebug)
			router := &Router{trustForwardedHeaders: tc.trust, trustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
			called := false
			handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				called = true
				wantHeader := tc.forwarded
				if !tc.trusted {
					wantHeader = ""
				}
				if req.Header.Get("X-Forwarded-For") != wantHeader {
					t.Error("unexpected forwarded header after boundary")
				}
				if got := clientAddress(req, router.trustedProxyCIDRs); got != tc.clientIP {
					t.Errorf("downstream client IP = %q, want %q", got, tc.clientIP)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, "/?secret=private-query", strings.NewReader("private-body"))
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			req.Header.Set("X-Forwarded-Proto", tc.proto)
			req.Header.Set("Authorization", "Bearer private-token")
			req.Header.Set("Cookie", "session=private-cookie")
			req = req.WithContext(observability.ContextWithRequestID(req.Context(), "boundary-request"))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			wantStatus := http.StatusNoContent
			if tc.rejected {
				wantStatus = http.StatusBadRequest
			}
			if rec.Code != wantStatus || called == tc.rejected {
				t.Fatalf("status=%d called=%t, want status=%d", rec.Code, called, wantStatus)
			}
			count := 0
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				var entry struct {
					Message           string `json:"msg"`
					Level             string `json:"level"`
					RequestID         string `json:"request_id"`
					RemoteAddr        string `json:"remote_addr"`
					ForwardedFor      string `json:"forwarded_for"`
					ForwardedTrusted  bool   `json:"forwarded_trusted"`
					ForwardedRejected bool   `json:"forwarded_rejected"`
					ClientIP          string `json:"client_ip"`
				}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if entry.Message != "request client address" {
					continue
				}
				count++
				wantForwarded := tc.forwarded[:min(len(tc.forwarded), forwardedForLogLimit)]
				if entry.Level != "DEBUG" || entry.RequestID != "boundary-request" || entry.RemoteAddr != tc.remote || entry.ForwardedFor != wantForwarded || entry.ForwardedTrusted != tc.trusted || entry.ForwardedRejected != tc.rejected || entry.ClientIP != tc.clientIP {
					t.Errorf("unexpected boundary log: %s", line)
				}
			}
			if count != 1 {
				t.Errorf("boundary log count=%d, want 1", count)
			}
			for _, secret := range []string{"private-query", "private-body", "private-token", "private-cookie"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("log contains %q", secret)
				}
			}
		})
	}
}

func TestExternalRequestBoundaryClientAddressIsDebugOnly(t *testing.T) {
	logs := captureHTTPLogs(t)
	router := &Router{}
	handler := router.externalRequestBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent || logs.Len() != 0 {
		t.Fatalf("status=%d logs=%s", rec.Code, logs.String())
	}
}
