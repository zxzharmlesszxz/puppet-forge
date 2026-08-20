package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Version                   bool
	AppEnv                    string
	LogLevel                  string
	HTTPAddr                  string
	MetricsAddr               string
	ReadHeaderTimeout         time.Duration
	ReadTimeout               time.Duration
	WriteTimeout              time.Duration
	IdleTimeout               time.Duration
	HTTPMaxHeaderBytes        int
	ShutdownTimeout           time.Duration
	DatabaseBackend           string
	DatabaseDSN               string
	DatabaseMaxConns          int
	DatabaseMinConns          int
	DatabaseMaxConnLifetime   time.Duration
	DatabaseMaxConnIdleTime   time.Duration
	AdminToken                string
	AccessTokenPepper         string
	AccessTokenHistoryTTL     time.Duration
	DeletedReleaseTTL         time.Duration
	ManageSessionSecret       string
	PublicModuleAccess        bool
	ActiveReleaseTTL          time.Duration
	ArtifactBackend           string
	ArtifactEndpoint          string
	ArtifactBucket            string
	ArtifactProject           string
	ArtifactEnsureBucket      bool
	ArtifactGCSAnonymous      bool
	ArtifactPrefix            string
	ArtifactRegion            string
	ArtifactAccessKeyID       string
	ArtifactSecretAccessKey   string
	ArtifactPathStyle         bool
	PublicBaseURL             string
	AllowedPublicHosts        string
	TrustedProxyCIDRs         string
	TrustForwardedHeaders     bool
	SecurityHSTSEnabled       bool
	WebAuthMode               string
	OIDCIssuerURL             string
	OIDCClientID              string
	OIDCClientSecret          string
	OIDCRedirectURL           string
	OIDCLogoutURL             string
	OIDCCookieSecret          string
	OIDCScopes                string
	OIDCSigningAlgorithms     string
	UpstreamURL               string
	UpstreamProxyJSONCacheTTL time.Duration
	UpstreamProxyJSONStaleTTL time.Duration
	ForgeCacheMaxBodyBytes    int64
	ModuleUploadMaxBytes      int64
	UpstreamArtifactMaxBytes  int64
	UpstreamArtifactOrphanTTL time.Duration
	UpstreamSyncInterval      time.Duration
	UpstreamSyncLimit         int
	UpstreamSyncConcurrency   int
	MetricsModuleLimit        int
	MetricsRefreshInterval    time.Duration
	ReconcileArtifacts        bool
	ReconcileRepair           bool
}

func Load() (Config, error) {
	return loadArgs(nil, io.Discard)
}

func LoadArgs(args []string) (Config, error) {
	return loadArgs(args, io.Discard)
}

func LoadCommandLine(args []string) (Config, error) {
	return loadArgs(args, os.Stderr)
}

func loadArgs(args []string, output io.Writer) (Config, error) {
	cfg := Config{
		AppEnv:                    getEnv("APP_ENV", "dev"),
		LogLevel:                  getEnv("LOG_LEVEL", "info"),
		HTTPAddr:                  getEnv("HTTP_ADDR", ":8080"),
		MetricsAddr:               getEnv("METRICS_ADDR", ":9090"),
		DatabaseBackend:           os.Getenv("DATABASE_BACKEND"),
		DatabaseDSN:               os.Getenv("DATABASE_DSN"),
		DatabaseMaxConns:          10,
		DatabaseMinConns:          0,
		DatabaseMaxConnLifetime:   time.Hour,
		DatabaseMaxConnIdleTime:   30 * time.Minute,
		AdminToken:                os.Getenv("ADMIN_TOKEN"),
		AccessTokenPepper:         os.Getenv("ACCESS_TOKEN_PEPPER"),
		AccessTokenHistoryTTL:     90 * 24 * time.Hour,
		DeletedReleaseTTL:         90 * 24 * time.Hour,
		ManageSessionSecret:       os.Getenv("MANAGE_SESSION_SECRET"),
		PublicModuleAccess:        false,
		ActiveReleaseTTL:          30 * 24 * time.Hour,
		ArtifactBackend:           getEnv("ARTIFACT_BACKEND", "gcs"),
		ArtifactEndpoint:          getEnv("ARTIFACT_ENDPOINT", "https://storage.googleapis.com"),
		ArtifactBucket:            os.Getenv("ARTIFACT_BUCKET"),
		ArtifactProject:           os.Getenv("ARTIFACT_PROJECT"),
		ArtifactEnsureBucket:      false,
		ArtifactGCSAnonymous:      false,
		ArtifactPrefix:            getEnv("ARTIFACT_PREFIX", "modules"),
		ArtifactRegion:            getEnv("ARTIFACT_REGION", "us-east-1"),
		ArtifactAccessKeyID:       os.Getenv("ARTIFACT_ACCESS_KEY_ID"),
		ArtifactSecretAccessKey:   os.Getenv("ARTIFACT_SECRET_ACCESS_KEY"),
		ArtifactPathStyle:         true,
		PublicBaseURL:             os.Getenv("PUBLIC_BASE_URL"),
		AllowedPublicHosts:        os.Getenv("ALLOWED_PUBLIC_HOSTS"),
		TrustedProxyCIDRs:         os.Getenv("TRUSTED_PROXY_CIDRS"),
		TrustForwardedHeaders:     false,
		SecurityHSTSEnabled:       false,
		WebAuthMode:               getEnv("WEB_AUTH_MODE", "none"),
		OIDCIssuerURL:             os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:              os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:          os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:           os.Getenv("OIDC_REDIRECT_URL"),
		OIDCLogoutURL:             os.Getenv("OIDC_LOGOUT_URL"),
		OIDCCookieSecret:          os.Getenv("OIDC_COOKIE_SECRET"),
		OIDCScopes:                getEnv("OIDC_SCOPES", "openid profile email"),
		OIDCSigningAlgorithms:     getEnv("OIDC_SIGNING_ALGORITHMS", "RS256"),
		UpstreamURL:               getEnv("UPSTREAM_URL", "https://forgeapi.puppetlabs.com"),
		ReadHeaderTimeout:         10 * time.Second,
		IdleTimeout:               60 * time.Second,
		HTTPMaxHeaderBytes:        1 << 20,
		ShutdownTimeout:           10 * time.Second,
		UpstreamProxyJSONCacheTTL: 5 * time.Minute,
		UpstreamProxyJSONStaleTTL: time.Hour,
		ForgeCacheMaxBodyBytes:    1 << 20,
		ModuleUploadMaxBytes:      128 << 20,
		UpstreamArtifactMaxBytes:  128 << 20,
		UpstreamArtifactOrphanTTL: 24 * time.Hour,
		UpstreamSyncLimit:         1000,
		UpstreamSyncConcurrency:   8,
		MetricsModuleLimit:        10000,
		MetricsRefreshInterval:    30 * time.Second,
		ReconcileArtifacts:        false,
		ReconcileRepair:           false,
	}
	if err := applyTypedEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := applyFlags(&cfg, args, output); err != nil {
		return Config{}, err
	}
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	cfg.DatabaseBackend = strings.ToLower(strings.TrimSpace(cfg.DatabaseBackend))
	if cfg.DatabaseBackend == "" {
		cfg.DatabaseBackend = databaseBackendFromDSN(cfg.DatabaseDSN)
	}
	if cfg.Version {
		return cfg, nil
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func databaseBackendFromDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "sqlite://"):
		return "sqlite"
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "postgres"
	default:
		return ""
	}
}

func applyFlags(cfg *Config, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("puppet-forge", flag.ContinueOnError)
	flags.SetOutput(output)

	flags.BoolVar(&cfg.Version, "version", cfg.Version, "print build version and exit")
	flags.StringVar(&cfg.AppEnv, "app-env", cfg.AppEnv, "runtime environment name")
	flags.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "minimum log level: debug, info, warn, or error")
	flags.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "HTTP listen address")
	flags.StringVar(&cfg.MetricsAddr, "metrics-addr", cfg.MetricsAddr, "Prometheus metrics listen address")
	flags.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "maximum time to read HTTP request headers")
	flags.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "HTTP read timeout")
	flags.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "HTTP write timeout")
	flags.DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout, "HTTP keep-alive idle timeout")
	flags.IntVar(&cfg.HTTPMaxHeaderBytes, "http-max-header-bytes", cfg.HTTPMaxHeaderBytes, "maximum HTTP request header size")
	flags.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "graceful shutdown timeout")
	flags.StringVar(&cfg.DatabaseBackend, "database-backend", cfg.DatabaseBackend, "metadata database backend: postgres or sqlite")
	flags.StringVar(&cfg.DatabaseDSN, "database-dsn", cfg.DatabaseDSN, "metadata database DSN")
	flags.IntVar(&cfg.DatabaseMaxConns, "database-max-conns", cfg.DatabaseMaxConns, "maximum PostgreSQL pool connections per replica")
	flags.IntVar(&cfg.DatabaseMinConns, "database-min-conns", cfg.DatabaseMinConns, "minimum PostgreSQL pool connections per replica")
	flags.DurationVar(&cfg.DatabaseMaxConnLifetime, "database-max-conn-lifetime", cfg.DatabaseMaxConnLifetime, "maximum PostgreSQL connection lifetime")
	flags.DurationVar(&cfg.DatabaseMaxConnIdleTime, "database-max-conn-idle-time", cfg.DatabaseMaxConnIdleTime, "maximum PostgreSQL connection idle time")
	flags.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "runtime bootstrap admin token")
	flags.StringVar(&cfg.AccessTokenPepper, "access-token-pepper", cfg.AccessTokenPepper, "server-side pepper used to hash stored access tokens")
	flags.DurationVar(&cfg.AccessTokenHistoryTTL, "access-token-history-ttl", cfg.AccessTokenHistoryTTL, "retention period for expired and revoked access tokens; zero disables cleanup")
	flags.DurationVar(&cfg.DeletedReleaseTTL, "deleted-release-ttl", cfg.DeletedReleaseTTL, "retention period for deleted upstream release tombstones; zero disables cleanup")
	flags.StringVar(&cfg.ManageSessionSecret, "manage-session-secret", cfg.ManageSessionSecret, "shared secret for encrypted manage token sessions")
	flags.BoolVar(&cfg.PublicModuleAccess, "public-module-access", cfg.PublicModuleAccess, "allow unauthenticated module read/download access")
	flags.DurationVar(&cfg.ActiveReleaseTTL, "active-release-ttl", cfg.ActiveReleaseTTL, "active release protection TTL")
	flags.StringVar(&cfg.ArtifactBackend, "artifact-backend", cfg.ArtifactBackend, "artifact storage backend: gcs or s3")
	flags.StringVar(&cfg.ArtifactEndpoint, "artifact-endpoint", cfg.ArtifactEndpoint, "artifact storage endpoint")
	flags.StringVar(&cfg.ArtifactBucket, "artifact-bucket", cfg.ArtifactBucket, "artifact storage bucket")
	flags.StringVar(&cfg.ArtifactProject, "artifact-project", cfg.ArtifactProject, "GCS project for artifact bucket operations")
	flags.BoolVar(&cfg.ArtifactEnsureBucket, "artifact-ensure-bucket", cfg.ArtifactEnsureBucket, "create the GCS artifact bucket when it does not exist")
	flags.BoolVar(&cfg.ArtifactGCSAnonymous, "artifact-gcs-anonymous", cfg.ArtifactGCSAnonymous, "disable GCS authentication for a local emulator")
	flags.StringVar(&cfg.ArtifactPrefix, "artifact-prefix", cfg.ArtifactPrefix, "artifact storage prefix")
	flags.StringVar(&cfg.ArtifactRegion, "artifact-region", cfg.ArtifactRegion, "S3-compatible artifact storage region")
	flags.StringVar(&cfg.ArtifactAccessKeyID, "artifact-access-key-id", cfg.ArtifactAccessKeyID, "S3-compatible artifact storage access key id")
	flags.StringVar(&cfg.ArtifactSecretAccessKey, "artifact-secret-access-key", cfg.ArtifactSecretAccessKey, "S3-compatible artifact storage secret access key")
	flags.BoolVar(&cfg.ArtifactPathStyle, "artifact-path-style", cfg.ArtifactPathStyle, "use path-style S3 URLs")
	flags.StringVar(&cfg.PublicBaseURL, "public-base-url", cfg.PublicBaseURL, "optional public base URL fallback")
	flags.StringVar(&cfg.AllowedPublicHosts, "allowed-public-hosts", cfg.AllowedPublicHosts, "comma- or space-separated public request hosts")
	flags.StringVar(&cfg.TrustedProxyCIDRs, "trusted-proxy-cidrs", cfg.TrustedProxyCIDRs, "comma- or space-separated CIDRs allowed to supply X-Forwarded-For")
	flags.BoolVar(&cfg.TrustForwardedHeaders, "trust-forwarded-headers", cfg.TrustForwardedHeaders, "trust validated Forwarded and X-Forwarded-* headers from trusted proxy CIDRs")
	flags.BoolVar(&cfg.SecurityHSTSEnabled, "security-hsts-enabled", cfg.SecurityHSTSEnabled, "enable Strict-Transport-Security response header")
	flags.StringVar(&cfg.WebAuthMode, "web-auth-mode", cfg.WebAuthMode, "web auth mode: none or oidc")
	flags.StringVar(&cfg.OIDCIssuerURL, "oidc-issuer-url", cfg.OIDCIssuerURL, "OIDC issuer URL")
	flags.StringVar(&cfg.OIDCClientID, "oidc-client-id", cfg.OIDCClientID, "OIDC client id")
	flags.StringVar(&cfg.OIDCClientSecret, "oidc-client-secret", cfg.OIDCClientSecret, "OIDC client secret")
	flags.StringVar(&cfg.OIDCRedirectURL, "oidc-redirect-url", cfg.OIDCRedirectURL, "explicit OIDC redirect URL")
	flags.StringVar(&cfg.OIDCLogoutURL, "oidc-logout-url", cfg.OIDCLogoutURL, "explicit OIDC logout URL")
	flags.StringVar(&cfg.OIDCCookieSecret, "oidc-cookie-secret", cfg.OIDCCookieSecret, "OIDC cookie signing secret")
	flags.StringVar(&cfg.OIDCScopes, "oidc-scopes", cfg.OIDCScopes, "space-separated OIDC scopes")
	flags.StringVar(&cfg.OIDCSigningAlgorithms, "oidc-signing-algorithms", cfg.OIDCSigningAlgorithms, "space-separated allowed OIDC ID token signing algorithms")
	flags.StringVar(&cfg.UpstreamURL, "upstream-url", cfg.UpstreamURL, "upstream Puppet Forge API URL")
	flags.DurationVar(&cfg.UpstreamProxyJSONCacheTTL, "upstream-proxy-json-cache-ttl", cfg.UpstreamProxyJSONCacheTTL, "upstream JSON proxy cache TTL")
	flags.DurationVar(&cfg.UpstreamProxyJSONStaleTTL, "upstream-proxy-json-stale-ttl", cfg.UpstreamProxyJSONStaleTTL, "maximum age for stale upstream JSON fallback after cache expiry")
	flags.Int64Var(&cfg.ForgeCacheMaxBodyBytes, "forge-cache-max-body-bytes", cfg.ForgeCacheMaxBodyBytes, "maximum upstream JSON response size cached in memory")
	flags.Int64Var(&cfg.ModuleUploadMaxBytes, "module-upload-max-bytes", cfg.ModuleUploadMaxBytes, "maximum publish upload size in bytes")
	flags.Int64Var(&cfg.UpstreamArtifactMaxBytes, "upstream-artifact-max-bytes", cfg.UpstreamArtifactMaxBytes, "maximum upstream artifact size cached in object storage")
	flags.DurationVar(&cfg.UpstreamArtifactOrphanTTL, "upstream-artifact-orphan-ttl", cfg.UpstreamArtifactOrphanTTL, "retention grace period for unreferenced upstream cache artifacts; zero disables cleanup")
	flags.DurationVar(&cfg.UpstreamSyncInterval, "upstream-sync-interval", cfg.UpstreamSyncInterval, "background upstream refresh interval")
	flags.IntVar(&cfg.UpstreamSyncLimit, "upstream-sync-limit", cfg.UpstreamSyncLimit, "maximum upstream modules refreshed per cycle")
	flags.IntVar(&cfg.UpstreamSyncConcurrency, "upstream-sync-concurrency", cfg.UpstreamSyncConcurrency, "maximum concurrent upstream module refreshes")
	flags.IntVar(&cfg.MetricsModuleLimit, "metrics-module-limit", cfg.MetricsModuleLimit, "maximum owner series exported by inventory metrics")
	flags.DurationVar(&cfg.MetricsRefreshInterval, "metrics-refresh-interval", cfg.MetricsRefreshInterval, "module inventory metrics refresh interval")
	flags.BoolVar(&cfg.ReconcileArtifacts, "reconcile-artifacts", cfg.ReconcileArtifacts, "report SQL/object-storage artifact inconsistencies and exit")
	flags.BoolVar(&cfg.ReconcileRepair, "reconcile-repair", cfg.ReconcileRepair, "delete orphan objects found by artifact reconciliation")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	return nil
}

func validate(cfg Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("LOG_LEVEL must be 'debug', 'info', 'warn' or 'error'")
	}
	if cfg.ReconcileRepair && !cfg.ReconcileArtifacts {
		return errors.New("RECONCILE_REPAIR requires RECONCILE_ARTIFACTS=true")
	}
	if cfg.DatabaseDSN == "" {
		return errors.New("DATABASE_DSN is required")
	}
	switch cfg.DatabaseBackend {
	case "postgres":
		if !strings.HasPrefix(cfg.DatabaseDSN, "postgres://") && !strings.HasPrefix(cfg.DatabaseDSN, "postgresql://") {
			return errors.New("DATABASE_DSN must use postgres:// or postgresql:// when DATABASE_BACKEND=postgres")
		}
	case "sqlite":
		if !strings.HasPrefix(cfg.DatabaseDSN, "sqlite://") {
			return errors.New("DATABASE_DSN must use sqlite:// when DATABASE_BACKEND=sqlite")
		}
	default:
		return errors.New("DATABASE_BACKEND must be 'postgres' or 'sqlite'")
	}
	if cfg.DatabaseMaxConns <= 0 || cfg.DatabaseMaxConns > 1<<31-1 {
		return errors.New("DATABASE_MAX_CONNS must be between 1 and 2147483647")
	}
	if cfg.DatabaseMinConns < 0 || cfg.DatabaseMinConns > cfg.DatabaseMaxConns {
		return errors.New("DATABASE_MIN_CONNS must be between 0 and DATABASE_MAX_CONNS")
	}
	if cfg.DatabaseMaxConnLifetime < 0 || cfg.DatabaseMaxConnIdleTime < 0 {
		return errors.New("DATABASE_MAX_CONN_LIFETIME and DATABASE_MAX_CONN_IDLE_TIME must not be negative")
	}
	if strings.TrimSpace(cfg.HTTPAddr) == "" || strings.TrimSpace(cfg.MetricsAddr) == "" {
		return errors.New("HTTP_ADDR and METRICS_ADDR are required")
	}
	if cfg.HTTPAddr == cfg.MetricsAddr {
		return errors.New("HTTP_ADDR and METRICS_ADDR must use different listeners")
	}
	if len(strings.TrimSpace(cfg.AccessTokenPepper)) < 32 {
		return errors.New("ACCESS_TOKEN_PEPPER must contain at least 32 bytes")
	}
	if token := strings.TrimSpace(cfg.AdminToken); token != "" && len(token) < 32 {
		return errors.New("ADMIN_TOKEN must contain at least 32 bytes when set")
	}
	if cfg.AccessTokenHistoryTTL < 0 {
		return errors.New("ACCESS_TOKEN_HISTORY_TTL must not be negative")
	}
	if cfg.DeletedReleaseTTL < 0 {
		return errors.New("DELETED_RELEASE_TTL must not be negative")
	}
	if len(strings.TrimSpace(cfg.ManageSessionSecret)) < 32 {
		return errors.New("MANAGE_SESSION_SECRET must contain at least 32 bytes")
	}
	if strings.TrimSpace(cfg.ManageSessionSecret) == strings.TrimSpace(cfg.AccessTokenPepper) {
		return errors.New("MANAGE_SESSION_SECRET must differ from ACCESS_TOKEN_PEPPER")
	}
	switch cfg.ArtifactBackend {
	case "gcs":
		if cfg.ArtifactBucket == "" {
			return errors.New("ARTIFACT_BUCKET is required for ARTIFACT_BACKEND=gcs")
		}
		if cfg.ArtifactEnsureBucket && cfg.ArtifactProject == "" {
			return errors.New("ARTIFACT_PROJECT is required when ARTIFACT_ENSURE_BUCKET=true")
		}
	case "s3":
		if cfg.ArtifactBucket == "" {
			return errors.New("ARTIFACT_BUCKET is required for ARTIFACT_BACKEND=s3")
		}
		if cfg.ArtifactEndpoint == "" {
			return errors.New("ARTIFACT_ENDPOINT is required for ARTIFACT_BACKEND=s3")
		}
		endpoint, err := url.Parse(cfg.ArtifactEndpoint)
		if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
			return errors.New("ARTIFACT_ENDPOINT must be an absolute HTTP(S) URL for ARTIFACT_BACKEND=s3")
		}
		if (cfg.ArtifactAccessKeyID == "") != (cfg.ArtifactSecretAccessKey == "") {
			return errors.New("ARTIFACT_ACCESS_KEY_ID and ARTIFACT_SECRET_ACCESS_KEY must be set together")
		}
	default:
		return errors.New("ARTIFACT_BACKEND must be 'gcs' or 's3'")
	}
	switch cfg.WebAuthMode {
	case "none":
	case "oidc":
		if cfg.OIDCIssuerURL == "" || cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "" || cfg.OIDCCookieSecret == "" {
			return errors.New("OIDC_ISSUER_URL, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_COOKIE_SECRET are required for WEB_AUTH_MODE=oidc")
		}
		if len(strings.TrimSpace(cfg.OIDCCookieSecret)) < 32 {
			return errors.New("OIDC_COOKIE_SECRET must contain at least 32 bytes")
		}
		if err := validateOIDCSigningAlgorithms(cfg.OIDCSigningAlgorithms); err != nil {
			return err
		}
	default:
		return errors.New("WEB_AUTH_MODE must be 'none' or 'oidc'")
	}
	if cfg.ForgeCacheMaxBodyBytes <= 0 {
		return errors.New("FORGE_CACHE_MAX_BODY_BYTES must be greater than 0")
	}
	if cfg.ModuleUploadMaxBytes <= 0 {
		return errors.New("MODULE_UPLOAD_MAX_BYTES must be greater than 0")
	}
	if cfg.UpstreamArtifactMaxBytes <= 0 {
		return errors.New("UPSTREAM_ARTIFACT_MAX_BYTES must be greater than 0")
	}
	if cfg.UpstreamArtifactOrphanTTL < 0 {
		return errors.New("UPSTREAM_ARTIFACT_ORPHAN_TTL must not be negative")
	}
	if cfg.UpstreamProxyJSONCacheTTL < 0 || cfg.UpstreamProxyJSONStaleTTL < 0 || cfg.UpstreamSyncInterval < 0 {
		return errors.New("UPSTREAM_PROXY_JSON_CACHE_TTL, UPSTREAM_PROXY_JSON_STALE_TTL and UPSTREAM_SYNC_INTERVAL must not be negative")
	}
	if cfg.UpstreamSyncConcurrency <= 0 {
		return errors.New("UPSTREAM_SYNC_CONCURRENCY must be greater than 0")
	}
	if cfg.UpstreamSyncLimit <= 0 {
		return errors.New("UPSTREAM_SYNC_LIMIT must be greater than 0")
	}
	if cfg.MetricsModuleLimit <= 0 {
		return errors.New("METRICS_MODULE_LIMIT must be greater than 0")
	}
	if cfg.MetricsRefreshInterval <= 0 {
		return errors.New("METRICS_REFRESH_INTERVAL must be greater than 0")
	}
	if cfg.ReadHeaderTimeout <= 0 {
		return errors.New("READ_HEADER_TIMEOUT must be greater than 0")
	}
	if cfg.ReadTimeout < 0 || cfg.WriteTimeout < 0 || cfg.IdleTimeout <= 0 {
		return errors.New("READ_TIMEOUT and WRITE_TIMEOUT must not be negative, and IDLE_TIMEOUT must be greater than 0")
	}
	if cfg.ShutdownTimeout <= 0 {
		return errors.New("SHUTDOWN_TIMEOUT must be greater than 0")
	}
	if cfg.ActiveReleaseTTL <= 0 {
		return errors.New("ACTIVE_RELEASE_TTL must be greater than 0")
	}
	if cfg.HTTPMaxHeaderBytes < 4096 || cfg.HTTPMaxHeaderBytes > 16<<20 {
		return errors.New("HTTP_MAX_HEADER_BYTES must be between 4096 and 16777216")
	}
	if _, err := ParseTrustedProxyCIDRs(cfg.TrustedProxyCIDRs); err != nil {
		return err
	}
	if cfg.TrustForwardedHeaders && strings.TrimSpace(cfg.TrustedProxyCIDRs) == "" {
		return errors.New("TRUSTED_PROXY_CIDRS is required when TRUST_FORWARDED_HEADERS=true")
	}
	return nil
}

func validateOIDCSigningAlgorithms(value string) error {
	allowed := map[string]struct{}{
		"RS256": {}, "RS384": {}, "RS512": {},
		"PS256": {}, "PS384": {}, "PS512": {},
		"ES256": {}, "ES384": {}, "ES512": {},
		"EdDSA": {},
	}
	algorithms := strings.Fields(value)
	if len(algorithms) == 0 {
		return errors.New("OIDC_SIGNING_ALGORITHMS must contain at least one asymmetric signing algorithm")
	}
	for _, algorithm := range algorithms {
		if _, ok := allowed[algorithm]; !ok {
			return fmt.Errorf("OIDC_SIGNING_ALGORITHMS contains unsupported or unsafe algorithm %q", algorithm)
		}
	}
	return nil
}

func ParseTrustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	values := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	allProxy := false
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		switch strings.ToLower(value) {
		case "", "*", "all":
			allProxy = true
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q: %w", value, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	if allProxy {
		prefixes = []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
			netip.MustParsePrefix("::/0"),
		}
	}
	return prefixes, nil
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func applyTypedEnv(cfg *Config) error {
	booleans := []struct {
		key    string
		target *bool
	}{
		{"PUBLIC_MODULE_ACCESS", &cfg.PublicModuleAccess},
		{"ARTIFACT_ENSURE_BUCKET", &cfg.ArtifactEnsureBucket},
		{"ARTIFACT_GCS_ANONYMOUS", &cfg.ArtifactGCSAnonymous},
		{"ARTIFACT_PATH_STYLE", &cfg.ArtifactPathStyle},
		{"TRUST_FORWARDED_HEADERS", &cfg.TrustForwardedHeaders},
		{"SECURITY_HSTS_ENABLED", &cfg.SecurityHSTSEnabled},
		{"RECONCILE_ARTIFACTS", &cfg.ReconcileArtifacts},
		{"RECONCILE_REPAIR", &cfg.ReconcileRepair},
	}
	for _, item := range booleans {
		if err := setBoolEnv(item.target, item.key); err != nil {
			return err
		}
	}

	durations := []struct {
		key    string
		target *time.Duration
	}{
		{"ACCESS_TOKEN_HISTORY_TTL", &cfg.AccessTokenHistoryTTL},
		{"DELETED_RELEASE_TTL", &cfg.DeletedReleaseTTL},
		{"ACTIVE_RELEASE_TTL", &cfg.ActiveReleaseTTL},
		{"READ_HEADER_TIMEOUT", &cfg.ReadHeaderTimeout},
		{"READ_TIMEOUT", &cfg.ReadTimeout},
		{"WRITE_TIMEOUT", &cfg.WriteTimeout},
		{"IDLE_TIMEOUT", &cfg.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"UPSTREAM_PROXY_JSON_CACHE_TTL", &cfg.UpstreamProxyJSONCacheTTL},
		{"UPSTREAM_PROXY_JSON_STALE_TTL", &cfg.UpstreamProxyJSONStaleTTL},
		{"UPSTREAM_SYNC_INTERVAL", &cfg.UpstreamSyncInterval},
		{"UPSTREAM_ARTIFACT_ORPHAN_TTL", &cfg.UpstreamArtifactOrphanTTL},
		{"DATABASE_MAX_CONN_LIFETIME", &cfg.DatabaseMaxConnLifetime},
		{"DATABASE_MAX_CONN_IDLE_TIME", &cfg.DatabaseMaxConnIdleTime},
		{"METRICS_REFRESH_INTERVAL", &cfg.MetricsRefreshInterval},
	}
	for _, item := range durations {
		if err := setDurationEnv(item.target, item.key); err != nil {
			return err
		}
	}

	ints := []struct {
		key    string
		target *int
	}{
		{"HTTP_MAX_HEADER_BYTES", &cfg.HTTPMaxHeaderBytes},
		{"UPSTREAM_SYNC_LIMIT", &cfg.UpstreamSyncLimit},
		{"UPSTREAM_SYNC_CONCURRENCY", &cfg.UpstreamSyncConcurrency},
		{"METRICS_MODULE_LIMIT", &cfg.MetricsModuleLimit},
		{"DATABASE_MAX_CONNS", &cfg.DatabaseMaxConns},
		{"DATABASE_MIN_CONNS", &cfg.DatabaseMinConns},
	}
	for _, item := range ints {
		if err := setIntEnv(item.target, item.key); err != nil {
			return err
		}
	}

	int64s := []struct {
		key    string
		target *int64
	}{
		{"FORGE_CACHE_MAX_BODY_BYTES", &cfg.ForgeCacheMaxBodyBytes},
		{"MODULE_UPLOAD_MAX_BYTES", &cfg.ModuleUploadMaxBytes},
		{"UPSTREAM_ARTIFACT_MAX_BYTES", &cfg.UpstreamArtifactMaxBytes},
	}
	for _, item := range int64s {
		if err := setInt64Env(item.target, item.key); err != nil {
			return err
		}
	}
	return nil
}

func setBoolEnv(target *bool, key string) error {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}

	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("invalid %s boolean %q: %w", key, raw, err)
	}
	*target = parsed
	return nil
}

func setDurationEnv(target *time.Duration, key string) error {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid %s duration %q: %w", key, raw, err)
	}
	*target = parsed
	return nil
}

func setInt64Env(target *int64, key string) error {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid %s integer %q: %w", key, raw, err)
	}
	*target = value
	return nil
}

func setIntEnv(target *int, key string) error {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}

	value, err := strconv.ParseInt(raw, 10, strconv.IntSize)
	if err != nil {
		return fmt.Errorf("invalid %s integer %q: %w", key, raw, err)
	}
	*target = int(value)
	return nil
}
