package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Version                   bool
	AppEnv                    string
	HTTPAddr                  string
	ReadTimeout               time.Duration
	WriteTimeout              time.Duration
	ShutdownTimeout           time.Duration
	DatabaseDSN               string
	AdminToken                string
	ManageSessionSecret       string
	PublicModuleAccess        bool
	ActiveReleaseTTL          time.Duration
	ArtifactBackend           string
	ArtifactEndpoint          string
	ArtifactBucket            string
	ArtifactProject           string
	ArtifactPrefix            string
	ArtifactRegion            string
	ArtifactAccessKeyID       string
	ArtifactSecretAccessKey   string
	ArtifactPathStyle         bool
	PublicBaseURL             string
	SecurityHSTSEnabled       bool
	WebAuthMode               string
	OIDCIssuerURL             string
	OIDCClientID              string
	OIDCClientSecret          string
	OIDCRedirectURL           string
	OIDCLogoutURL             string
	OIDCCookieSecret          string
	UpstreamURL               string
	UpstreamProxyJSONCacheTTL time.Duration
	UpstreamProxyJSONStaleTTL time.Duration
	ForgeCacheMaxBodyBytes    int64
	ModuleUploadMaxBytes      int64
	UpstreamArtifactMaxBytes  int64
	UpstreamSyncInterval      time.Duration
	UpstreamSyncLimit         int
	MetricsModuleLimit        int
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

func loadArgs(args []string, output io.Writer) (cfg Config, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("config: %v", r)
		}
	}()
	cfg = Config{
		AppEnv:                    getEnv("APP_ENV", "dev"),
		HTTPAddr:                  getEnv("HTTP_ADDR", ":8080"),
		DatabaseDSN:               os.Getenv("DATABASE_DSN"),
		AdminToken:                os.Getenv("ADMIN_TOKEN"),
		ManageSessionSecret:       os.Getenv("MANAGE_SESSION_SECRET"),
		PublicModuleAccess:        getEnv("PUBLIC_MODULE_ACCESS", "false") == "true",
		ActiveReleaseTTL:          mustDuration("ACTIVE_RELEASE_TTL", 30*24*time.Hour),
		ArtifactBackend:           getEnv("ARTIFACT_BACKEND", "gcs"),
		ArtifactEndpoint:          getEnv("ARTIFACT_ENDPOINT", "https://storage.googleapis.com"),
		ArtifactBucket:            os.Getenv("ARTIFACT_BUCKET"),
		ArtifactProject:           os.Getenv("ARTIFACT_PROJECT"),
		ArtifactPrefix:            getEnv("ARTIFACT_PREFIX", "modules"),
		ArtifactRegion:            getEnv("ARTIFACT_REGION", "us-east-1"),
		ArtifactAccessKeyID:       os.Getenv("ARTIFACT_ACCESS_KEY_ID"),
		ArtifactSecretAccessKey:   os.Getenv("ARTIFACT_SECRET_ACCESS_KEY"),
		ArtifactPathStyle:         getEnv("ARTIFACT_PATH_STYLE", "true") == "true",
		PublicBaseURL:             os.Getenv("PUBLIC_BASE_URL"),
		SecurityHSTSEnabled:       getEnv("SECURITY_HSTS_ENABLED", "false") == "true",
		WebAuthMode:               getEnv("WEB_AUTH_MODE", "none"),
		OIDCIssuerURL:             os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:              os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:          os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:           os.Getenv("OIDC_REDIRECT_URL"),
		OIDCLogoutURL:             os.Getenv("OIDC_LOGOUT_URL"),
		OIDCCookieSecret:          os.Getenv("OIDC_COOKIE_SECRET"),
		UpstreamURL:               getEnv("UPSTREAM_URL", "https://forgeapi.puppetlabs.com"),
		ReadTimeout:               mustDuration("READ_TIMEOUT", 10*time.Second),
		WriteTimeout:              mustDuration("WRITE_TIMEOUT", 30*time.Second),
		ShutdownTimeout:           mustDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		UpstreamProxyJSONCacheTTL: mustDuration("UPSTREAM_PROXY_JSON_CACHE_TTL", 5*time.Minute),
		UpstreamProxyJSONStaleTTL: mustDuration("UPSTREAM_PROXY_JSON_STALE_TTL", time.Hour),
		ForgeCacheMaxBodyBytes:    mustInt64("FORGE_CACHE_MAX_BODY_BYTES", 1<<20),
		ModuleUploadMaxBytes:      mustInt64("MODULE_UPLOAD_MAX_BYTES", 128<<20),
		UpstreamArtifactMaxBytes:  mustInt64("UPSTREAM_ARTIFACT_MAX_BYTES", 128<<20),
		UpstreamSyncInterval:      mustDuration("UPSTREAM_SYNC_INTERVAL", 0),
		UpstreamSyncLimit:         mustInt("UPSTREAM_SYNC_LIMIT", 1000),
		MetricsModuleLimit:        mustInt("METRICS_MODULE_LIMIT", 10000),
	}
	if err := applyFlags(&cfg, args, output); err != nil {
		return Config{}, err
	}
	if cfg.Version {
		return cfg, nil
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyFlags(cfg *Config, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("puppet-forge", flag.ContinueOnError)
	flags.SetOutput(output)

	flags.BoolVar(&cfg.Version, "version", cfg.Version, "print build version and exit")
	flags.StringVar(&cfg.AppEnv, "app-env", cfg.AppEnv, "runtime environment name")
	flags.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "HTTP listen address")
	flags.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "HTTP read timeout")
	flags.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "HTTP write timeout")
	flags.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "graceful shutdown timeout")
	flags.StringVar(&cfg.DatabaseDSN, "database-dsn", cfg.DatabaseDSN, "metadata database DSN")
	flags.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "runtime bootstrap admin token")
	flags.StringVar(&cfg.ManageSessionSecret, "manage-session-secret", cfg.ManageSessionSecret, "shared secret for encrypted manage token sessions")
	flags.BoolVar(&cfg.PublicModuleAccess, "public-module-access", cfg.PublicModuleAccess, "allow unauthenticated module read/download access")
	flags.DurationVar(&cfg.ActiveReleaseTTL, "active-release-ttl", cfg.ActiveReleaseTTL, "active release protection TTL")
	flags.StringVar(&cfg.ArtifactBackend, "artifact-backend", cfg.ArtifactBackend, "artifact storage backend: gcs or s3")
	flags.StringVar(&cfg.ArtifactEndpoint, "artifact-endpoint", cfg.ArtifactEndpoint, "artifact storage endpoint")
	flags.StringVar(&cfg.ArtifactBucket, "artifact-bucket", cfg.ArtifactBucket, "artifact storage bucket")
	flags.StringVar(&cfg.ArtifactProject, "artifact-project", cfg.ArtifactProject, "GCS project for artifact bucket operations")
	flags.StringVar(&cfg.ArtifactPrefix, "artifact-prefix", cfg.ArtifactPrefix, "artifact storage prefix")
	flags.StringVar(&cfg.ArtifactRegion, "artifact-region", cfg.ArtifactRegion, "S3-compatible artifact storage region")
	flags.StringVar(&cfg.ArtifactAccessKeyID, "artifact-access-key-id", cfg.ArtifactAccessKeyID, "S3-compatible artifact storage access key id")
	flags.StringVar(&cfg.ArtifactSecretAccessKey, "artifact-secret-access-key", cfg.ArtifactSecretAccessKey, "S3-compatible artifact storage secret access key")
	flags.BoolVar(&cfg.ArtifactPathStyle, "artifact-path-style", cfg.ArtifactPathStyle, "use path-style S3 URLs")
	flags.StringVar(&cfg.PublicBaseURL, "public-base-url", cfg.PublicBaseURL, "optional public base URL fallback")
	flags.BoolVar(&cfg.SecurityHSTSEnabled, "security-hsts-enabled", cfg.SecurityHSTSEnabled, "enable Strict-Transport-Security response header")
	flags.StringVar(&cfg.WebAuthMode, "web-auth-mode", cfg.WebAuthMode, "web auth mode: none or oidc")
	flags.StringVar(&cfg.OIDCIssuerURL, "oidc-issuer-url", cfg.OIDCIssuerURL, "OIDC issuer URL")
	flags.StringVar(&cfg.OIDCClientID, "oidc-client-id", cfg.OIDCClientID, "OIDC client id")
	flags.StringVar(&cfg.OIDCClientSecret, "oidc-client-secret", cfg.OIDCClientSecret, "OIDC client secret")
	flags.StringVar(&cfg.OIDCRedirectURL, "oidc-redirect-url", cfg.OIDCRedirectURL, "explicit OIDC redirect URL")
	flags.StringVar(&cfg.OIDCLogoutURL, "oidc-logout-url", cfg.OIDCLogoutURL, "explicit OIDC logout URL")
	flags.StringVar(&cfg.OIDCCookieSecret, "oidc-cookie-secret", cfg.OIDCCookieSecret, "OIDC cookie signing secret")
	flags.StringVar(&cfg.UpstreamURL, "upstream-url", cfg.UpstreamURL, "upstream Puppet Forge API URL")
	flags.DurationVar(&cfg.UpstreamProxyJSONCacheTTL, "upstream-proxy-json-cache-ttl", cfg.UpstreamProxyJSONCacheTTL, "upstream JSON proxy cache TTL")
	flags.DurationVar(&cfg.UpstreamProxyJSONStaleTTL, "upstream-proxy-json-stale-ttl", cfg.UpstreamProxyJSONStaleTTL, "maximum age for stale upstream JSON fallback after cache expiry")
	flags.Int64Var(&cfg.ForgeCacheMaxBodyBytes, "forge-cache-max-body-bytes", cfg.ForgeCacheMaxBodyBytes, "maximum upstream JSON response size cached in memory")
	flags.Int64Var(&cfg.ModuleUploadMaxBytes, "module-upload-max-bytes", cfg.ModuleUploadMaxBytes, "maximum publish upload size in bytes")
	flags.Int64Var(&cfg.UpstreamArtifactMaxBytes, "upstream-artifact-max-bytes", cfg.UpstreamArtifactMaxBytes, "maximum upstream artifact size cached in object storage")
	flags.DurationVar(&cfg.UpstreamSyncInterval, "upstream-sync-interval", cfg.UpstreamSyncInterval, "background upstream refresh interval")
	flags.IntVar(&cfg.UpstreamSyncLimit, "upstream-sync-limit", cfg.UpstreamSyncLimit, "maximum upstream modules refreshed per cycle")
	flags.IntVar(&cfg.MetricsModuleLimit, "metrics-module-limit", cfg.MetricsModuleLimit, "maximum modules exported by inventory metrics")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	return nil
}

func validate(cfg Config) error {
	if cfg.DatabaseDSN == "" {
		return errors.New("DATABASE_DSN is required")
	}
	switch cfg.ArtifactBackend {
	case "gcs":
		if cfg.ArtifactBucket == "" {
			return errors.New("ARTIFACT_BUCKET is required for ARTIFACT_BACKEND=gcs")
		}
		if cfg.ArtifactProject == "" {
			return errors.New("ARTIFACT_PROJECT is required for ARTIFACT_BACKEND=gcs")
		}
	case "s3":
		if cfg.ArtifactBucket == "" {
			return errors.New("ARTIFACT_BUCKET is required for ARTIFACT_BACKEND=s3")
		}
		if cfg.ArtifactEndpoint == "" {
			return errors.New("ARTIFACT_ENDPOINT is required for ARTIFACT_BACKEND=s3")
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
	if cfg.MetricsModuleLimit <= 0 {
		return errors.New("METRICS_MODULE_LIMIT must be greater than 0")
	}

	return nil
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func mustDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid duration for %s: %v", key, err))
	}

	return parsed
}

func mustInt64(key string, fallback int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid int64 for %s: %v", key, err))
	}

	return value
}

func mustInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	value, err := strconv.ParseInt(raw, 10, strconv.IntSize)
	if err != nil {
		panic(fmt.Sprintf("invalid int for %s: %v", key, err))
	}

	return int(value)
}
