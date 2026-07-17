package httputil

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

func FirstHeaderValue(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	return strings.TrimSpace(parts[0])
}

func ForwardedParam(value, key string) string {
	value = FirstHeaderValue(value)
	for _, part := range strings.Split(value, ";") {
		name, raw, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(name, key) {
			continue
		}
		return strings.Trim(strings.TrimSpace(raw), `"`)
	}
	return ""
}

func ForwardedScheme(req *http.Request) string {
	if req == nil {
		return "http"
	}
	if forwarded := FirstHeaderValue(req.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		return forwarded
	}
	if forwarded := ForwardedParam(req.Header.Get("Forwarded"), "proto"); forwarded != "" {
		return forwarded
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

func ExternalBaseURL(req *http.Request, fallback string) string {
	fallback = strings.TrimRight(strings.TrimSpace(fallback), "/")
	if req == nil {
		return fallback
	}

	scheme := ForwardedScheme(req)
	host := FirstHeaderValue(req.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = ForwardedParam(req.Header.Get("Forwarded"), "host")
	}
	if host == "" {
		host = req.Host
	}
	if host == "" && fallback != "" {
		parsed, err := url.Parse(fallback)
		if err == nil {
			host = parsed.Host
			if parsed.Scheme != "" {
				scheme = parsed.Scheme
			}
		}
	}
	if host == "" {
		return fallback
	}
	return scheme + "://" + host
}

func ReleaseVersionFromSlug(moduleSlug, releaseSlug string) string {
	if moduleSlug == "" || releaseSlug == "" {
		return ""
	}
	releaseSlug = path.Base(strings.TrimRight(releaseSlug, "/"))
	releaseSlug = strings.TrimSuffix(releaseSlug, ".tar.gz")
	prefix := moduleSlug + "-"
	if !strings.HasPrefix(releaseSlug, prefix) {
		return ""
	}
	return strings.TrimPrefix(releaseSlug, prefix)
}

func SingleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	default:
		return a + b
	}
}
