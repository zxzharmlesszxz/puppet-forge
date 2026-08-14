package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
)

const requestIDHeader = "X-Request-ID"

func (r *Router) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := ""
		if r.forwardedHeadersTrusted(req) {
			requestID = normalizeRequestID(req.Header.Get(requestIDHeader))
		}
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set(requestIDHeader, requestID)
		req.Header.Set(requestIDHeader, requestID)
		req = req.WithContext(observability.ContextWithRequestID(req.Context(), requestID))
		next.ServeHTTP(w, req)
	})
}

func normalizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) < 8 || len(raw) > 64 {
		return ""
	}
	for _, char := range raw {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return ""
	}
	return raw
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(value[:])
}
