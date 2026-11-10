package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gaugeSeries returns the value of each series of g whose binding label is binding, keyed by namespace.
func gaugeSeries(t *testing.T, g *prometheus.GaugeVec, binding string) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric)
	go func() { g.Collect(ch); close(ch) }()

	out := map[string]float64{}

	for m := range ch {
		d := &dto.Metric{}
		if err := m.Write(d); err != nil {
			t.Fatalf("read gauge: %v", err)
		}

		labels := map[string]string{}
		for _, l := range d.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}

		if labels[labelBinding] == binding {
			out[labels["namespace"]] = d.GetGauge().GetValue()
		}
	}

	return out
}

// TestRecordPassExemptionExpiry reads package-level gauges under its own binding label.
//
//nolint:paralleltest // writes the global gauges
func TestRecordPassExemptionExpiry(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	binding := "test-record-pass-expiry"

	t.Cleanup(func() { forgetMetrics(binding) })

	recordPass(binding, map[string]int{"a": 2}, exemptions{
		"a": {FirstSeen: t0, Expiration: t0.Add(testTTL)},
		"b": {FirstSeen: t0, Expiration: t0.Add(time.Hour)},
	})

	got := gaugeSeries(t, exemptionExpiry, binding)
	if len(got) != 2 || got["b"] != float64(t0.Add(time.Hour).Unix()) {
		t.Fatalf("expiry series = %v, want a and b with b at %d", got, t0.Add(time.Hour).Unix())
	}

	recordPass(binding, nil, exemptions{"a": {FirstSeen: t0, Expiration: t0.Add(testTTL)}})

	if got := gaugeSeries(t, exemptionExpiry, binding); len(got) != 1 {
		t.Fatalf("expiry series after b expired = %v, want only a", got)
	}

	forgetMetrics(binding)

	if got := gaugeSeries(t, exemptionExpiry, binding); len(got) != 0 {
		t.Fatalf("expiry series after forget = %v, want none", got)
	}
}
