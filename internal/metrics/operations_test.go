package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestOperationCounters(t *testing.T) {
	ObservePublish(nil)
	ObservePublish(errors.New("boom"))
	ObserveDelete("release", nil)
	ObserveReleaseUsageMark(nil)
	ObserveUpstreamSync("single", errors.New("boom"))
	ObserveUpstreamCache("json", "hit")
	ObserveArtifactDeletion("deleted")
	ObserveArtifactStream("local", "success", 128)
	ObserveArtifactStream("upstream_cache", "error", 64)
	ObserveUpstreamArtifactIntegrity("repaired")
	ObserveUpstreamArtifactCleanup(nil, 7, 3, 0, 250*time.Millisecond)
	ObserveUpstreamArtifactCleanup(errors.New("partial cleanup"), 4, 2, 1, 500*time.Millisecond)
	SetArtifactDeletionsPending(3)

	if got := testutil.ToFloat64(publishTotal.WithLabelValues("success")); got < 1 {
		t.Fatalf("publish success counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(publishTotal.WithLabelValues("error")); got < 1 {
		t.Fatalf("publish error counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(deleteTotal.WithLabelValues("success", "release")); got < 1 {
		t.Fatalf("delete success counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(releaseUsageMarkedTotal.WithLabelValues("success")); got < 1 {
		t.Fatalf("release usage counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(upstreamSyncTotal.WithLabelValues("error", "single")); got < 1 {
		t.Fatalf("upstream sync error counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(upstreamCacheRequestsTotal.WithLabelValues("json", "hit")); got < 1 {
		t.Fatalf("upstream cache hit counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(artifactDeletionTotal.WithLabelValues("deleted")); got < 1 {
		t.Fatalf("artifact deletion counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(artifactStreamTotal.WithLabelValues("local", "success")); got < 1 {
		t.Fatalf("local artifact stream success counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(artifactStreamTotal.WithLabelValues("upstream_cache", "error")); got < 1 {
		t.Fatalf("upstream artifact stream error counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(artifactStreamBytesTotal.WithLabelValues("local")); got < 128 {
		t.Fatalf("local artifact stream bytes counter = %f, want at least 128", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactIntegrityTotal.WithLabelValues("repaired")); got < 1 {
		t.Fatalf("upstream artifact repaired counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactCleanupTotal.WithLabelValues("success")); got < 1 {
		t.Fatalf("upstream artifact cleanup success counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactCleanupTotal.WithLabelValues("error")); got < 1 {
		t.Fatalf("upstream artifact cleanup error counter = %f, want at least 1", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactCleanupObjectsTotal); got < 5 {
		t.Fatalf("upstream artifact cleanup objects counter = %f, want at least 5", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactCleanupScannedTotal); got < 11 {
		t.Fatalf("upstream artifact cleanup scanned counter = %f, want at least 11", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactCleanupFailuresTotal); got < 1 {
		t.Fatalf("upstream artifact cleanup failures counter = %f, want at least 1", got)
	}
	if got := testutil.CollectAndCount(upstreamArtifactCleanupDuration); got != 1 {
		t.Fatalf("upstream artifact cleanup duration metric count = %d, want 1", got)
	}
	if got := testutil.ToFloat64(artifactDeletionsPending); got != 3 {
		t.Fatalf("pending artifact deletion gauge = %f, want 3", got)
	}
}

func TestOperationMetricsAreInitialized(t *testing.T) {
	if got := testutil.ToFloat64(upstreamSyncTotal.WithLabelValues("success", "single")); got != 0 {
		t.Fatalf("upstream sync single success counter = %f, want 0", got)
	}
	if got := testutil.ToFloat64(upstreamCacheRequestsTotal.WithLabelValues("json", "miss")); got != 0 {
		t.Fatalf("upstream cache json miss counter = %f, want 0", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshModules.WithLabelValues("attempted")); got != 0 {
		t.Fatalf("upstream refresh attempted gauge = %f, want 0", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshCyclesTotal.WithLabelValues("success")); got != 0 {
		t.Fatalf("upstream refresh success cycles counter = %f, want 0", got)
	}
	if got := testutil.ToFloat64(artifactDeletionTotal.WithLabelValues("error")); got != 0 {
		t.Fatalf("artifact deletion error counter = %f, want 0", got)
	}
	if got := testutil.ToFloat64(upstreamArtifactIntegrityTotal.WithLabelValues("valid")); got != 0 {
		t.Fatalf("upstream artifact valid counter = %f, want 0", got)
	}
}

func TestBuildInfo(t *testing.T) {
	RecordBuildInfo("1.2.3", "go1.26.0")

	if got := testutil.ToFloat64(buildInfo.WithLabelValues("1.2.3", "go1.26.0")); got != 1 {
		t.Fatalf("build_info gauge = %f, want 1", got)
	}

	RecordBuildInfo("1.2.4", "go1.26.1")
	if got := testutil.ToFloat64(buildInfo.WithLabelValues("1.2.3", "go1.26.0")); got != 0 {
		t.Fatalf("old build_info gauge = %f, want 0", got)
	}
	if got := testutil.ToFloat64(buildInfo.WithLabelValues("1.2.4", "go1.26.1")); got != 1 {
		t.Fatalf("new build_info gauge = %f, want 1", got)
	}
}

func TestObserveUpstreamRefresh(t *testing.T) {
	ObserveUpstreamRefresh(time.Now().Add(-time.Second), 3, 2, 1)

	if got := testutil.ToFloat64(upstreamRefreshModules.WithLabelValues("attempted")); got != 3 {
		t.Fatalf("attempted modules gauge = %f, want 3", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshModules.WithLabelValues("success")); got != 2 {
		t.Fatalf("success modules gauge = %f, want 2", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshModules.WithLabelValues("error")); got != 1 {
		t.Fatalf("error modules gauge = %f, want 1", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshLastErrorTimestamp); got <= 0 {
		t.Fatalf("last error timestamp = %f, want positive", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshLastDuration); got <= 0 {
		t.Fatalf("last duration = %f, want positive", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshLastTimestamp); got <= 0 {
		t.Fatalf("last refresh timestamp = %f, want positive", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshCyclesTotal.WithLabelValues("error")); got < 1 {
		t.Fatalf("refresh error cycles counter = %f, want at least 1", got)
	}

	ObserveUpstreamRefresh(time.Now().Add(-time.Second), 1, 1, 0)
	if got := testutil.ToFloat64(upstreamRefreshLastSuccessTimestamp); got <= 0 {
		t.Fatalf("last success timestamp = %f, want positive", got)
	}
	if got := testutil.ToFloat64(upstreamRefreshCyclesTotal.WithLabelValues("success")); got < 1 {
		t.Fatalf("refresh success cycles counter = %f, want at least 1", got)
	}
}
