package controller

import (
	"cmp"
	"maps"
	"slices"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
)

const testTTL = 24 * time.Hour

func sameExemptions(a, b exemptions) bool {
	if len(a) != len(b) {
		return false
	}

	for ns, ea := range a {
		eb, ok := b[ns]
		if !ok || !ea.FirstSeen.Equal(eb.FirstSeen) || !ea.Expiration.Equal(eb.Expiration) {
			return false
		}
	}

	return true
}

func TestExemptionsSee(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		start     exemptions
		now       time.Time
		wantAdded bool
		want      entry
	}{
		{
			name:      "new namespace",
			start:     exemptions{},
			now:       t0,
			wantAdded: true,
			want:      entry{FirstSeen: t0, Expiration: t0.Add(testTTL)},
		},
		{
			name:      "extends without moving first seen",
			start:     exemptions{"a": {FirstSeen: t0, Expiration: t0.Add(testTTL)}},
			now:       t0.Add(time.Hour),
			wantAdded: false,
			want:      entry{FirstSeen: t0, Expiration: t0.Add(time.Hour + testTTL)},
		},
		{
			name:      "never moves expiration back",
			start:     exemptions{"a": {FirstSeen: t0, Expiration: t0.Add(10 * testTTL)}},
			now:       t0.Add(time.Hour),
			wantAdded: false,
			want:      entry{FirstSeen: t0, Expiration: t0.Add(10 * testTTL)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := maps.Clone(tt.start)
			if added := e.see("a", tt.now, testTTL); added != tt.wantAdded {
				t.Errorf("see() added = %v, want %v", added, tt.wantAdded)
			}

			if !sameExemptions(e, exemptions{"a": tt.want}) {
				t.Errorf("state = %+v, want %+v", e, tt.want)
			}
		})
	}
}

func TestExemptionsExpire(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	all := func(string) bool { return true }
	notLive := func(ns string) bool { return ns != "live" }

	tests := []struct {
		name   string
		now    time.Time
		exists func(string) bool
		want   []string
	}{
		{name: "drops expired", now: t0.Add(2 * testTTL), exists: all, want: []string{"live"}},
		{name: "drops missing namespace", now: t0, exists: notLive, want: []string{"old"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := exemptions{
				"old":  {FirstSeen: t0, Expiration: t0.Add(testTTL)},
				"live": {FirstSeen: t0, Expiration: t0.Add(3 * testTTL)},
			}
			e.expire(tt.now, tt.exists)

			if got := e.names(); !slices.Equal(got, tt.want) {
				t.Errorf("names() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecoverState(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	now := t0.Add(time.Hour)

	// The source has its own NotIn on the name key, which recovery must not mistake for kar's.
	human := metav1.LabelSelectorRequirement{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"h"}}
	env := metav1.LabelSelectorRequirement{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}
	source := sourceWith([]metav1.LabelSelectorRequirement{human})
	derivedWith := func(exempt ...string) *admissionregv1.ValidatingAdmissionPolicyBinding {
		return derive(source, exempt)
	}

	kept := karv1alpha1.ExemptNamespace{Name: "a", FirstSeen: metav1.NewTime(t0), Expiration: metav1.NewTime(t0.Add(testTTL))}
	keptEntry := entry{FirstSeen: t0, Expiration: t0.Add(testTTL)}
	fresh := entry{FirstSeen: now, Expiration: now.Add(testTTL)}

	tests := []struct {
		name    string
		status  []karv1alpha1.ExemptNamespace
		derived *admissionregv1.ValidatingAdmissionPolicyBinding
		source  *admissionregv1.ValidatingAdmissionPolicyBinding
		want    exemptions
	}{
		{name: "status kept as is without derived", status: []karv1alpha1.ExemptNamespace{kept}, source: source, want: exemptions{"a": keptEntry}},
		{name: "kar names missing from status start now", status: []karv1alpha1.ExemptNamespace{kept}, derived: derivedWith("a", "b"), source: source, want: exemptions{"a": keptEntry, "b": fresh}},
		{name: "source's own human NotIn is not a kar name", derived: derivedWith("a"), source: source, want: exemptions{"a": fresh}},
		{name: "source's last expression is a human NotIn and derived has no kar expression", derived: derivedWith(), source: source, want: exemptions{}},
		{name: "source gained an expression since derived was written", derived: derivedWith("a"), source: sourceWith([]metav1.LabelSelectorRequirement{human, env}), want: exemptions{"a": fresh}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := recoverState(tt.status, tt.derived, tt.source, now, testTTL)
			if !sameExemptions(got, tt.want) {
				t.Errorf("recoverState() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestToStatusAndNeedsWrite(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	e := exemptions{
		"b": {FirstSeen: t0, Expiration: t0.Add(testTTL)},
		"a": {FirstSeen: t0, Expiration: t0.Add(testTTL)},
	}

	persisted := toStatus(e)
	if got := []string{persisted[0].Name, persisted[1].Name}; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("toStatus() names = %q, want sorted", got)
	}

	// shifted returns persisted with ns's expiration moved by d.
	shifted := func(d time.Duration, ns string) []karv1alpha1.ExemptNamespace {
		out := slices.Clone(persisted)
		for i := range out {
			if out[i].Name == ns {
				out[i].Expiration = metav1.NewTime(out[i].Expiration.Add(d))
			}
		}

		return out
	}

	tests := []struct {
		name      string
		persisted []karv1alpha1.ExemptNamespace
		ttl       time.Duration // testTTL when zero
		want      bool
	}{
		{name: "same state", persisted: persisted, want: false},
		{name: "expiration moved under the step", persisted: shifted(-time.Hour+time.Minute, "a"), want: false},
		{name: "expiration moved past the step", persisted: shifted(-time.Hour, "a"), want: true},
		{name: "namespace missing from status", persisted: persisted[:1], want: true},
		{name: "namespace not yet exempt", persisted: append(slices.Clone(persisted), karv1alpha1.ExemptNamespace{Name: "c", Expiration: metav1.NewTime(t0)}), want: true},
		{name: "short TTL: expiration moved under the TTL", persisted: shifted(-time.Minute, "a"), ttl: 2 * time.Minute, want: false},
		{name: "short TTL: expiration moved by the TTL", persisted: shifted(-2*time.Minute, "a"), ttl: 2 * time.Minute, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ttl := cmp.Or(tt.ttl, testTTL)
			if got := needsWrite(tt.persisted, e, ttl); got != tt.want {
				t.Errorf("needsWrite() = %v, want %v", got, tt.want)
			}
		})
	}
}
