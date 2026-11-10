package controller

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const labelBinding = "binding"

//nolint:gochecknoglobals // Collectors are package-wide by nature; registerMetrics registers them once.
var (
	violations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kar_violations",
		Help: "Existing objects a source binding's policy denies, per namespace (\"\" for cluster-scoped).",
	}, []string{labelBinding, "namespace"})

	exemptNamespaces = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kar_exempt_namespaces",
		Help: "Namespaces exempted from a source binding's policy.",
	}, []string{labelBinding})

	exemptionExpiry = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kar_exemption_expiry_timestamp_seconds",
		Help: "Unix time a namespace's exemption ends unless kar sees another violation there.",
	}, []string{labelBinding, "namespace"})

	informerRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kar_informer_restarts_total",
		Help: "Informers kar replaced to re-evaluate a watched resource, per resource.",
	}, []string{"resource"})

	registerOnce sync.Once
)

// registerMetrics registers the collectors once per process, however many reconcilers are set up.
func registerMetrics() {
	registerOnce.Do(func() {
		metrics.Registry.MustRegister(violations, exemptNamespaces, exemptionExpiry, informerRestarts)
	})
}

// recordPass replaces binding's gauges with the counts and exemptions of one audit pass.
func recordPass(binding string, counts map[string]int, state exemptions) {
	violations.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
	exemptionExpiry.DeletePartialMatch(prometheus.Labels{labelBinding: binding})

	for ns, n := range counts {
		violations.WithLabelValues(binding, ns).Set(float64(n))
	}

	for ns, en := range state {
		exemptionExpiry.WithLabelValues(binding, ns).Set(float64(en.Expiration.Unix()))
	}

	exemptNamespaces.WithLabelValues(binding).Set(float64(len(state)))
}

// forgetMetrics removes binding's series.
func forgetMetrics(binding string) {
	violations.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
	exemptNamespaces.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
	exemptionExpiry.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
}
