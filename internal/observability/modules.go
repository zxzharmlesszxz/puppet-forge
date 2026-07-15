package observability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"puppet-forge/internal/service"
)

const (
	metricsCacheInterval  = 30 * time.Second
	metricsCollectTimeout = 30 * time.Second
)

type moduleMetricsCollector struct {
	modules     *service.ModuleService
	moduleLimit int

	moduleInfoDesc           *prometheus.Desc
	releaseSummaryDesc       *prometheus.Desc
	releaseLatestSummaryDesc *prometheus.Desc

	mu        sync.RWMutex
	cached    []prometheus.Metric
	lastFetch time.Time
}

func RegisterModuleMetrics(ctx context.Context, modules *service.ModuleService, moduleLimit int) {
	if modules == nil {
		return
	}
	if moduleLimit <= 0 {
		moduleLimit = 10000
	}

	collector := newModuleMetricsCollector(modules, moduleLimit)
	if err := prometheus.Register(collector); err != nil {
		if _, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); !ok {
			slog.Default().Error("register module metrics collector failed", "err", err)
		}
	}

	go collector.refreshLoop(ctx)
}

func newModuleMetricsCollector(modules *service.ModuleService, moduleLimit int) *moduleMetricsCollector {
	return &moduleMetricsCollector{
		modules:     modules,
		moduleLimit: moduleLimit,
		moduleInfoDesc: prometheus.NewDesc(
			"puppet_forge_module_info",
			"Known Puppet modules indexed by the service.",
			[]string{"module", "owner", "name", "latest_version"},
			nil,
		),
		releaseSummaryDesc: prometheus.NewDesc(
			"puppet_forge_module_releases",
			"Known Puppet module releases indexed by the service, grouped by source.",
			[]string{"source"},
			nil,
		),
		releaseLatestSummaryDesc: prometheus.NewDesc(
			"puppet_forge_module_latest_releases",
			"Known latest Puppet module releases indexed by the service, grouped by source.",
			[]string{"source"},
			nil,
		),
	}
}

func (c *moduleMetricsCollector) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(metricsCacheInterval)
	defer ticker.Stop()

	c.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *moduleMetricsCollector) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, metricsCollectTimeout)
	defer cancel()

	modules, err := c.modules.ListModules(ctx, c.moduleLimit)
	if err != nil {
		slog.Default().Error("collect module metrics list modules failed", "err", err)
		return
	}

	var metrics []prometheus.Metric

	for _, module := range modules {
		moduleLabel := module.Owner + "-" + module.Name
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.moduleInfoDesc,
			prometheus.GaugeValue,
			1,
			moduleLabel,
			module.Owner,
			module.Name,
			module.LatestVersion,
		))
	}

	summaries, err := c.modules.ListReleaseMetricSummaries(ctx)
	if err != nil {
		slog.Default().Error("collect release metric summaries failed", "err", err)
		return
	}
	for _, summary := range summaries {
		source := summary.Source
		if source == "" {
			source = "unknown"
		}
		metrics = append(metrics,
			prometheus.MustNewConstMetric(
				c.releaseSummaryDesc,
				prometheus.GaugeValue,
				float64(summary.Releases),
				source,
			),
			prometheus.MustNewConstMetric(
				c.releaseLatestSummaryDesc,
				prometheus.GaugeValue,
				float64(summary.LatestReleases),
				source,
			),
		)
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastFetch = time.Now()
	c.mu.Unlock()
}

func (c *moduleMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.moduleInfoDesc
	ch <- c.releaseSummaryDesc
	ch <- c.releaseLatestSummaryDesc
}

func (c *moduleMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	cached := c.cached
	c.mu.RUnlock()

	if cached == nil {
		c.refresh(context.Background())
		c.mu.RLock()
		cached = c.cached
		c.mu.RUnlock()
	}

	for _, m := range cached {
		ch <- m
	}
}
