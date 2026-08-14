package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/throttle"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

const (
	manageTokenCookie           = "puppet_forge_manage_token"
	manageCSRFCookie            = "puppet_forge_manage_csrf"
	defaultActiveReleaseTTL     = 30 * 24 * time.Hour
	defaultModuleUploadMax      = 128 << 20
	manageSessionTTL            = 8 * time.Hour
	accessConfigRefreshTTL      = 2 * time.Second
	defaultTokenUsageMaxEntries = 10_000
	tokenUsageRecordInterval    = time.Minute
)

type cspNonceContextKey struct{}

type RouterOption func(*Router)

type RouterConfig struct {
	Modules               *service.ModuleService
	ForgeProxy            http.Handler
	PublicBaseURL         string
	AllowedPublicHosts    []string
	TrustedProxyCIDRs     []netip.Prefix
	TrustForwardedHeaders bool
	Authorizer            *auth.Authorizer
	TokenHasher           *auth.TokenHasher
	WebAuth               *webauth.OIDCAuth
	AdminToken            string
	ManageSessionSecret   string
	RefreshAccessConfig   bool
	PublicModuleAccess    bool
	ActiveReleaseTTL      time.Duration
	SecurityHSTSEnabled   bool
}

type requestTooLargeError struct {
	limit int64
}

func (e requestTooLargeError) Error() string {
	return fmt.Sprintf("uploaded module exceeds maximum size of %d bytes", e.limit)
}

type Router struct {
	modules               *service.ModuleService
	forgeProxy            http.Handler
	publicBaseURL         string
	authorizerMu          sync.RWMutex
	authorizer            *auth.Authorizer
	tokenHasher           *auth.TokenHasher
	authorizerRefreshMu   sync.Mutex
	authorizerRefreshed   time.Time // guarded by authorizerMu
	tokenUsage            *throttle.ExpirySet
	refreshAccessConfig   bool
	webAuth               *webauth.OIDCAuth
	adminToken            string
	manageSessions        *manageSessionStore
	rateLimiter           *rateLimiter
	trustedProxyCIDRs     []netip.Prefix
	allowedPublicHosts    []string
	trustForwardedHeaders bool
	publicModuleAccess    bool
	activeReleaseTTL      time.Duration
	securityHSTSEnabled   bool
	moduleUploadMax       int64
	formActionSources     string
}

func WithModuleUploadMaxBytes(maxBytes int64) RouterOption {
	return func(r *Router) {
		if maxBytes > 0 {
			r.moduleUploadMax = maxBytes
		}
	}
}

func NewRouter(config RouterConfig, opts ...RouterOption) (http.Handler, error) {
	if config.ActiveReleaseTTL <= 0 {
		config.ActiveReleaseTTL = defaultActiveReleaseTTL
	}
	if len(strings.TrimSpace(config.ManageSessionSecret)) < 32 {
		return nil, errors.New("manage session secret must contain at least 32 bytes")
	}
	if config.Modules == nil {
		return nil, errors.New("module service is required")
	}

	r := &Router{
		modules:               config.Modules,
		forgeProxy:            config.ForgeProxy,
		publicBaseURL:         strings.TrimRight(config.PublicBaseURL, "/"),
		authorizer:            config.Authorizer,
		tokenHasher:           config.TokenHasher,
		tokenUsage:            throttle.NewExpirySet(defaultTokenUsageMaxEntries, tokenUsageRecordInterval),
		refreshAccessConfig:   config.RefreshAccessConfig,
		webAuth:               config.WebAuth,
		adminToken:            config.AdminToken,
		manageSessions:        newManageSessionStore(config.Modules, config.ManageSessionSecret),
		rateLimiter:           newRateLimiter(time.Now),
		trustedProxyCIDRs:     append([]netip.Prefix(nil), config.TrustedProxyCIDRs...),
		allowedPublicHosts:    append([]string(nil), config.AllowedPublicHosts...),
		trustForwardedHeaders: config.TrustForwardedHeaders,
		publicModuleAccess:    config.PublicModuleAccess,
		activeReleaseTTL:      config.ActiveReleaseTTL,
		securityHSTSEnabled:   config.SecurityHSTSEnabled,
		moduleUploadMax:       defaultModuleUploadMax,
		formActionSources:     formActionSources(oidcLogoutOrigin(config.WebAuth)),
	}
	for _, opt := range opts {
		opt(r)
	}
	obs := observability.NewMiddleware()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", r.loginPage)
	mux.HandleFunc("/auth/callback", r.callbackPage)
	mux.HandleFunc("/auth/logout", r.logoutPage)
	mux.HandleFunc("/manage", r.managePage)
	mux.HandleFunc("/manage/login", r.manageLoginPage)
	mux.HandleFunc("/manage/logout", r.manageLogoutPage)
	mux.HandleFunc("/manage/access", r.manageAccessPage)
	mux.HandleFunc("/manage/access/add", r.manageAccessAddPage)
	mux.HandleFunc("/manage/access/token", r.manageAccessTokenPage)
	mux.HandleFunc("/manage/admin/access", r.manageAccessPage)
	mux.HandleFunc("/manage/admin/spaces", r.managePublishSpacesPage)
	mux.HandleFunc("/manage/teams", r.manageTeamsPage)
	mux.HandleFunc("/manage/teams/new", r.manageAccessAddPage)
	mux.HandleFunc("/manage/teams/", r.manageTeamPage)
	mux.HandleFunc("/manage/upstream", r.manageUpstreamModule)
	mux.HandleFunc("/manage/modules", r.manageModulesRoot)
	mux.HandleFunc("/manage/modules/", r.manageModuleAction)
	mux.HandleFunc("/", r.indexPage)
	mux.HandleFunc("/healthz", r.healthz)
	mux.HandleFunc("/readyz", r.readyz)
	mux.HandleFunc("/api/v1/modules", r.modulesCollection)
	mux.HandleFunc("/api/v1/modules/", r.moduleItem)
	mux.HandleFunc("/api/v1/manage/publish-spaces", r.publishSpaces)
	mux.HandleFunc("/modules/", r.modulePage)
	if config.ForgeProxy != nil {
		mux.Handle("/v3/", r.requireRead(r.rateLimited("v3-read", 1200, time.Minute, http.HandlerFunc(r.v3Handler))))
	}

	return r.requestID(obs.Wrap(r.externalRequestBoundary(r.securityHeaders(mux)))), nil
}

func (r *Router) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		nonce := ""
		if browserRoute(req.URL.Path) {
			var err error
			nonce, err = randomCSPNonce()
			if err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("generate content security policy nonce: %w", err))
				return
			}
			req = req.WithContext(context.WithValue(req.Context(), cspNonceContextKey{}, nonce))
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		policy := "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'"
		if nonce != "" {
			policy = fmt.Sprintf("default-src 'self'; base-uri 'self'; connect-src 'self'; form-action %s; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self' 'nonce-%s'; style-src 'self' 'nonce-%s'", r.formActionSources, nonce, nonce)
		}
		w.Header().Set("Content-Security-Policy", policy)
		if strings.HasPrefix(req.URL.Path, "/manage") || strings.HasPrefix(req.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.securityHSTSEnabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, req)
	})
}

func browserRoute(requestPath string) bool {
	return requestPath == "/" || strings.HasPrefix(requestPath, "/manage") || strings.HasPrefix(requestPath, "/auth/") || strings.HasPrefix(requestPath, "/modules/")
}

func oidcLogoutOrigin(webAuth *webauth.OIDCAuth) string {
	if webAuth == nil {
		return ""
	}
	return webAuth.LogoutOrigin()
}

func formActionSources(logoutOrigin string) string {
	sources := "'self'"
	if logoutOrigin != "" {
		sources += " " + logoutOrigin
	}
	return sources
}

func randomCSPNonce() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func cspNonce(req *http.Request) string {
	if req == nil {
		return ""
	}
	nonce, _ := req.Context().Value(cspNonceContextKey{}).(string)
	return nonce
}
