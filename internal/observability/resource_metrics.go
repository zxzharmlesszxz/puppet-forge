package observability

import (
	"context"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (c *moduleMetricsCollector) collectResourceMetrics(ctx context.Context) ([]prometheus.Metric, error) {
	source, ok := c.modules.(store.ResourceMetricStore)
	if !ok {
		return nil, nil
	}
	summaries, err := source.ListResourceMetricSummaries(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[[3]string]int)
	pending := make(map[[2]string]float64)
	for _, summary := range summaries {
		kind := boundedResourceLabel(summary.Kind, "module", "release")
		source := boundedResourceLabel(summary.Source, "local", "upstream")
		state := boundedResourceLabel(string(summary.State), "available", "pending", "deprecated", "blocked")
		counts[[3]string{kind, source, state}] += summary.Count
		if state == "pending" && !summary.OldestPendingAt.IsZero() {
			key := [2]string{kind, source}
			value := unixSeconds(summary.OldestPendingAt)
			if pending[key] == 0 || value < pending[key] {
				pending[key] = value
			}
		}
	}
	var metrics []prometheus.Metric
	for _, kind := range []string{"module", "release", "unknown"} {
		for _, source := range []string{"local", "upstream", "unknown"} {
			for _, state := range []string{"available", "pending", "deprecated", "blocked", "unknown"} {
				metrics = append(metrics, prometheus.MustNewConstMetric(c.resourceStatesDesc, prometheus.GaugeValue, float64(counts[[3]string{kind, source, state}]), kind, source, state))
			}
			metrics = append(metrics, prometheus.MustNewConstMetric(c.oldestPendingDesc, prometheus.GaugeValue, pending[[2]string{kind, source}], kind, source))
		}
	}
	return metrics, nil
}

func boundedResourceLabel(value string, allowed ...string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return "unknown"
}
