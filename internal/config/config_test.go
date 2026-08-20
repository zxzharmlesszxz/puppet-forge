package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadGCSConfigWithDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ArtifactBackend != "gcs" {
		t.Fatalf("expected gcs backend, got %s", cfg.ArtifactBackend)
	}
	if cfg.DatabaseBackend != "sqlite" {
		t.Fatalf("unexpected inferred DATABASE_BACKEND: %q", cfg.DatabaseBackend)
	}
	if cfg.ArtifactEnsureBucket {
		t.Fatal("expected ARTIFACT_ENSURE_BUCKET to be disabled by default")
	}
	if cfg.ArtifactGCSAnonymous {
		t.Fatal("expected ARTIFACT_GCS_ANONYMOUS to be disabled by default")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("unexpected HTTP_ADDR default: %s", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("unexpected LOG_LEVEL default: %q", cfg.LogLevel)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Fatalf("unexpected METRICS_ADDR default: %s", cfg.MetricsAddr)
	}
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("streaming request timeouts = read %s, write %s; want disabled defaults", cfg.ReadTimeout, cfg.WriteTimeout)
	}
	if cfg.PublicBaseURL != "" {
		t.Fatalf("unexpected PUBLIC_BASE_URL default: %s", cfg.PublicBaseURL)
	}
	if cfg.UpstreamProxyJSONCacheTTL != 5*time.Minute {
		t.Fatalf("unexpected upstream JSON cache TTL: %s", cfg.UpstreamProxyJSONCacheTTL)
	}
	if cfg.UpstreamProxyJSONStaleTTL != time.Hour {
		t.Fatalf("unexpected upstream JSON stale TTL: %s", cfg.UpstreamProxyJSONStaleTTL)
	}
	if cfg.ActiveReleaseTTL != 30*24*time.Hour {
		t.Fatalf("unexpected ACTIVE_RELEASE_TTL default: %s", cfg.ActiveReleaseTTL)
	}
	if cfg.AccessTokenHistoryTTL != 90*24*time.Hour {
		t.Fatalf("unexpected ACCESS_TOKEN_HISTORY_TTL default: %s", cfg.AccessTokenHistoryTTL)
	}
	if cfg.DeletedReleaseTTL != 90*24*time.Hour {
		t.Fatalf("unexpected DELETED_RELEASE_TTL default: %s", cfg.DeletedReleaseTTL)
	}
	if cfg.SecurityHSTSEnabled {
		t.Fatal("expected SECURITY_HSTS_ENABLED default to be false")
	}
	if cfg.ModuleUploadMaxBytes != 128<<20 {
		t.Fatalf("unexpected MODULE_UPLOAD_MAX_BYTES default: %d", cfg.ModuleUploadMaxBytes)
	}
	if cfg.UpstreamArtifactMaxBytes != 128<<20 {
		t.Fatalf("unexpected UPSTREAM_ARTIFACT_MAX_BYTES default: %d", cfg.UpstreamArtifactMaxBytes)
	}
	if cfg.UpstreamArtifactOrphanTTL != 24*time.Hour {
		t.Fatalf("unexpected UPSTREAM_ARTIFACT_ORPHAN_TTL default: %s", cfg.UpstreamArtifactOrphanTTL)
	}
	if cfg.DatabaseMaxConns != 10 || cfg.DatabaseMinConns != 0 {
		t.Fatalf("unexpected database pool defaults: max=%d min=%d", cfg.DatabaseMaxConns, cfg.DatabaseMinConns)
	}
	if cfg.DatabaseMaxConnLifetime != time.Hour || cfg.DatabaseMaxConnIdleTime != 30*time.Minute {
		t.Fatalf("unexpected database pool lifetime defaults: lifetime=%s idle=%s", cfg.DatabaseMaxConnLifetime, cfg.DatabaseMaxConnIdleTime)
	}
	if cfg.MetricsModuleLimit != 10000 {
		t.Fatalf("unexpected METRICS_MODULE_LIMIT default: %d", cfg.MetricsModuleLimit)
	}
	if cfg.MetricsRefreshInterval != 30*time.Second {
		t.Fatalf("unexpected METRICS_REFRESH_INTERVAL default: %s", cfg.MetricsRefreshInterval)
	}
	if cfg.TrustedProxyCIDRs != "" {
		t.Fatalf("unexpected TRUSTED_PROXY_CIDRS default: %q", cfg.TrustedProxyCIDRs)
	}
	if cfg.TrustForwardedHeaders {
		t.Fatal("expected TRUST_FORWARDED_HEADERS default to be false")
	}
	if cfg.AllowedPublicHosts != "" {
		t.Fatalf("unexpected ALLOWED_PUBLIC_HOSTS default: %q", cfg.AllowedPublicHosts)
	}
	if cfg.OIDCScopes != "openid profile email" {
		t.Fatalf("unexpected OIDC_SCOPES default: %q", cfg.OIDCScopes)
	}
	if cfg.OIDCSigningAlgorithms != "RS256" {
		t.Fatalf("unexpected OIDC_SIGNING_ALGORITHMS default: %q", cfg.OIDCSigningAlgorithms)
	}
}

func TestLoadReadsAdminToken(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ADMIN_TOKEN", "bootstrap-token-with-at-least-32-bytes")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AdminToken != "bootstrap-token-with-at-least-32-bytes" {
		t.Fatalf("unexpected ADMIN_TOKEN: %q", cfg.AdminToken)
	}
}

func TestLoadRejectsShortAdminToken(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ADMIN_TOKEN", "too-short")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_TOKEN must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want admin token validation", err)
	}
}

func TestLoadArgsRejectsShortAdminToken(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	_, err := LoadArgs([]string{"--admin-token", "too-short"})
	if err == nil || !strings.Contains(err.Error(), "ADMIN_TOKEN must contain at least 32 bytes") {
		t.Fatalf("LoadArgs() error = %v, want admin token validation", err)
	}
}

func TestLoadReadsLogLevel(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("LOG_LEVEL", "WARN")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("unexpected LOG_LEVEL: %q", cfg.LogLevel)
	}
}

func TestLoadRejectsShortAccessTokenPepper(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ACCESS_TOKEN_PEPPER", "too-short")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ACCESS_TOKEN_PEPPER must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want access token pepper validation", err)
	}
}

func TestLoadReadsManageSessionSecret(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("MANAGE_SESSION_SECRET", "shared-manage-session-secret-32-bytes")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ManageSessionSecret != "shared-manage-session-secret-32-bytes" {
		t.Fatalf("unexpected MANAGE_SESSION_SECRET: %q", cfg.ManageSessionSecret)
	}
}

func TestLoadRejectsShortManageSessionSecret(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("MANAGE_SESSION_SECRET", "too-short")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MANAGE_SESSION_SECRET must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want manage session secret validation", err)
	}
}

func TestLoadRejectsMissingManageSessionSecret(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("MANAGE_SESSION_SECRET", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MANAGE_SESSION_SECRET must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want required manage session secret validation", err)
	}
}

func TestLoadRejectsManageSessionSecretMatchingAccessTokenPepper(t *testing.T) {
	clearConfigEnv(t)
	const sharedSecret = "shared-secret-material-at-least-32-bytes"
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ACCESS_TOKEN_PEPPER", sharedSecret)
	t.Setenv("MANAGE_SESSION_SECRET", sharedSecret)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MANAGE_SESSION_SECRET must differ from ACCESS_TOKEN_PEPPER") {
		t.Fatalf("Load() error = %v, want distinct-secret error", err)
	}
}

func TestLoadReadsPublicModuleAccess(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("PUBLIC_MODULE_ACCESS", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.PublicModuleAccess {
		t.Fatal("expected PUBLIC_MODULE_ACCESS=true")
	}
}

func TestLoadReadsActiveReleaseTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ACTIVE_RELEASE_TTL", "168h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ActiveReleaseTTL != 168*time.Hour {
		t.Fatalf("unexpected ACTIVE_RELEASE_TTL: %s", cfg.ActiveReleaseTTL)
	}
}

func TestLoadReadsAccessTokenHistoryTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ACCESS_TOKEN_HISTORY_TTL", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AccessTokenHistoryTTL != 0 {
		t.Fatalf("ACCESS_TOKEN_HISTORY_TTL = %s, want disabled", cfg.AccessTokenHistoryTTL)
	}
}

func TestLoadRejectsNegativeAccessTokenHistoryTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ACCESS_TOKEN_HISTORY_TTL", "-1h")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ACCESS_TOKEN_HISTORY_TTL must not be negative") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadReadsDeletedReleaseTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("DELETED_RELEASE_TTL", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DeletedReleaseTTL != 0 {
		t.Fatalf("DELETED_RELEASE_TTL = %s, want disabled", cfg.DeletedReleaseTTL)
	}
}

func TestLoadRejectsNegativeDeletedReleaseTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("DELETED_RELEASE_TTL", "-1h")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DELETED_RELEASE_TTL must not be negative") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsMissingOIDCFields(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://issuer.example.com")
	t.Setenv("OIDC_CLIENT_ID", "client-id")

	_, err := Load()
	if err == nil {
		t.Fatal("expected WEB_AUTH_MODE=oidc validation error")
	}
	if !strings.Contains(err.Error(), "OIDC_ISSUER_URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadKeepsOIDCRedirectURLOptional(t *testing.T) {
	setValidOIDCConfigEnv(t)
	t.Setenv("OIDC_LOGOUT_URL", "https://auth.example.com/application/o/forge/end-session/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.OIDCRedirectURL != "" {
		t.Fatalf("unexpected OIDC redirect URL: %s", cfg.OIDCRedirectURL)
	}
	if cfg.OIDCLogoutURL != "https://auth.example.com/application/o/forge/end-session/" {
		t.Fatalf("unexpected OIDC logout URL: %s", cfg.OIDCLogoutURL)
	}
}

func TestLoadReadsExplicitOIDCRedirectURL(t *testing.T) {
	setValidOIDCConfigEnv(t)
	t.Setenv("OIDC_REDIRECT_URL", "https://forge.example.com/auth/callback")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.OIDCRedirectURL != "https://forge.example.com/auth/callback" {
		t.Fatalf("unexpected OIDC redirect URL: %s", cfg.OIDCRedirectURL)
	}
}

func setValidOIDCConfigEnv(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com/application/o/forge/")
	t.Setenv("OIDC_CLIENT_ID", "forge")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_COOKIE_SECRET", strings.Repeat("cookie-secret-", 3))
}

func TestLoadReadsTrustedProxyCIDRsAndOIDCScopes(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 2001:db8::/32")
	t.Setenv("OIDC_SCOPES", "openid profile email groups")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TrustedProxyCIDRs != "10.0.0.0/8, 2001:db8::/32" {
		t.Fatalf("unexpected TRUSTED_PROXY_CIDRS: %q", cfg.TrustedProxyCIDRs)
	}
	if cfg.OIDCScopes != "openid profile email groups" {
		t.Fatalf("unexpected OIDC_SCOPES: %q", cfg.OIDCScopes)
	}
}

func TestLoadAcceptsTrustedProxyCIDRsAllMarker(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("TRUSTED_PROXY_CIDRS", "*")
	t.Setenv("TRUST_FORWARDED_HEADERS", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TrustedProxyCIDRs != "*" {
		t.Fatalf("unexpected TRUSTED_PROXY_CIDRS: %q", cfg.TrustedProxyCIDRs)
	}
	prefixes, err := ParseTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("ParseTrustedProxyCIDRs() error = %v", err)
	}
	if len(prefixes) != 2 {
		t.Fatalf("unexpected trusted prefixes count: %d", len(prefixes))
	}
}

func TestLoadRejectsUnsafeOIDCSigningAlgorithm(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com")
	t.Setenv("OIDC_CLIENT_ID", "forge")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_COOKIE_SECRET", strings.Repeat("cookie-secret-", 3))
	t.Setenv("OIDC_SIGNING_ALGORITHMS", "RS256 none")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "unsupported or unsafe algorithm") {
		t.Fatalf("Load() error = %v, want unsafe signing algorithm rejection", err)
	}
}

func TestLoadRejectsShortOIDCCookieSecret(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com")
	t.Setenv("OIDC_CLIENT_ID", "forge")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_COOKIE_SECRET", "short-secret")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_COOKIE_SECRET must contain at least 32 bytes") {
		t.Fatalf("Load() error = %v, want short OIDC cookie secret rejection", err)
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")

	_, err := Load()
	if err == nil {
		t.Fatal("expected TRUSTED_PROXY_CIDRS validation error")
	}
	if !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsForwardedTrustWithoutProxyCIDRs(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("TRUST_FORWARDED_HEADERS", "true")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS is required") {
		t.Fatalf("Load() error = %v, want trusted proxy requirement", err)
	}
}

func TestLoadRejectsSharedApplicationAndMetricsListener(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("METRICS_ADDR", ":8080")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "different listeners") {
		t.Fatalf("Load() error = %v, want distinct listener validation", err)
	}
}

func TestLoadAcceptsS3Backend(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "postgres://forge:forge@localhost:5432/forge?sslmode=disable")
	t.Setenv("ARTIFACT_BACKEND", "s3")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_ENDPOINT", "http://minio:9000")
	t.Setenv("ARTIFACT_ACCESS_KEY_ID", "minioadmin")
	t.Setenv("ARTIFACT_SECRET_ACCESS_KEY", "minioadmin")
	t.Setenv("READ_TIMEOUT", "15s")
	t.Setenv("READ_HEADER_TIMEOUT", "4s")
	t.Setenv("IDLE_TIMEOUT", "45s")
	t.Setenv("HTTP_MAX_HEADER_BYTES", "32768")
	t.Setenv("UPSTREAM_SYNC_LIMIT", "42")
	t.Setenv("UPSTREAM_SYNC_CONCURRENCY", "6")
	t.Setenv("MODULE_UPLOAD_MAX_BYTES", "2048")
	t.Setenv("UPSTREAM_ARTIFACT_MAX_BYTES", "4096")
	t.Setenv("UPSTREAM_PROXY_JSON_STALE_TTL", "30m")
	t.Setenv("METRICS_MODULE_LIMIT", "123")
	t.Setenv("METRICS_REFRESH_INTERVAL", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ArtifactBackend != "s3" {
		t.Fatalf("expected s3 backend, got %s", cfg.ArtifactBackend)
	}
	if cfg.ReadTimeout != 15*time.Second {
		t.Fatalf("unexpected READ_TIMEOUT: %s", cfg.ReadTimeout)
	}
	if cfg.ReadHeaderTimeout != 4*time.Second || cfg.IdleTimeout != 45*time.Second || cfg.HTTPMaxHeaderBytes != 32768 {
		t.Fatalf("unexpected HTTP limits: header=%s idle=%s bytes=%d", cfg.ReadHeaderTimeout, cfg.IdleTimeout, cfg.HTTPMaxHeaderBytes)
	}
	if cfg.UpstreamSyncLimit != 42 {
		t.Fatalf("unexpected UPSTREAM_SYNC_LIMIT: %d", cfg.UpstreamSyncLimit)
	}
	if cfg.UpstreamSyncConcurrency != 6 {
		t.Fatalf("unexpected UPSTREAM_SYNC_CONCURRENCY: %d", cfg.UpstreamSyncConcurrency)
	}
	if cfg.ModuleUploadMaxBytes != 2048 {
		t.Fatalf("unexpected MODULE_UPLOAD_MAX_BYTES: %d", cfg.ModuleUploadMaxBytes)
	}
	if cfg.UpstreamArtifactMaxBytes != 4096 {
		t.Fatalf("unexpected UPSTREAM_ARTIFACT_MAX_BYTES: %d", cfg.UpstreamArtifactMaxBytes)
	}
	if cfg.UpstreamProxyJSONStaleTTL != 30*time.Minute {
		t.Fatalf("unexpected UPSTREAM_PROXY_JSON_STALE_TTL: %s", cfg.UpstreamProxyJSONStaleTTL)
	}
	if cfg.MetricsModuleLimit != 123 {
		t.Fatalf("unexpected METRICS_MODULE_LIMIT: %d", cfg.MetricsModuleLimit)
	}
	if cfg.MetricsRefreshInterval != 45*time.Second {
		t.Fatalf("unexpected METRICS_REFRESH_INTERVAL: %s", cfg.MetricsRefreshInterval)
	}
}

func TestLoadRejectsInvalidS3Configuration(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		accessKey string
		secretKey string
		want      string
	}{
		{name: "relative endpoint", endpoint: "minio:9000", want: "absolute HTTP(S) URL"},
		{name: "unsupported endpoint scheme", endpoint: "ftp://minio:9000", want: "absolute HTTP(S) URL"},
		{name: "access key only", endpoint: "http://minio:9000", accessKey: "key", want: "must be set together"},
		{name: "secret key only", endpoint: "http://minio:9000", secretKey: "secret", want: "must be set together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
			t.Setenv("ARTIFACT_BACKEND", "s3")
			t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
			t.Setenv("ARTIFACT_ENDPOINT", tt.endpoint)
			t.Setenv("ARTIFACT_ACCESS_KEY_ID", tt.accessKey)
			t.Setenv("ARTIFACT_SECRET_ACCESS_KEY", tt.secretKey)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadRequiresGCSProjectOnlyForBucketProvisioning(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() without GCS project error = %v", err)
	}
	if cfg.ArtifactProject != "" || cfg.ArtifactEnsureBucket {
		t.Fatalf("unexpected runtime-only GCS config: %#v", cfg)
	}

	t.Setenv("ARTIFACT_ENSURE_BUCKET", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ARTIFACT_PROJECT") {
		t.Fatalf("Load() provisioning error = %v, want ARTIFACT_PROJECT requirement", err)
	}
}

func TestLoadArgsOverridesEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///env.db")
	t.Setenv("ARTIFACT_BUCKET", "env-bucket")
	t.Setenv("ARTIFACT_PROJECT", "env-project")
	t.Setenv("READ_TIMEOUT", "15s")
	t.Setenv("UPSTREAM_SYNC_LIMIT", "42")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := LoadArgs([]string{
		"--database-dsn", "sqlite:///flag.db",
		"--log-level", "DEBUG",
		"--artifact-bucket", "flag-bucket",
		"--artifact-project", "flag-project",
		"--read-timeout", "30s",
		"--read-header-timeout", "5s",
		"--idle-timeout", "90s",
		"--http-max-header-bytes", "65536",
		"--upstream-sync-limit", "7",
		"--upstream-sync-concurrency", "3",
		"--module-upload-max-bytes", "8192",
		"--upstream-artifact-max-bytes", "16384",
		"--upstream-proxy-json-stale-ttl", "10m",
		"--metrics-module-limit", "321",
		"--manage-session-secret", "flag-manage-session-secret-32-bytes",
		"--access-token-pepper", "flag-access-token-pepper-32-bytes",
		"--access-token-history-ttl", "720h",
		"--deleted-release-ttl", "1440h",
		"--trusted-proxy-cidrs", "192.0.2.0/24",
		"--trust-forwarded-headers=true",
		"--allowed-public-hosts", "forge.example.com forge-alt.example.com",
		"--oidc-scopes", "profile email groups",
		"--oidc-signing-algorithms", "RS256 ES256",
	})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}

	if cfg.DatabaseDSN != "sqlite:///flag.db" {
		t.Fatalf("unexpected DATABASE_DSN: %q", cfg.DatabaseDSN)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("unexpected LOG_LEVEL: %q", cfg.LogLevel)
	}
	if cfg.ArtifactBucket != "flag-bucket" {
		t.Fatalf("unexpected ARTIFACT_BUCKET: %q", cfg.ArtifactBucket)
	}
	if cfg.ArtifactProject != "flag-project" {
		t.Fatalf("unexpected ARTIFACT_PROJECT: %q", cfg.ArtifactProject)
	}
	if cfg.ReadTimeout != 30*time.Second {
		t.Fatalf("unexpected READ_TIMEOUT: %s", cfg.ReadTimeout)
	}
	if cfg.ReadHeaderTimeout != 5*time.Second || cfg.IdleTimeout != 90*time.Second || cfg.HTTPMaxHeaderBytes != 65536 {
		t.Fatalf("unexpected HTTP flag limits: header=%s idle=%s bytes=%d", cfg.ReadHeaderTimeout, cfg.IdleTimeout, cfg.HTTPMaxHeaderBytes)
	}
	if cfg.UpstreamSyncLimit != 7 {
		t.Fatalf("unexpected UPSTREAM_SYNC_LIMIT: %d", cfg.UpstreamSyncLimit)
	}
	if cfg.UpstreamSyncConcurrency != 3 {
		t.Fatalf("unexpected UPSTREAM_SYNC_CONCURRENCY: %d", cfg.UpstreamSyncConcurrency)
	}
	if cfg.ModuleUploadMaxBytes != 8192 {
		t.Fatalf("unexpected MODULE_UPLOAD_MAX_BYTES: %d", cfg.ModuleUploadMaxBytes)
	}
	if cfg.UpstreamArtifactMaxBytes != 16384 {
		t.Fatalf("unexpected UPSTREAM_ARTIFACT_MAX_BYTES: %d", cfg.UpstreamArtifactMaxBytes)
	}
	if cfg.UpstreamProxyJSONStaleTTL != 10*time.Minute {
		t.Fatalf("unexpected UPSTREAM_PROXY_JSON_STALE_TTL: %s", cfg.UpstreamProxyJSONStaleTTL)
	}
	if cfg.ManageSessionSecret != "flag-manage-session-secret-32-bytes" {
		t.Fatalf("unexpected MANAGE_SESSION_SECRET: %q", cfg.ManageSessionSecret)
	}
	if cfg.AccessTokenPepper != "flag-access-token-pepper-32-bytes" {
		t.Fatalf("unexpected ACCESS_TOKEN_PEPPER: %q", cfg.AccessTokenPepper)
	}
	if cfg.AccessTokenHistoryTTL != 30*24*time.Hour {
		t.Fatalf("unexpected ACCESS_TOKEN_HISTORY_TTL: %s", cfg.AccessTokenHistoryTTL)
	}
	if cfg.DeletedReleaseTTL != 60*24*time.Hour {
		t.Fatalf("unexpected DELETED_RELEASE_TTL: %s", cfg.DeletedReleaseTTL)
	}
	if cfg.MetricsModuleLimit != 321 {
		t.Fatalf("unexpected METRICS_MODULE_LIMIT: %d", cfg.MetricsModuleLimit)
	}
	if cfg.TrustedProxyCIDRs != "192.0.2.0/24" {
		t.Fatalf("unexpected TRUSTED_PROXY_CIDRS: %q", cfg.TrustedProxyCIDRs)
	}
	if !cfg.TrustForwardedHeaders {
		t.Fatal("expected TRUST_FORWARDED_HEADERS flag to be true")
	}
	if cfg.AllowedPublicHosts != "forge.example.com forge-alt.example.com" {
		t.Fatalf("unexpected ALLOWED_PUBLIC_HOSTS: %q", cfg.AllowedPublicHosts)
	}
	if cfg.OIDCScopes != "profile email groups" {
		t.Fatalf("unexpected OIDC_SCOPES: %q", cfg.OIDCScopes)
	}
	if cfg.OIDCSigningAlgorithms != "RS256 ES256" {
		t.Fatalf("unexpected OIDC_SIGNING_ALGORITHMS: %q", cfg.OIDCSigningAlgorithms)
	}
}

func TestLoadRejectsInvalidSizeLimits(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "json-cache",
			args: []string{"--forge-cache-max-body-bytes", "0"},
			want: "FORGE_CACHE_MAX_BODY_BYTES",
		},
		{
			name: "module-upload",
			args: []string{"--module-upload-max-bytes", "0"},
			want: "MODULE_UPLOAD_MAX_BYTES",
		},
		{
			name: "upstream-artifact",
			args: []string{"--upstream-artifact-max-bytes", "0"},
			want: "UPSTREAM_ARTIFACT_MAX_BYTES",
		},
		{
			name: "metrics-module-limit",
			args: []string{"--metrics-module-limit", "0"},
			want: "METRICS_MODULE_LIMIT",
		},
		{
			name: "upstream-sync-concurrency",
			args: []string{"--upstream-sync-concurrency", "0"},
			want: "UPSTREAM_SYNC_CONCURRENCY",
		},
		{
			name: "upstream-sync-limit",
			args: []string{"--upstream-sync-limit", "0"},
			want: "UPSTREAM_SYNC_LIMIT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadArgs(tc.args)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadRejectsNegativeUpstreamDurations(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	for _, tc := range []struct {
		name string
		flag string
		want string
	}{
		{name: "JSON cache", flag: "--upstream-proxy-json-cache-ttl", want: "UPSTREAM_PROXY_JSON_CACHE_TTL"},
		{name: "JSON stale", flag: "--upstream-proxy-json-stale-ttl", want: "UPSTREAM_PROXY_JSON_STALE_TTL"},
		{name: "sync interval", flag: "--upstream-sync-interval", want: "UPSTREAM_SYNC_INTERVAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadArgs([]string{tc.flag, "-1s"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadArgs() error = %v, want %s validation", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	_, err := LoadArgs([]string{"--log-level", "verbose"})
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("LoadArgs() error = %v, want LOG_LEVEL validation error", err)
	}
}

func TestLoadReturnsTypedEnvironmentErrors(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "duration", key: "READ_HEADER_TIMEOUT", value: "eventually"},
		{name: "int", key: "HTTP_MAX_HEADER_BYTES", value: "large"},
		{name: "int64", key: "MODULE_UPLOAD_MAX_BYTES", value: "huge"},
		{name: "boolean", key: "PUBLIC_MODULE_ACCESS", value: "sometimes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("Load() error = %v, want parameter name and invalid value", err)
			}
		})
	}
}

func TestLoadParsesBooleanEnvironmentValues(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("PUBLIC_MODULE_ACCESS", "TRUE")
	t.Setenv("ARTIFACT_PATH_STYLE", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.PublicModuleAccess {
		t.Fatal("PUBLIC_MODULE_ACCESS=TRUE was not parsed as true")
	}
	if cfg.ArtifactPathStyle {
		t.Fatal("ARTIFACT_PATH_STYLE=0 was not parsed as false")
	}
}

func TestLoadValidatesPostgresPoolConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		match string
	}{
		{name: "zero max", args: []string{"--database-max-conns=0"}, match: "DATABASE_MAX_CONNS"},
		{name: "negative min", args: []string{"--database-min-conns=-1"}, match: "DATABASE_MIN_CONNS"},
		{name: "min above max", args: []string{"--database-max-conns=2", "--database-min-conns=3"}, match: "DATABASE_MIN_CONNS"},
		{name: "negative lifetime", args: []string{"--database-max-conn-lifetime=-1s"}, match: "DATABASE_MAX_CONN_LIFETIME"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
			t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
			t.Setenv("ARTIFACT_PROJECT", "local-dev")

			_, err := LoadArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("LoadArgs() error = %v, want %s validation error", err, tc.match)
			}
		})
	}
}

func TestLoadArgsBoolFlagCanDisableEnvBool(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("PUBLIC_MODULE_ACCESS", "true")

	cfg, err := LoadArgs([]string{"--public-module-access=false"})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}
	if cfg.PublicModuleAccess {
		t.Fatal("expected --public-module-access=false to override env")
	}
}

func TestLoadReadsSecurityHSTS(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("SECURITY_HSTS_ENABLED", "true")

	cfg, err := LoadArgs([]string{"--security-hsts-enabled=false"})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}
	if cfg.SecurityHSTSEnabled {
		t.Fatal("expected --security-hsts-enabled=false to override env")
	}
}

func TestLoadArgsVersionSkipsRuntimeValidation(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := LoadArgs([]string{"--version"})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}
	if !cfg.Version {
		t.Fatal("expected --version to set Version")
	}
}

func TestLoadArgsReconciliationFlags(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	cfg, err := LoadArgs([]string{"--reconcile-artifacts", "--reconcile-repair"})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}
	if !cfg.ReconcileArtifacts || !cfg.ReconcileRepair {
		t.Fatalf("unexpected reconciliation flags: %#v", cfg)
	}
}

func TestLoadArgsRejectsRepairWithoutReconciliation(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	if _, err := LoadArgs([]string{"--reconcile-repair"}); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("LoadArgs() error = %v, want reconciliation requirement", err)
	}
}

func TestLoadArgsRejectsUnknownFlag(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	_, err := LoadArgs([]string{"--does-not-exist"})
	if err == nil {
		t.Fatal("expected unknown flag error")
	}
	if !strings.Contains(err.Error(), "parse flags") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsDatabaseBackendMismatch(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_BACKEND", "postgres")
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_DSN") {
		t.Fatalf("Load() error = %v, want database backend mismatch", err)
	}
}

func TestLoadRejectsNonPositiveOperationalDurations(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "shutdown", key: "SHUTDOWN_TIMEOUT"},
		{name: "active release", key: "ACTIVE_RELEASE_TTL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
			t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
			t.Setenv("ARTIFACT_PROJECT", "local-dev")
			t.Setenv(tc.key, "0s")

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Load() error = %v, want %s validation", err, tc.key)
			}
		})
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"APP_ENV",
		"LOG_LEVEL",
		"HTTP_ADDR",
		"METRICS_ADDR",
		"READ_HEADER_TIMEOUT",
		"READ_TIMEOUT",
		"WRITE_TIMEOUT",
		"IDLE_TIMEOUT",
		"HTTP_MAX_HEADER_BYTES",
		"SHUTDOWN_TIMEOUT",
		"DATABASE_BACKEND",
		"DATABASE_DSN",
		"DATABASE_MAX_CONNS",
		"DATABASE_MIN_CONNS",
		"DATABASE_MAX_CONN_LIFETIME",
		"DATABASE_MAX_CONN_IDLE_TIME",
		"ADMIN_TOKEN",
		"ACCESS_TOKEN_PEPPER",
		"ACCESS_TOKEN_HISTORY_TTL",
		"DELETED_RELEASE_TTL",
		"PUBLIC_MODULE_ACCESS",
		"ACTIVE_RELEASE_TTL",
		"ARTIFACT_BACKEND",
		"ARTIFACT_ENDPOINT",
		"ARTIFACT_BUCKET",
		"ARTIFACT_PROJECT",
		"ARTIFACT_ENSURE_BUCKET",
		"ARTIFACT_GCS_ANONYMOUS",
		"ARTIFACT_PREFIX",
		"ARTIFACT_REGION",
		"ARTIFACT_ACCESS_KEY_ID",
		"ARTIFACT_SECRET_ACCESS_KEY",
		"ARTIFACT_PATH_STYLE",
		"PUBLIC_BASE_URL",
		"ALLOWED_PUBLIC_HOSTS",
		"TRUSTED_PROXY_CIDRS",
		"TRUST_FORWARDED_HEADERS",
		"SECURITY_HSTS_ENABLED",
		"WEB_AUTH_MODE",
		"OIDC_ISSUER_URL",
		"OIDC_CLIENT_ID",
		"OIDC_CLIENT_SECRET",
		"OIDC_REDIRECT_URL",
		"OIDC_LOGOUT_URL",
		"OIDC_COOKIE_SECRET",
		"OIDC_SCOPES",
		"OIDC_SIGNING_ALGORITHMS",
		"UPSTREAM_URL",
		"UPSTREAM_PROXY_JSON_CACHE_TTL",
		"UPSTREAM_PROXY_JSON_STALE_TTL",
		"FORGE_CACHE_MAX_BODY_BYTES",
		"MODULE_UPLOAD_MAX_BYTES",
		"UPSTREAM_ARTIFACT_MAX_BYTES",
		"UPSTREAM_ARTIFACT_ORPHAN_TTL",
		"UPSTREAM_SYNC_INTERVAL",
		"UPSTREAM_SYNC_LIMIT",
		"UPSTREAM_SYNC_CONCURRENCY",
		"METRICS_MODULE_LIMIT",
		"METRICS_REFRESH_INTERVAL",
		"RECONCILE_ARTIFACTS",
		"RECONCILE_REPAIR",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("ACCESS_TOKEN_PEPPER", strings.Repeat("test-pepper-", 3))
	t.Setenv("MANAGE_SESSION_SECRET", "test-manage-session-secret-32-bytes")
}
