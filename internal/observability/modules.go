package observability

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type moduleMetricsSource interface {
	CountModulesByOwner(ctx context.Context, owners []string) (map[string]int, error)
	ListReleaseMetricSummaries(ctx context.Context) ([]domain.ReleaseMetricSummary, error)
}

const (
	metricsCollectTimeout = 30 * time.Second
	metricsJitterPercent  = 10
)

type moduleMetricsCollector struct {
	modules     moduleMetricsSource
	moduleLimit int

	modulesByOwnerDesc       *prometheus.Desc
	releaseSummaryDesc       *prometheus.Desc
	releaseLatestSummaryDesc *prometheus.Desc
	readyDesc                *prometheus.Desc
	lastSuccessDesc          *prometheus.Desc
	refreshErrorsDesc        *prometheus.Desc
	ownersTotalDesc          *prometheus.Desc
	truncatedDesc            *prometheus.Desc

	mu            sync.RWMutex
	cached        []prometheus.Metric
	ready         bool
	lastSuccess   time.Time
	refreshErrors uint64
	ownersTotal   int
	truncated     bool
}

func RegisterModuleMetrics(ctx context.Context, modules moduleMetricsSource, moduleLimit int, refreshInterval time.Duration, registerer prometheus.Registerer) (func(), error) {
	if modules == nil {
		return func() {}, nil
	}
	if moduleLimit <= 0 {
		moduleLimit = 10000
	}
	if refreshInterval <= 0 {
		return nil, errors.New("module metrics refresh interval must be greater than zero")
	}
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	collector := newModuleMetricsCollector(modules, moduleLimit)
	if err := registerer.Register(collector); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	wg.Go(func() { collector.refreshLoop(ctx, refreshInterval) })
	return wg.Wait, nil
}

func newModuleMetricsCollector(modules moduleMetricsSource, moduleLimit int) *moduleMetricsCollector {
	return &moduleMetricsCollector{
		modules:     modules,
		moduleLimit: moduleLimit,
		modulesByOwnerDesc: prometheus.NewDesc(
			"puppet_forge_modules",
			"Known Puppet modules indexed by the service, grouped by owner.",
			[]string{"owner"},
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
		readyDesc: prometheus.NewDesc(
			"puppet_forge_module_metrics_ready",
			"Whether the module inventory metrics cache has completed at least one successful refresh.",
			nil,
			nil,
		),
		lastSuccessDesc: prometheus.NewDesc(
			"puppet_forge_module_metrics_last_success_timestamp_seconds",
			"Unix timestamp of the last successful module inventory metrics refresh.",
			nil,
			nil,
		),
		refreshErrorsDesc: prometheus.NewDesc(
			"puppet_forge_module_metrics_refresh_errors_total",
			"Total number of failed module inventory metrics refreshes.",
			nil,
			nil,
		),
		ownersTotalDesc: prometheus.NewDesc(
			"puppet_forge_module_metrics_owners_total",
			"Total number of module owners found during the last successful inventory refresh.",
			nil,
			nil,
		),
		truncatedDesc: prometheus.NewDesc(
			"puppet_forge_module_metrics_truncated",
			"Whether owner-labelled module inventory metrics were truncated by METRICS_MODULE_LIMIT.",
			nil,
			nil,
		),
	}
}

func (c *moduleMetricsCollector) refreshLoop(ctx context.Context, interval time.Duration) {
	c.refresh(ctx)
	for {
		timer := time.NewTimer(jitteredMetricsInterval(interval))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			c.refresh(ctx)
		}
	}
}

func jitteredMetricsInterval(interval time.Duration) time.Duration {
	return jitteredMetricsIntervalFrom(interval, cryptorand.Reader)
}

func jitteredMetricsIntervalFrom(interval time.Duration, random io.Reader) time.Duration {
	window := interval/100*metricsJitterPercent + interval%100*metricsJitterPercent/100
	if window <= 0 {
		return interval
	}
	offset, err := cryptorand.Int(random, big.NewInt(int64(2*window)+1))
	if err != nil {
		return interval
	}
	return interval - window + time.Duration(offset.Int64())
}

func (c *moduleMetricsCollector) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, metricsCollectTimeout)
	defer cancel()

	modulesByOwner, err := c.modules.CountModulesByOwner(ctx, nil)
	if err != nil {
		c.recordRefreshError()
		slog.Default().Error("collect module metrics count modules by owner failed", "err", err)
		return
	}

	var metrics []prometheus.Metric
	owners := make([]string, 0, len(modulesByOwner))
	for owner := range modulesByOwner {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	ownersTotal := len(owners)
	truncated := ownersTotal > c.moduleLimit
	owners = owners[:min(len(owners), c.moduleLimit)]
	for _, owner := range owners {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.modulesByOwnerDesc,
			prometheus.GaugeValue,
			float64(modulesByOwner[owner]),
			owner,
		))
	}

	summaries, err := c.modules.ListReleaseMetricSummaries(ctx)
	if err != nil {
		c.recordRefreshError()
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
	c.ready = true
	c.lastSuccess = time.Now()
	c.ownersTotal = ownersTotal
	c.truncated = truncated
	c.mu.Unlock()
	if truncated {
		slog.Default().Warn("module owner metrics truncated", "owners", ownersTotal, "limit", c.moduleLimit)
	}
}

func (c *moduleMetricsCollector) recordRefreshError() {
	c.mu.Lock()
	c.refreshErrors++
	c.mu.Unlock()
}

func (c *moduleMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.modulesByOwnerDesc
	ch <- c.releaseSummaryDesc
	ch <- c.releaseLatestSummaryDesc
	ch <- c.readyDesc
	ch <- c.lastSuccessDesc
	ch <- c.refreshErrorsDesc
	ch <- c.ownersTotalDesc
	ch <- c.truncatedDesc
}

func (c *moduleMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	cached := append([]prometheus.Metric(nil), c.cached...)
	ready := c.ready
	lastSuccess := c.lastSuccess
	refreshErrors := c.refreshErrors
	ownersTotal := c.ownersTotal
	truncated := c.truncated
	c.mu.RUnlock()

	for _, m := range cached {
		ch <- m
	}
	ch <- prometheus.MustNewConstMetric(c.readyDesc, prometheus.GaugeValue, boolFloat(ready))
	ch <- prometheus.MustNewConstMetric(c.lastSuccessDesc, prometheus.GaugeValue, unixSeconds(lastSuccess))
	ch <- prometheus.MustNewConstMetric(c.refreshErrorsDesc, prometheus.CounterValue, float64(refreshErrors))
	ch <- prometheus.MustNewConstMetric(c.ownersTotalDesc, prometheus.GaugeValue, float64(ownersTotal))
	ch <- prometheus.MustNewConstMetric(c.truncatedDesc, prometheus.GaugeValue, boolFloat(truncated))
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func unixSeconds(value time.Time) float64 {
	if value.IsZero() {
		return 0
	}
	return float64(value.UnixNano()) / float64(time.Second)
}
