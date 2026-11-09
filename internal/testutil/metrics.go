package testutil

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricCounterValue reads a named default-registry counter without resetting
// process-wide collectors used by other tests.
func MetricCounterValue(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := len(labels) == len(metric.Label)
			for _, label := range metric.Label {
				value, ok := labels[label.GetName()]
				if !ok || value != label.GetValue() {
					matches = false
				}
			}
			if matches {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
