package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/webauth"
)

const (
	manageTokenCookie       = "puppet_forge_manage_token"
	manageCSRFCookie        = "puppet_forge_manage_csrf"
	defaultActiveReleaseTTL = 30 * 24 * time.Hour
	defaultModuleUploadMax  = 128 << 20
	manageSessionTTL        = 8 * time.Hour
	accessConfigRefreshTTL  = 2 * time.Second
)

type RouterOption func(*Router)

type RouterConfig struct {
	Modules             *service.ModuleService
	ForgeProxy          http.Handler
	PublicBaseURL       string
	Authorizer          *auth.Authorizer
	WebAuth             *webauth.OIDCAuth
	AdminToken          string
	ManageSessionSecret string
	RefreshAccessConfig bool
	PublicModuleAccess  bool
	ActiveReleaseTTL    time.Duration
	SecurityHSTSEnabled bool
}

type requestTooLargeError struct {
	limit int64
}

func (e requestTooLargeError) Error() string {
	return fmt.Sprintf("uploaded module exceeds maximum size of %d bytes", e.limit)
}

type protectedDeleteError struct {
	message string
}

func (e protectedDeleteError) Error() string {
	return e.message
}

type Router struct {
	modules             *service.ModuleService
	forgeProxy          http.Handler
	publicBaseURL       string
	authorizerMu        sync.RWMutex
	authorizer          *auth.Authorizer
	authorizerRefreshMu sync.Mutex
	authorizerRefreshed time.Time
	refreshAccessConfig bool
	webAuth             *webauth.OIDCAuth
	adminToken          string
	manageSessions      *manageSessionStore
	rateLimiter         *rateLimiter
	publicModuleAccess  bool
	activeReleaseTTL    time.Duration
	securityHSTSEnabled bool
	moduleUploadMax     int64
}

func WithModuleUploadMaxBytes(maxBytes int64) RouterOption {
	return func(r *Router) {
		if maxBytes > 0 {
			r.moduleUploadMax = maxBytes
		}
	}
}

func NewRouter(config RouterConfig, opts ...RouterOption) http.Handler {
	if config.ActiveReleaseTTL <= 0 {
		config.ActiveReleaseTTL = defaultActiveReleaseTTL
	}
	manageSessionSecret := config.ManageSessionSecret
	if manageSessionSecret == "" {
		manageSessionSecret = config.AdminToken
	}
	if manageSessionSecret == "" {
		manageSessionSecret = randomManageSessionSecret()
	}

	r := &Router{
		modules:             config.Modules,
		forgeProxy:          config.ForgeProxy,
		publicBaseURL:       strings.TrimRight(config.PublicBaseURL, "/"),
		authorizer:          config.Authorizer,
		refreshAccessConfig: config.RefreshAccessConfig,
		webAuth:             config.WebAuth,
		adminToken:          config.AdminToken,
		manageSessions:      newManageSessionStore(manageSessionSecret),
		rateLimiter:         newRateLimiter(time.Now),
		publicModuleAccess:  config.PublicModuleAccess,
		activeReleaseTTL:    config.ActiveReleaseTTL,
		securityHSTSEnabled: config.SecurityHSTSEnabled,
		moduleUploadMax:     defaultModuleUploadMax,
	}
	for _, opt := range opts {
		opt(r)
	}
	obs := observability.NewMiddleware()

	mux := http.NewServeMux()
	mux.Handle("/metrics", obs.MetricsHandler())
	mux.HandleFunc("/auth/login", r.loginPage)
	mux.HandleFunc("/auth/callback", r.callbackPage)
	mux.HandleFunc("/auth/logout", r.logoutPage)
	mux.HandleFunc("/manage", r.managePage)
	mux.HandleFunc("/manage/login", r.manageLoginPage)
	mux.HandleFunc("/manage/logout", r.manageLogoutPage)
	mux.HandleFunc("/manage/access", r.manageAccessPage)
	mux.HandleFunc("/manage/access/add", r.manageAccessAddPage)
	mux.HandleFunc("/manage/upstream", r.manageUpstreamModule)
	mux.HandleFunc("/manage/modules", r.manageModules)
	mux.HandleFunc("/manage/modules/", r.manageModuleAction)
	mux.HandleFunc("/", r.indexPage)
	mux.HandleFunc("/healthz", r.healthz)
	mux.HandleFunc("/readyz", r.readyz)
	mux.HandleFunc("/api/v1/modules", r.modulesCollection)
	mux.HandleFunc("/api/v1/modules/", r.moduleItem)
	mux.HandleFunc("/modules/", r.modulePage)
	if config.ForgeProxy != nil {
		mux.Handle("/v3/", r.requireRead(http.HandlerFunc(r.v3Handler)))
	}

	return obs.Wrap(r.securityHeaders(mux))
}

func (r *Router) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.securityHSTSEnabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, req)
	})
}
