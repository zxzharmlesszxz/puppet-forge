package observability

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type staticModuleMetricsSource struct {
	owners    map[string]int
	summaries []domain.ReleaseMetricSummary
	err       error
}

type staticReleaseConsumerMetricsSource struct {
	staticModuleMetricsSource
	consumers []store.ReleaseConsumer
	total     int
}

func (s staticReleaseConsumerMetricsSource) ListReleaseConsumers(context.Context, time.Time, int) ([]store.ReleaseConsumer, int, error) {
	return s.consumers, s.total, nil
}

func (s staticModuleMetricsSource) CountModulesByOwner(context.Context, []string) (map[string]int, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.owners, nil
}

func (s staticModuleMetricsSource) ListReleaseMetricSummaries(context.Context) ([]domain.ReleaseMetricSummary, error) {
	return s.summaries, nil
}

type panicModuleMetricsSource struct{}

func (panicModuleMetricsSource) CountModulesByOwner(context.Context, []string) (map[string]int, error) {
	panic("Collect performed storage I/O")
}

func (panicModuleMetricsSource) ListReleaseMetricSummaries(context.Context) ([]domain.ReleaseMetricSummary, error) {
	panic("Collect performed storage I/O")
}

func TestModuleMetricsCollectorExportsAggregatedReleaseMetrics(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer st.Close()

	modules := service.NewModuleService(st, nil, "modules", nil)
	err = modules.IndexUpstreamModule(ctx, proxy.UpstreamModule{
		Slug:  "puppetlabs-concat",
		Owner: "puppetlabs",
		Name:  "concat",
		CurrentRelease: proxy.UpstreamReleaseRef{
			Slug:    "puppetlabs-concat-9.1.0",
			Version: "9.1.0",
		},
		Releases: []proxy.UpstreamReleaseRef{
			{Slug: "puppetlabs-concat-8.0.0", Version: "8.0.0"},
			{Slug: "puppetlabs-concat-9.1.0", Version: "9.1.0"},
		},
	})
	if err != nil {
		t.Fatalf("IndexUpstreamModule() error = %v", err)
	}

	collector := newModuleMetricsCollector(modules, 10000, 10000, 180*24*time.Hour)
	collector.refresh(ctx)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	expected := `
# HELP puppet_forge_modules Known Puppet modules indexed by the service, grouped by owner.
# TYPE puppet_forge_modules gauge
puppet_forge_modules{owner="puppetlabs"} 1
# HELP puppet_forge_module_latest_releases Known latest Puppet module releases indexed by the service, grouped by source.
# TYPE puppet_forge_module_latest_releases gauge
puppet_forge_module_latest_releases{source="upstream"} 1
# HELP puppet_forge_module_releases Known Puppet module releases indexed by the service, grouped by source.
# TYPE puppet_forge_module_releases gauge
puppet_forge_module_releases{source="upstream"} 2
`
	if err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(expected),
		"puppet_forge_modules",
		"puppet_forge_module_latest_releases",
		"puppet_forge_module_releases",
	); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsCollectDoesNotAccessStorage(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	registry.MustRegister(newModuleMetricsCollector(panicModuleMetricsSource{}, 10, 10, time.Hour))

	expected := `
# HELP puppet_forge_module_metrics_last_success_timestamp_seconds Unix timestamp of the last successful module inventory metrics refresh.
# TYPE puppet_forge_module_metrics_last_success_timestamp_seconds gauge
puppet_forge_module_metrics_last_success_timestamp_seconds 0
# HELP puppet_forge_module_metrics_owners_total Total number of module owners found during the last successful inventory refresh.
# TYPE puppet_forge_module_metrics_owners_total gauge
puppet_forge_module_metrics_owners_total 0
# HELP puppet_forge_module_metrics_ready Whether the module inventory metrics cache has completed at least one successful refresh.
# TYPE puppet_forge_module_metrics_ready gauge
puppet_forge_module_metrics_ready 0
# HELP puppet_forge_module_metrics_refresh_errors_total Total number of failed module inventory metrics refreshes.
# TYPE puppet_forge_module_metrics_refresh_errors_total counter
puppet_forge_module_metrics_refresh_errors_total 0
# HELP puppet_forge_module_metrics_truncated Whether owner-labelled module inventory metrics were truncated by METRICS_MODULE_LIMIT.
# TYPE puppet_forge_module_metrics_truncated gauge
puppet_forge_module_metrics_truncated 0
# HELP puppet_forge_release_consumer_metrics_truncated Whether release consumer metrics were truncated by METRICS_RELEASE_CONSUMER_LIMIT.
# TYPE puppet_forge_release_consumer_metrics_truncated gauge
puppet_forge_release_consumer_metrics_truncated 0
# HELP puppet_forge_release_consumer_series_total Release consumer records found during the last successful inventory refresh.
# TYPE puppet_forge_release_consumer_series_total gauge
puppet_forge_release_consumer_series_total 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected)); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsReportsLegacyReleaseConsumers(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	collector := newModuleMetricsCollector(staticReleaseConsumerMetricsSource{
		owners: map[string]int{},
		consumers: []store.ReleaseConsumer{{
			ConsumerTeam:  "platform",
			ConsumerName:  "production",
			ConsumerRole:  "read",
			Owner:         "puppetlabs",
			Name:          "stdlib",
			Version:       "8.0.0",
			LatestVersion: "9.0.0",
			LastSeenAt:    now,
			Observations:  3,
		}},
		total: 1,
	}, 10, 10, time.Hour)
	collector.refresh(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	expected := `
# HELP puppet_forge_release_consumer_last_seen_timestamp_seconds Unix timestamp of the latest observed artifact download by a named access token.
# TYPE puppet_forge_release_consumer_last_seen_timestamp_seconds gauge
puppet_forge_release_consumer_last_seen_timestamp_seconds{consumer="production",consumer_role="read",consumer_team="platform",latest_version="9.0.0",module="stdlib",owner="puppetlabs",status="legacy",version="8.0.0"} 1.7e+09
# HELP puppet_forge_release_consumer_observations Retained coalesced artifact download observations for a named access token and module release.
# TYPE puppet_forge_release_consumer_observations gauge
puppet_forge_release_consumer_observations{consumer="production",consumer_role="read",consumer_team="platform",latest_version="9.0.0",module="stdlib",owner="puppetlabs",status="legacy",version="8.0.0"} 3
# HELP puppet_forge_release_consumer_series_total Release consumer records found during the last successful inventory refresh.
# TYPE puppet_forge_release_consumer_series_total gauge
puppet_forge_release_consumer_series_total 1
# HELP puppet_forge_release_consumer_metrics_truncated Whether release consumer metrics were truncated by METRICS_RELEASE_CONSUMER_LIMIT.
# TYPE puppet_forge_release_consumer_metrics_truncated gauge
puppet_forge_release_consumer_metrics_truncated 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected), "puppet_forge_release_consumer_last_seen_timestamp_seconds", "puppet_forge_release_consumer_observations", "puppet_forge_release_consumer_series_total", "puppet_forge_release_consumer_metrics_truncated"); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsReportsConsumerTruncation(t *testing.T) {
	t.Parallel()

	collector := newModuleMetricsCollector(staticReleaseConsumerMetricsSource{
		owners: map[string]int{},
		total:  11,
	}, 10, 10, time.Hour)
	collector.refresh(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	expected := `
# HELP puppet_forge_release_consumer_series_total Release consumer records found during the last successful inventory refresh.
# TYPE puppet_forge_release_consumer_series_total gauge
puppet_forge_release_consumer_series_total 11
# HELP puppet_forge_release_consumer_metrics_truncated Whether release consumer metrics were truncated by METRICS_RELEASE_CONSUMER_LIMIT.
# TYPE puppet_forge_release_consumer_metrics_truncated gauge
puppet_forge_release_consumer_metrics_truncated 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected), "puppet_forge_release_consumer_series_total", "puppet_forge_release_consumer_metrics_truncated"); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsReportsOwnerTruncation(t *testing.T) {
	t.Parallel()

	collector := newModuleMetricsCollector(staticModuleMetricsSource{
		owners: map[string]int{"alpha": 1, "beta": 2, "gamma": 3},
	}, 2, 10, time.Hour)
	collector.refresh(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	expected := `
# HELP puppet_forge_modules Known Puppet modules indexed by the service, grouped by owner.
# TYPE puppet_forge_modules gauge
puppet_forge_modules{owner="alpha"} 1
puppet_forge_modules{owner="beta"} 2
# HELP puppet_forge_module_metrics_owners_total Total number of module owners found during the last successful inventory refresh.
# TYPE puppet_forge_module_metrics_owners_total gauge
puppet_forge_module_metrics_owners_total 3
# HELP puppet_forge_module_metrics_truncated Whether owner-labelled module inventory metrics were truncated by METRICS_MODULE_LIMIT.
# TYPE puppet_forge_module_metrics_truncated gauge
puppet_forge_module_metrics_truncated 1
`
	if err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(expected),
		"puppet_forge_modules",
		"puppet_forge_module_metrics_owners_total",
		"puppet_forge_module_metrics_truncated",
	); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsReportsRefreshFailure(t *testing.T) {
	t.Parallel()

	collector := newModuleMetricsCollector(staticModuleMetricsSource{err: errors.New("store unavailable")}, 10, 10, time.Hour)
	collector.refresh(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	expected := `
# HELP puppet_forge_module_metrics_last_success_timestamp_seconds Unix timestamp of the last successful module inventory metrics refresh.
# TYPE puppet_forge_module_metrics_last_success_timestamp_seconds gauge
puppet_forge_module_metrics_last_success_timestamp_seconds 0
# HELP puppet_forge_module_metrics_ready Whether the module inventory metrics cache has completed at least one successful refresh.
# TYPE puppet_forge_module_metrics_ready gauge
puppet_forge_module_metrics_ready 0
# HELP puppet_forge_module_metrics_refresh_errors_total Total number of failed module inventory metrics refreshes.
# TYPE puppet_forge_module_metrics_refresh_errors_total counter
puppet_forge_module_metrics_refresh_errors_total 1
`
	if err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(expected),
		"puppet_forge_module_metrics_last_success_timestamp_seconds",
		"puppet_forge_module_metrics_ready",
		"puppet_forge_module_metrics_refresh_errors_total",
	); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}

func TestModuleMetricsRegistrationsUseIndependentRegistries(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	source := staticModuleMetricsSource{owners: map[string]int{"teamname": 1}}
	waitFirst, err := RegisterModuleMetrics(ctx, source, 10, 10, time.Hour, time.Hour, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("RegisterModuleMetrics(first) error = %v", err)
	}
	waitSecond, err := RegisterModuleMetrics(ctx, source, 10, 10, time.Hour, time.Hour, prometheus.NewRegistry())
	if err != nil {
		cancel()
		waitFirst()
		t.Fatalf("RegisterModuleMetrics(second) error = %v", err)
	}
	cancel()
	waitFirst()
	waitSecond()
}

func TestJitteredMetricsIntervalStaysWithinWindow(t *testing.T) {
	t.Parallel()

	const interval = time.Minute
	for range 100 {
		got := jitteredMetricsInterval(interval)
		if got < 54*time.Second || got > 66*time.Second {
			t.Fatalf("jitteredMetricsInterval() = %s, want [54s, 66s]", got)
		}
	}
}

func TestJitteredMetricsIntervalFallsBackWhenRandomSourceFails(t *testing.T) {
	t.Parallel()

	const interval = time.Minute
	if got := jitteredMetricsIntervalFrom(interval, failingReader{}); got != interval {
		t.Fatalf("jitteredMetricsIntervalFrom() = %s, want %s", got, interval)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
