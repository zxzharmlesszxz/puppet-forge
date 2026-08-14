package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/zxzharmlesszxz/puppet-forge/internal/app"
	"github.com/zxzharmlesszxz/puppet-forge/internal/config"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
)

var version = "dev"
var logLevel = new(slog.LevelVar)

func main() {
	logger := newLogger(os.Stdout, logLevel)
	slog.SetDefault(logger)
	if err := run(); err != nil {
		logger.Error("puppet-forge stopped", "err", err)
		os.Exit(1)
	}
}

func newLogger(output io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level}))
}

func run() (runErr error) {
	cfg, err := config.LoadCommandLine(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Version {
		_, _ = os.Stdout.WriteString(version + "\n")
		return nil
	}
	logLevel.Set(configuredLogLevel(cfg.LogLevel))
	slog.Debug("debug logging enabled", "log_level", cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics.RecordBuildInfo(version, runtime.Version())
	if cfg.ReconcileArtifacts {
		report, err := app.ReconcileArtifacts(ctx, cfg, cfg.ReconcileRepair)
		if err != nil {
			return fmt.Errorf("reconcile artifacts: %w", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return fmt.Errorf("encode reconciliation report: %w", err)
		}
		unresolved := len(report.MissingObjects) + len(report.CorruptObjects) + len(report.OrphanObjects) - len(report.DeletedOrphans)
		if unresolved > 0 {
			return errors.New("artifact reconciliation found unresolved inconsistencies")
		}
		return nil
	}

	application, err := app.NewContext(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, application.Close()) }()

	apiServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Router(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.HTTPMaxHeaderBytes,
	}
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           application.MetricsHandler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.HTTPMaxHeaderBytes,
	}

	slog.Info("starting puppet-forge API",
		"app_env", cfg.AppEnv,
		"log_level", cfg.LogLevel,
		"http_addr", cfg.HTTPAddr,
		"metrics_addr", cfg.MetricsAddr,
		"read_header_timeout", cfg.ReadHeaderTimeout,
		"read_timeout", cfg.ReadTimeout,
		"write_timeout", cfg.WriteTimeout,
		"idle_timeout", cfg.IdleTimeout,
		"http_max_header_bytes", cfg.HTTPMaxHeaderBytes,
		"shutdown_timeout", cfg.ShutdownTimeout,
		"database_max_conns", cfg.DatabaseMaxConns,
		"database_min_conns", cfg.DatabaseMinConns,
		"database_max_conn_lifetime", cfg.DatabaseMaxConnLifetime,
		"database_max_conn_idle_time", cfg.DatabaseMaxConnIdleTime,
		"artifact_backend", cfg.ArtifactBackend,
		"artifact_endpoint", cfg.ArtifactEndpoint,
		"artifact_bucket", cfg.ArtifactBucket,
		"artifact_prefix", cfg.ArtifactPrefix,
		"public_base_url", cfg.PublicBaseURL,
		"trusted_proxy_cidrs", cfg.TrustedProxyCIDRs,
		"public_module_access", cfg.PublicModuleAccess,
		"active_release_ttl", cfg.ActiveReleaseTTL,
		"access_token_history_ttl", cfg.AccessTokenHistoryTTL,
		"deleted_release_ttl", cfg.DeletedReleaseTTL,
		"security_hsts_enabled", cfg.SecurityHSTSEnabled,
		"web_auth_mode", cfg.WebAuthMode,
		"oidc_redirect_url", cfg.OIDCRedirectURL,
		"oidc_scopes", cfg.OIDCScopes,
		"upstream_url", cfg.UpstreamURL,
		"upstream_proxy_json_cache_ttl", cfg.UpstreamProxyJSONCacheTTL,
		"upstream_proxy_json_stale_ttl", cfg.UpstreamProxyJSONStaleTTL,
		"upstream_artifact_orphan_ttl", cfg.UpstreamArtifactOrphanTTL,
		"upstream_sync_interval", cfg.UpstreamSyncInterval,
		"upstream_sync_limit", cfg.UpstreamSyncLimit,
		"upstream_sync_concurrency", cfg.UpstreamSyncConcurrency,
		"metrics_module_limit", cfg.MetricsModuleLimit,
		"metrics_refresh_interval", cfg.MetricsRefreshInterval,
	)

	serverErrors := make(chan error, 2)
	go func() { serverErrors <- apiServer.ListenAndServe() }()
	go func() { serverErrors <- metricsServer.ListenAndServe() }()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverErrors:
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := shutdownHTTPServers(shutdownCtx, apiServer, metricsServer); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}

func shutdownHTTPServers(ctx context.Context, servers ...*http.Server) error {
	errorsByServer := make([]error, len(servers))
	var wg sync.WaitGroup
	for index, server := range servers {
		if server == nil {
			continue
		}
		wg.Go(func() { errorsByServer[index] = server.Shutdown(ctx) })
	}
	wg.Wait()
	return errors.Join(errorsByServer...)
}

func configuredLogLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
