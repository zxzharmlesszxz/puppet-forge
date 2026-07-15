package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"puppet-forge/internal/app"
	"puppet-forge/internal/config"
	"puppet-forge/internal/metrics"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.LoadCommandLine(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	if cfg.Version {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}

	metrics.RecordBuildInfo(version, runtime.Version())

	application, err := app.New(cfg)
	if err != nil {
		logger.Error("build app", "err", err)
		os.Exit(1)
	}
	defer application.Close()

	server := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      application.Router(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown", "err", err)
		}
	}()

	logger.Info("starting puppet-forge API",
		"app_env", cfg.AppEnv,
		"http_addr", cfg.HTTPAddr,
		"read_timeout", cfg.ReadTimeout,
		"write_timeout", cfg.WriteTimeout,
		"shutdown_timeout", cfg.ShutdownTimeout,
		"artifact_backend", cfg.ArtifactBackend,
		"artifact_endpoint", cfg.ArtifactEndpoint,
		"artifact_bucket", cfg.ArtifactBucket,
		"artifact_prefix", cfg.ArtifactPrefix,
		"public_base_url", cfg.PublicBaseURL,
		"public_module_access", cfg.PublicModuleAccess,
		"active_release_ttl", cfg.ActiveReleaseTTL,
		"security_hsts_enabled", cfg.SecurityHSTSEnabled,
		"web_auth_mode", cfg.WebAuthMode,
		"oidc_redirect_url", cfg.OIDCRedirectURL,
		"upstream_url", cfg.UpstreamURL,
		"upstream_proxy_json_cache_ttl", cfg.UpstreamProxyJSONCacheTTL,
		"upstream_proxy_json_stale_ttl", cfg.UpstreamProxyJSONStaleTTL,
		"upstream_sync_interval", cfg.UpstreamSyncInterval,
		"upstream_sync_limit", cfg.UpstreamSyncLimit,
		"metrics_module_limit", cfg.MetricsModuleLimit,
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}
