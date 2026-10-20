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

	informerRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kar_informer_restarts_total",
		Help: "Informers kar replaced to re-evaluate a watched resource, per resource.",
	}, []string{"resource"})

	registerOnce sync.Once
)

// registerMetrics registers the collectors once per process, however many reconcilers are set up.
func registerMetrics() {
	registerOnce.Do(func() { metrics.Registry.MustRegister(violations, exemptNamespaces, informerRestarts) })
}

// recordPass replaces binding's gauges with the counts of one audit pass.
func recordPass(binding string, counts map[string]int, exempt int) {
	violations.DeletePartialMatch(prometheus.Labels{labelBinding: binding})

	for ns, n := range counts {
		violations.WithLabelValues(binding, ns).Set(float64(n))
	}

	exemptNamespaces.WithLabelValues(binding).Set(float64(exempt))
}

// forgetMetrics removes binding's series.
func forgetMetrics(binding string) {
	violations.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
	exemptNamespaces.DeletePartialMatch(prometheus.Labels{labelBinding: binding})
}
