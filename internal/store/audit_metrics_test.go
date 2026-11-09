package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/zxzharmlesszxz/puppet-forge/internal/testutil"
)

func TestStoreParityAuditMetricsFollowTransactionResult(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "auditmetrics", "demo")
			if err != nil {
				t.Fatal(err)
			}
			event := AuditEvent{ID: uuid.NewString(), Action: "publish_module", Result: "success"}
			success := map[string]string{"result": "success"}
			failure := map[string]string{"result": "error"}
			before := testutil.MetricCounterValue(t, "puppet_forge_audit_writes_total", success)
			release := NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "", "", "demo.tar.gz", "application/gzip", "", "sha", "modules/demo1", 1, nil)
			if _, err := st.CreateRelease(ContextWithMutationAudit(t.Context(), event), release); err != nil {
				t.Fatal(err)
			}
			if got := testutil.MetricCounterValue(t, "puppet_forge_audit_writes_total", success); got < before+1 {
				t.Fatal("successful commit did not record audit persistence")
			}
			before = testutil.MetricCounterValue(t, "puppet_forge_audit_writes_total", failure)
			release.ID, release.Version, release.StoragePath = uuid.NewString(), "2.0.0", "modules/demo2"
			if _, err := st.CreateRelease(ContextWithMutationAudit(t.Context(), event), release); err == nil {
				t.Fatal("duplicate audit ID did not fail the mutation")
			}
			if got := testutil.MetricCounterValue(t, "puppet_forge_audit_writes_total", failure); got < before+1 {
				t.Fatal("failed transaction audit was not counted")
			}
			if _, err := st.GetRelease(t.Context(), module.Owner, module.Name, "2.0.0"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("audit failure did not roll back publication: %v", err)
			}
		})
	}
}
