package metrics

import (
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	publishTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_publish_total",
		Help: "Total number of module publish attempts.",
	}, []string{"result", "owner"}))

	deleteTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_delete_total",
		Help: "Total number of module and release delete attempts.",
	}, []string{"result", "kind", "owner"}))

	releaseUsageMarkedTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_release_usage_mark_total",
		Help: "Total number of release usage mark attempts.",
	}, []string{"result", "owner"}))

	upstreamSyncTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_upstream_sync_total",
		Help: "Total number of upstream module sync attempts.",
	}, []string{"result", "trigger"}))

	upstreamRefreshCyclesTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_upstream_refresh_cycles_total",
		Help: "Total number of upstream refresh cycles.",
	}, []string{"result"}))

	upstreamRefreshDuration = RegisterHistogram(prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "puppet_forge_upstream_refresh_duration_seconds",
		Help:    "Duration of upstream refresh cycles in seconds.",
		Buckets: prometheus.DefBuckets,
	}))

	upstreamRefreshLastDuration = RegisterGauge(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "puppet_forge_upstream_refresh_last_duration_seconds",
		Help: "Duration of the most recent upstream refresh cycle in seconds.",
	}))

	upstreamRefreshLastSuccessTimestamp = RegisterGauge(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "puppet_forge_upstream_refresh_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful upstream refresh cycle.",
	}))

	upstreamRefreshLastErrorTimestamp = RegisterGauge(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "puppet_forge_upstream_refresh_last_error_timestamp_seconds",
		Help: "Unix timestamp of the last upstream refresh cycle that had at least one error.",
	}))

	upstreamRefreshModules = RegisterGaugeVec(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "puppet_forge_upstream_refresh_modules",
		Help: "Number of modules observed in the last upstream refresh cycle.",
	}, []string{"result"}))

	upstreamCacheRequestsTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "puppet_forge_upstream_cache_requests_total",
		Help: "Total number of upstream cache decisions.",
	}, []string{"kind", "result"}))

	buildInfo = RegisterGaugeVec(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "puppet_forge_build_info",
		Help: "Build information about puppet-forge.",
	}, []string{"version", "go_version"}))
)

func init() {
	initOperationMetrics()
}

func initOperationMetrics() {
	for _, trigger := range []string{"single", "refresh"} {
		for _, result := range []string{"success", "error"} {
			upstreamSyncTotal.WithLabelValues(result, trigger).Add(0)
		}
	}
	for _, result := range []string{"attempted", "success", "error"} {
		upstreamRefreshModules.WithLabelValues(result).Set(0)
	}
	for _, result := range []string{"success", "error"} {
		upstreamRefreshCyclesTotal.WithLabelValues(result).Add(0)
	}
	for _, kind := range []string{"json", "artifact"} {
		for _, result := range []string{"hit", "miss", "stale", "bypass"} {
			upstreamCacheRequestsTotal.WithLabelValues(kind, result).Add(0)
		}
	}
}

func ObservePublish(owner string, err error) {
	publishTotal.WithLabelValues(resultLabel(err), owner).Inc()
}

func ObserveDelete(kind, owner string, err error) {
	deleteTotal.WithLabelValues(resultLabel(err), kind, owner).Inc()
}

func ObserveReleaseUsageMark(owner string, err error) {
	releaseUsageMarkedTotal.WithLabelValues(resultLabel(err), owner).Inc()
}

func ObserveUpstreamSync(trigger string, err error) {
	upstreamSyncTotal.WithLabelValues(resultLabel(err), trigger).Inc()
}

func ObserveUpstreamRefresh(start time.Time, attempted, succeeded, failed int) {
	duration := time.Since(start).Seconds()
	upstreamRefreshDuration.Observe(duration)
	upstreamRefreshLastDuration.Set(duration)
	upstreamRefreshModules.WithLabelValues("attempted").Set(float64(attempted))
	upstreamRefreshModules.WithLabelValues("success").Set(float64(succeeded))
	upstreamRefreshModules.WithLabelValues("error").Set(float64(failed))
	now := float64(time.Now().Unix())
	if failed == 0 {
		upstreamRefreshCyclesTotal.WithLabelValues("success").Inc()
		upstreamRefreshLastSuccessTimestamp.Set(now)
		return
	}
	upstreamRefreshCyclesTotal.WithLabelValues("error").Inc()
	upstreamRefreshLastErrorTimestamp.Set(now)
}

func ObserveUpstreamCache(kind, result string) {
	upstreamCacheRequestsTotal.WithLabelValues(kind, result).Inc()
}

func RecordBuildInfo(version, goVersion string) {
	buildInfo.Reset()
	buildInfo.WithLabelValues(version, goVersion).Set(1)
}

func resultLabel(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func RegisterCounterVec(collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := prometheus.Register(collector); err != nil {
		if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		slog.Default().Error("register counter collector failed", "err", err)
	}
	return collector
}

func RegisterGauge(collector prometheus.Gauge) prometheus.Gauge {
	if err := prometheus.Register(collector); err != nil {
		if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(prometheus.Gauge); ok {
				return existing
			}
		}
		slog.Default().Error("register gauge collector failed", "err", err)
	}
	return collector
}

func RegisterGaugeVec(collector *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := prometheus.Register(collector); err != nil {
		if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing
			}
		}
		slog.Default().Error("register gauge collector failed", "err", err)
	}
	return collector
}

func RegisterHistogram(collector prometheus.Histogram) prometheus.Histogram {
	if err := prometheus.Register(collector); err != nil {
		if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(prometheus.Histogram); ok {
				return existing
			}
		}
		slog.Default().Error("register histogram collector failed", "err", err)
	}
	return collector
}

func RegisterHistogramVec(collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := prometheus.Register(collector); err != nil {
		if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
		slog.Default().Error("register histogram collector failed", "err", err)
	}
	return collector
}
