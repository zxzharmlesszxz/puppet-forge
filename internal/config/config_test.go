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
	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("unexpected HTTP_ADDR default: %s", cfg.HTTPAddr)
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
	if cfg.SecurityHSTSEnabled {
		t.Fatal("expected SECURITY_HSTS_ENABLED default to be false")
	}
	if cfg.ModuleUploadMaxBytes != 128<<20 {
		t.Fatalf("unexpected MODULE_UPLOAD_MAX_BYTES default: %d", cfg.ModuleUploadMaxBytes)
	}
	if cfg.UpstreamArtifactMaxBytes != 128<<20 {
		t.Fatalf("unexpected UPSTREAM_ARTIFACT_MAX_BYTES default: %d", cfg.UpstreamArtifactMaxBytes)
	}
	if cfg.MetricsModuleLimit != 10000 {
		t.Fatalf("unexpected METRICS_MODULE_LIMIT default: %d", cfg.MetricsModuleLimit)
	}
}

func TestLoadReadsAdminToken(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("ADMIN_TOKEN", "bootstrap-token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AdminToken != "bootstrap-token" {
		t.Fatalf("unexpected ADMIN_TOKEN: %q", cfg.AdminToken)
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
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com/application/o/forge/")
	t.Setenv("OIDC_CLIENT_ID", "forge")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_COOKIE_SECRET", "cookie-secret")
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
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///tmp/puppet-forge.db")
	t.Setenv("ARTIFACT_BUCKET", "forge-artifacts")
	t.Setenv("ARTIFACT_PROJECT", "local-dev")
	t.Setenv("WEB_AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER_URL", "https://auth.example.com/application/o/forge/")
	t.Setenv("OIDC_CLIENT_ID", "forge")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_COOKIE_SECRET", "cookie-secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://forge.example.com/auth/callback")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.OIDCRedirectURL != "https://forge.example.com/auth/callback" {
		t.Fatalf("unexpected OIDC redirect URL: %s", cfg.OIDCRedirectURL)
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
	t.Setenv("UPSTREAM_SYNC_LIMIT", "42")
	t.Setenv("MODULE_UPLOAD_MAX_BYTES", "2048")
	t.Setenv("UPSTREAM_ARTIFACT_MAX_BYTES", "4096")
	t.Setenv("UPSTREAM_PROXY_JSON_STALE_TTL", "30m")
	t.Setenv("METRICS_MODULE_LIMIT", "123")

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
	if cfg.UpstreamSyncLimit != 42 {
		t.Fatalf("unexpected UPSTREAM_SYNC_LIMIT: %d", cfg.UpstreamSyncLimit)
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
}

func TestLoadArgsOverridesEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_DSN", "sqlite:///env.db")
	t.Setenv("ARTIFACT_BUCKET", "env-bucket")
	t.Setenv("ARTIFACT_PROJECT", "env-project")
	t.Setenv("READ_TIMEOUT", "15s")
	t.Setenv("UPSTREAM_SYNC_LIMIT", "42")

	cfg, err := LoadArgs([]string{
		"--database-dsn", "sqlite:///flag.db",
		"--artifact-bucket", "flag-bucket",
		"--artifact-project", "flag-project",
		"--read-timeout", "30s",
		"--upstream-sync-limit", "7",
		"--module-upload-max-bytes", "8192",
		"--upstream-artifact-max-bytes", "16384",
		"--upstream-proxy-json-stale-ttl", "10m",
		"--metrics-module-limit", "321",
	})
	if err != nil {
		t.Fatalf("LoadArgs() error = %v", err)
	}

	if cfg.DatabaseDSN != "sqlite:///flag.db" {
		t.Fatalf("unexpected DATABASE_DSN: %q", cfg.DatabaseDSN)
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
	if cfg.UpstreamSyncLimit != 7 {
		t.Fatalf("unexpected UPSTREAM_SYNC_LIMIT: %d", cfg.UpstreamSyncLimit)
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
	if cfg.MetricsModuleLimit != 321 {
		t.Fatalf("unexpected METRICS_MODULE_LIMIT: %d", cfg.MetricsModuleLimit)
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

func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"APP_ENV",
		"HTTP_ADDR",
		"READ_TIMEOUT",
		"WRITE_TIMEOUT",
		"SHUTDOWN_TIMEOUT",
		"DATABASE_DSN",
		"ADMIN_TOKEN",
		"PUBLIC_MODULE_ACCESS",
		"ACTIVE_RELEASE_TTL",
		"ARTIFACT_BACKEND",
		"ARTIFACT_ENDPOINT",
		"ARTIFACT_BUCKET",
		"ARTIFACT_PROJECT",
		"ARTIFACT_PREFIX",
		"ARTIFACT_REGION",
		"ARTIFACT_ACCESS_KEY_ID",
		"ARTIFACT_SECRET_ACCESS_KEY",
		"ARTIFACT_PATH_STYLE",
		"PUBLIC_BASE_URL",
		"SECURITY_HSTS_ENABLED",
		"WEB_AUTH_MODE",
		"OIDC_ISSUER_URL",
		"OIDC_CLIENT_ID",
		"OIDC_CLIENT_SECRET",
		"OIDC_REDIRECT_URL",
		"OIDC_LOGOUT_URL",
		"OIDC_COOKIE_SECRET",
		"UPSTREAM_URL",
		"UPSTREAM_PROXY_JSON_CACHE_TTL",
		"UPSTREAM_PROXY_JSON_STALE_TTL",
		"FORGE_CACHE_MAX_BODY_BYTES",
		"MODULE_UPLOAD_MAX_BYTES",
		"UPSTREAM_ARTIFACT_MAX_BYTES",
		"UPSTREAM_SYNC_INTERVAL",
		"UPSTREAM_SYNC_LIMIT",
		"METRICS_MODULE_LIMIT",
	} {
		t.Setenv(key, "")
	}
}
