package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zxzharmlesszxz/puppet-forge/internal/proxy"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

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

	registry := prometheus.NewRegistry()
	registry.MustRegister(newModuleMetricsCollector(modules, 10000))

	expected := `
# HELP puppet_forge_module_info Known Puppet modules indexed by the service.
# TYPE puppet_forge_module_info gauge
puppet_forge_module_info{latest_version="9.1.0",module="puppetlabs-concat",name="concat",owner="puppetlabs"} 1
# HELP puppet_forge_module_latest_releases Known latest Puppet module releases indexed by the service, grouped by source.
# TYPE puppet_forge_module_latest_releases gauge
puppet_forge_module_latest_releases{source="upstream"} 1
# HELP puppet_forge_module_releases Known Puppet module releases indexed by the service, grouped by source.
# TYPE puppet_forge_module_releases gauge
puppet_forge_module_releases{source="upstream"} 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected)); err != nil {
		t.Fatalf("GatherAndCompare() error = %v", err)
	}
}
