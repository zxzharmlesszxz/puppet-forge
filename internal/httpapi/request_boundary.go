package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
)

var forwardedHeaderNames = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Port",
	"X-Forwarded-Prefix",
	"X-Forwarded-Proto",
	"X-Forwarded-Server",
}

func (r *Router) externalRequestBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.forwardedHeadersTrusted(req) {
			stripForwardedHeaders(req)
		} else if err := validateForwardedHeaders(req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !validAuthority(effectiveRequestHost(req)) {
			writeError(w, http.StatusBadRequest, errors.New("invalid request host"))
			return
		}
		if len(r.allowedPublicHosts) > 0 && !authorityAllowed(effectiveRequestHost(req), r.allowedPublicHosts) {
			writeError(w, http.StatusMisdirectedRequest, errors.New("request host is not allowed"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Router) forwardedHeadersTrusted(req *http.Request) bool {
	if !r.trustForwardedHeaders {
		return false
	}
	remote, ok := parseRequestAddress(req.RemoteAddr)
	return ok && addressInPrefixes(remote, r.trustedProxyCIDRs)
}

func stripForwardedHeaders(req *http.Request) {
	for _, name := range forwardedHeaderNames {
		req.Header.Del(name)
	}
}

func validateForwardedHeaders(req *http.Request) error {
	forwardedHost, forwardedProto, err := validatedStandardForwarded(req.Header.Get("Forwarded"))
	if err != nil {
		return err
	}
	xHost := httputil.FirstHeaderValue(req.Header.Get("X-Forwarded-Host"))
	if xHost != "" && !validAuthority(xHost) {
		return errors.New("invalid X-Forwarded-Host header")
	}
	xProto := strings.ToLower(httputil.FirstHeaderValue(req.Header.Get("X-Forwarded-Proto")))
	if xProto != "" && xProto != "http" && xProto != "https" {
		return errors.New("invalid X-Forwarded-Proto header")
	}
	if xHost != "" && forwardedHost != "" && !strings.EqualFold(xHost, forwardedHost) {
		return errors.New("conflicting forwarded host headers")
	}
	if xProto != "" && forwardedProto != "" && xProto != forwardedProto {
		return errors.New("conflicting forwarded proto headers")
	}
	if raw := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); raw != "" {
		for part := range strings.SplitSeq(raw, ",") {
			if _, ok := parseRequestAddress(strings.TrimSpace(part)); !ok {
				return errors.New("invalid X-Forwarded-For header")
			}
		}
	}
	return nil
}

func validatedStandardForwarded(raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", nil
	}
	first := httputil.FirstHeaderValue(raw)
	if first == "" {
		return "", "", errors.New("invalid Forwarded header")
	}
	values := make(map[string]string)
	for part := range strings.SplitSeq(first, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if !ok || name == "" || value == "" {
			return "", "", errors.New("invalid Forwarded header")
		}
		if _, duplicate := values[name]; duplicate {
			return "", "", errors.New("duplicate Forwarded parameter")
		}
		values[name] = value
	}
	host := values["host"]
	if host != "" && !validAuthority(host) {
		return "", "", errors.New("invalid Forwarded host parameter")
	}
	proto := strings.ToLower(values["proto"])
	if proto != "" && proto != "http" && proto != "https" {
		return "", "", errors.New("invalid Forwarded proto parameter")
	}
	return host, proto, nil
}

func effectiveRequestHost(req *http.Request) string {
	if host := httputil.FirstHeaderValue(req.Header.Get("X-Forwarded-Host")); host != "" {
		return host
	}
	if host := httputil.ForwardedParam(req.Header.Get("Forwarded"), "host"); host != "" {
		return host
	}
	return req.Host
}

func validAuthority(authority string) bool {
	authority = strings.TrimSpace(authority)
	if authority == "" || strings.ContainsAny(authority, " \\/@\r\n\t") {
		return false
	}
	parsed, err := url.Parse("//" + authority)
	return err == nil && parsed.User == nil && parsed.Host == authority && parsed.Hostname() != ""
}

func authorityAllowed(authority string, allowed []string) bool {
	parsed, err := url.Parse("//" + authority)
	if err != nil {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(candidate)
		if strings.EqualFold(authority, candidate) {
			return true
		}
		if !strings.Contains(candidate, ":") && strings.EqualFold(parsed.Hostname(), candidate) {
			return true
		}
	}
	return false
}
