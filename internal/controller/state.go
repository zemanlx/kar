package controller

import (
	"maps"
	"slices"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

// statusStep is how far an expiration must move before it is worth a status write.
const statusStep = time.Hour

// entry is the exemption state of one namespace.
type entry struct{ FirstSeen, Expiration time.Time }

// exemptions is the exemption state of one source binding, keyed by namespace.
type exemptions map[string]entry

// see records a violation in ns at now and extends its expiration by ttl. It returns true when ns
// was not exempt before.
func (e exemptions) see(ns string, now time.Time, ttl time.Duration) bool {
	old, added := e[ns]
	if !added {
		old.FirstSeen = now
	}

	if exp := now.Add(ttl); exp.After(old.Expiration) {
		old.Expiration = exp
	}

	e[ns] = old

	return !added
}

// expire drops entries that have expired or whose namespace no longer exists.
func (e exemptions) expire(now time.Time, exists func(ns string) bool) {
	for ns, en := range e {
		if en.Expiration.Before(now) || !exists(ns) {
			delete(e, ns)
		}
	}
}

// names returns the exempt namespaces, sorted.
func (e exemptions) names() []string {
	return slices.Sorted(maps.Keys(e))
}

// recoverState rebuilds exemptions after a restart from the persisted status plus the namespaces
// still listed in derived's kar expression. Those namespaces have no recorded times, so they start
// at now.
func recoverState(status []karv1alpha1.ExemptNamespace, derived, source *admissionregv1.ValidatingAdmissionPolicyBinding, now time.Time, ttl time.Duration) exemptions {
	out := make(exemptions, len(status))
	for _, s := range status {
		out[s.Name] = entry{FirstSeen: s.FirstSeen.Time, Expiration: s.Expiration.Time}
	}

	if kar := karExpression(derived, source); kar != nil {
		for _, ns := range kar.Values {
			if _, ok := out[ns]; !ok {
				out[ns] = entry{FirstSeen: now, Expiration: now.Add(ttl)}
			}
		}
	}

	return out
}

// toStatus renders e as status entries sorted by namespace.
func toStatus(e exemptions) []karv1alpha1.ExemptNamespace {
	names := e.names()

	out := make([]karv1alpha1.ExemptNamespace, 0, len(names))
	for _, ns := range names {
		en := e[ns]

		out = append(out, karv1alpha1.ExemptNamespace{
			Name:       ns,
			FirstSeen:  metav1.NewTime(en.FirstSeen),
			Expiration: metav1.NewTime(en.Expiration),
		})
	}

	return out
}

// effectiveMode returns the mode kar applies for a binding annotated with mode. An auto binding enforces
// once since plus ttl has passed, or at once when latched; until then it observes. A zero since means the
// observation clock has not started.
func effectiveMode(mode string, since time.Time, ttl time.Duration, latched bool, now time.Time) string {
	if mode != modeAuto {
		return mode
	}

	if latched || (!since.IsZero() && !now.Before(since.Add(ttl))) {
		return modeEnforce
	}

	return modeObserve
}

// needsWrite reports whether the persisted status differs from e enough to be worth a write: a
// different set of namespaces, or an expiration pushed back by at least statusStep or ttl, whichever
// is shorter. A new leader loads the persisted expirations, so they may trail by less than one ttl.
func needsWrite(persisted []karv1alpha1.ExemptNamespace, e exemptions, ttl time.Duration) bool {
	if len(persisted) != len(e) {
		return true
	}

	step := min(statusStep, ttl)

	for _, p := range persisted {
		en, ok := e[p.Name]
		if !ok || en.Expiration.Sub(p.Expiration.Time) >= step {
			return true
		}
	}

	return false
}
