package metrics

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestControlMetricsBoundLabelsAndCountOutcomes(t *testing.T) {
	for _, result := range []string{"success", "rejected", "error"} {
		counter := controlOperationsTotal.WithLabelValues("approve", "release", result)
		before := testutil.ToFloat64(counter)
		ObserveControlOperation("approve", "release", result)
		if got := testutil.ToFloat64(counter); got != before+1 {
			t.Fatalf("%s outcome=%v want=%v", result, got, before+1)
		}
	}
	before := testutil.ToFloat64(controlOperationsTotal.WithLabelValues("other", "other", "other"))
	ObserveControlOperation("private@example.com", "secret-token", "request-123")
	if got := testutil.ToFloat64(controlOperationsTotal.WithLabelValues("other", "other", "other")); got != before+1 {
		t.Fatal("unbounded control labels")
	}
	ObserveAuthorization("arbitrary-user", "arbitrary-reason")
	if got := testutil.ToFloat64(authorizationChecksTotal.WithLabelValues("other", "other")); got < 1 {
		t.Fatal("unbounded authorization labels")
	}
	ObserveSecurityEvent("private-user-action", "private-error")
	if got := testutil.ToFloat64(securityEventsTotal.WithLabelValues("other", "other")); got < 1 {
		t.Fatal("unbounded security labels")
	}
	for _, err := range []error{nil, errors.New("private storage error")} {
		result := resultLabel(err)
		auditBefore := testutil.ToFloat64(auditWritesTotal.WithLabelValues(result))
		refreshBefore := testutil.ToFloat64(accessConfigRefreshTotal.WithLabelValues(result))
		ObserveAuditWrite(err)
		ObserveAccessConfigRefresh(err)
		if testutil.ToFloat64(auditWritesTotal.WithLabelValues(result)) != auditBefore+1 || testutil.ToFloat64(accessConfigRefreshTotal.WithLabelValues(result)) != refreshBefore+1 {
			t.Fatal("audit or access-config refresh outcome lost")
		}
	}
}

func TestObserveOutcomePreservesErrorsAndPanics(t *testing.T) {
	want := errors.New("SQL failure")
	var observed error
	func() {
		var err error
		defer ObserveOutcome(&err, func(value error) { observed = value })
		err = want
	}()
	if !errors.Is(observed, want) {
		t.Fatal("deferred outcome did not observe the final error")
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		var err error
		defer ObserveOutcome(&err, func(value error) { observed = value })
		panic("test panic")
	}()
	if recovered != "test panic" || observed == nil {
		t.Fatal("panic was swallowed or recorded as success")
	}
}
