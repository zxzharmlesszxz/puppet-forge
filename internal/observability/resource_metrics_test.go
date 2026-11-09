package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type resourceMetricsFixture struct {
	staticModuleMetricsSource
	resources []domain.ResourceMetricSummary
	err       error
	calls     int
}

func (s *resourceMetricsFixture) ListResourceMetricSummaries(context.Context) ([]domain.ResourceMetricSummary, error) {
	s.calls++
	return s.resources, s.err
}

func TestResourceMetricsRefreshZerosAndRetainsLastGoodSnapshot(t *testing.T) {
	source := &resourceMetricsFixture{resources: []domain.ResourceMetricSummary{
		{Kind: "release", Source: "local", State: domain.StatePending, Count: 2, OldestPendingAt: time.Unix(1000, 0)},
		{Kind: "module", Source: "local", State: domain.StateBlocked, Count: 1},
		{Kind: "untrusted-kind", Source: "private-owner", State: "private-state", Count: 3},
	}}
	collector := newModuleMetricsCollector(source, 10, 10, time.Hour)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	read := func(name string, labels map[string]string) float64 {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() != name {
				continue
			}
			for _, metric := range family.Metric {
				matches := len(metric.Label) == len(labels)
				for _, label := range metric.Label {
					if labels[label.GetName()] != label.GetValue() {
						matches = false
					}
				}
				if matches {
					return metric.GetGauge().GetValue()
				}
			}
		}
		t.Fatalf("missing metric %s %v", name, labels)
		return 0
	}
	collector.refresh(t.Context())
	pending := map[string]string{"kind": "release", "source": "local", "state": "pending"}
	if got := read("puppet_forge_resources", pending); got != 2 {
		t.Fatal(got)
	}
	if got := read("puppet_forge_resource_oldest_pending_timestamp_seconds", map[string]string{"kind": "release", "source": "local"}); got != 1000 {
		t.Fatal(got)
	}
	if got := read("puppet_forge_resources", map[string]string{"kind": "unknown", "source": "unknown", "state": "unknown"}); got != 3 {
		t.Fatal("unbounded state labels", got)
	}
	if source.calls != 1 {
		t.Fatal("scrape performed resource storage I/O")
	}
	source.err = errors.New("SQL unavailable")
	collector.refresh(t.Context())
	if got := read("puppet_forge_resources", pending); got != 2 || collector.refreshErrors != 1 {
		t.Fatal("failed refresh discarded last good inventory")
	}
	source.err, source.resources = nil, nil
	collector.refresh(t.Context())
	if got := read("puppet_forge_resources", pending); got != 0 {
		t.Fatal("empty inventory retained pending resources")
	}
	if got := read("puppet_forge_resource_oldest_pending_timestamp_seconds", map[string]string{"kind": "release", "source": "local"}); got != 0 {
		t.Fatal("empty approval queue retained an age")
	}
}
