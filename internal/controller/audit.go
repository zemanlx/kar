package controller

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/environment"
)

// celEnv parses policy expressions as the API server does, so its syntax and macros apply.
var celEnv = environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()).StoredExpressionsEnv() //nolint:gochecknoglobals // built once, read-only afterwards

// auditIssues returns why evaluating existing objects as CREATE cannot give the server's answer
// for policy, or nil when the audit is sound.
func auditIssues(policy *admissionregv1.ValidatingAdmissionPolicy) []string {
	var issues []string
	if !matchesCreate(policy.Spec.MatchConstraints) {
		issues = append(issues, "matchConstraints does not match CREATE")
	}

	spec := &policy.Spec
	check := func(field, expr string) {
		if expr != "" {
			issues = append(issues, readsIssues(celEnv, field, expr)...)
		}
	}

	for i, v := range spec.Validations {
		check(fmt.Sprintf("validations[%d].expression", i), v.Expression)
		check(fmt.Sprintf("validations[%d].messageExpression", i), v.MessageExpression)
	}

	for i, a := range spec.AuditAnnotations {
		check(fmt.Sprintf("auditAnnotations[%d].valueExpression", i), a.ValueExpression)
	}

	for i, m := range spec.MatchConditions {
		check(fmt.Sprintf("matchConditions[%d].expression", i), m.Expression)
	}

	for i, v := range spec.Variables {
		check(fmt.Sprintf("variables[%d].expression", i), v.Expression)
	}

	return issues
}

// matchesCreate reports whether some resource rule in mc matches CREATE.
func matchesCreate(mc *admissionregv1.MatchResources) bool {
	if mc == nil {
		return false
	}

	return slices.ContainsFunc(mc.ResourceRules, func(r admissionregv1.NamedRuleWithOperations) bool {
		return slices.ContainsFunc(r.Operations, func(op admissionregv1.OperationType) bool {
			return op == admissionregv1.Create || op == admissionregv1.OperationAll
		})
	})
}

// readsIssues returns one issue per distinct name expr reads that the audit cannot supply. An expr
// that does not parse is a single issue, since what it reads is unknown.
func readsIssues(env *cel.Env, field, expr string) []string {
	parsed, iss := env.Parse(expr)
	if err := iss.Err(); err != nil {
		return []string{field + " cannot be checked: " + err.Error()}
	}

	r := &reads{selected: map[int64]bool{}}
	ast.PostOrderVisit(parsed.NativeRep().Expr(), ast.NewExprVisitor(r.visit))

	// Any other use, such as binding request to a name or a computed key, could reach any field.
	if slices.ContainsFunc(r.requests, func(id int64) bool { return !r.selected[id] }) {
		r.add("request")
	}

	issues := make([]string, 0, len(r.names))
	for _, name := range r.names {
		issues = append(issues, field+" reads "+name)
	}

	return issues
}

// reads collects what an expression reads that the audit cannot supply.
type reads struct {
	names    []string       // distinct, in the order first read
	requests []int64        // every read of request
	selected map[int64]bool // the reads of request that select a named field
}

func (r *reads) add(name string) {
	if !slices.Contains(r.names, name) {
		r.names = append(r.names, name)
	}
}

func (r *reads) visit(e ast.Expr) {
	if e.Kind() == ast.IdentKind {
		switch e.AsIdent() {
		case "oldObject", "authorizer":
			r.add(e.AsIdent())
		case "request":
			r.requests = append(r.requests, e.ID())
		}

		return
	}

	operand, name := selection(e)
	if operand == nil || operand.Kind() != ast.IdentKind || operand.AsIdent() != "request" {
		return
	}

	r.selected[operand.ID()] = true

	// The audit's request is fixed: user, operation, dryRun, and options differ from a real admission.
	if slices.Contains([]string{"userInfo", "operation", "dryRun", "options"}, name) {
		r.add("request." + name)
	}
}

// selection returns the operand and field name of e when e selects a named field: a.b, a.?b, a['b'] or
// a[?'b']. Otherwise it returns nil and "".
func selection(e ast.Expr) (ast.Expr, string) {
	if e.Kind() == ast.SelectKind {
		sel := e.AsSelect()

		return sel.Operand(), sel.FieldName()
	}

	if e.Kind() != ast.CallKind {
		return nil, ""
	}

	call := e.AsCall()
	if fn := call.FunctionName(); !slices.Contains([]string{operators.Index, operators.OptSelect, operators.OptIndex}, fn) ||
		call.Args()[1].Kind() != ast.LiteralKind {
		return nil, ""
	}

	name, ok := call.Args()[1].AsLiteral().Value().(string)
	if !ok {
		return nil, ""
	}

	return call.Args()[0], name
}

// discoveryBlocks returns an error naming a failed group that a rule could match, or nil when every
// failed group is outside the policy's rules.
func discoveryBlocks(rules []admissionregv1.NamedRuleWithOperations, failed map[schema.GroupVersion]error) error {
	for gv, err := range failed {
		for _, r := range rules {
			if slices.Contains(r.APIGroups, gv.Group) || slices.Contains(r.APIGroups, "*") {
				return fmt.Errorf("discovery of group %s failed: %w", gv, err)
			}
		}
	}

	return nil
}

// watched is a resource the audit lists and watches.
type watched struct {
	GVR        schema.GroupVersionResource
	GVK        schema.GroupVersionKind
	Namespaced bool
}

// groupVersions maps each group to its versions, the preferred one first.
func groupVersions(groups []*metav1.APIGroup) map[string][]string {
	out := make(map[string][]string, len(groups))
	for _, g := range groups {
		versions := []string{g.PreferredVersion.Version}
		for _, v := range g.Versions {
			versions = append(versions, v.Version)
		}

		out[g.Name] = versions
	}

	return out
}

// servedResources returns the watchable versions of each resource in lists, and the resources in discovery order.
func servedResources(lists []*metav1.APIResourceList) ([]schema.GroupResource, map[schema.GroupResource][]watched) {
	var keys []schema.GroupResource

	served := map[schema.GroupResource][]watched{}

	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}

		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || !slices.Contains(r.Verbs, "watch") {
				continue
			}

			w := watched{GVR: gv.WithResource(r.Name), GVK: gv.WithKind(r.Kind), Namespaced: r.Namespaced}
			gr := w.GVR.GroupResource()

			if len(served[gr]) == 0 {
				keys = append(keys, gr)
			}

			served[gr] = append(served[gr], w)
		}
	}

	return keys, served
}

// watchedResources returns the resources that some rule matches, each at the version the rule takes (see
// ruleVersion).
func watchedResources(rules []admissionregv1.NamedRuleWithOperations, groups []*metav1.APIGroup, lists []*metav1.APIResourceList) []watched {
	order := groupVersions(groups)
	keys, served := servedResources(lists)
	found := map[schema.GroupVersionResource]watched{}

	for _, rule := range rules {
		for _, gr := range keys {
			cands := served[gr]
			if !matchesResource(rule, cands[0]) {
				continue
			}

			if w, ok := ruleVersion(rule, cands, order[gr.Group]); ok {
				found[w.GVR] = w
			}
		}
	}

	out := slices.Collect(maps.Values(found))
	slices.SortFunc(out, func(a, b watched) int { return strings.Compare(a.GVR.String(), b.GVR.String()) })

	return out
}

// ruleVersion returns the candidate of cands that rule takes. A rule with "*" or no versions takes the first
// version in order that cands serve, or the first candidate. Otherwise it takes the first listed version
// that cands serve, and returns false when none is served: the API server maps a request only onto served
// versions, so the rule matches nothing there.
func ruleVersion(rule admissionregv1.NamedRuleWithOperations, cands []watched, order []string) (watched, bool) {
	versions := rule.APIVersions

	wildcard := len(versions) == 0 || slices.Contains(versions, "*")
	if wildcard {
		versions = order
	}

	for _, v := range versions {
		if i := slices.IndexFunc(cands, func(w watched) bool { return w.GVR.Version == v }); i >= 0 {
			return cands[i], true
		}
	}

	if wildcard {
		return cands[0], true
	}

	return watched{}, false
}

// matchesResource reports whether rule names w's group, resource, and scope. Versions are left to ruleVersion.
//
// ponytail: matchPolicy Exact is treated as Equivalent. Ceiling: objects written through a version or group
// the rule does not name are audited though admission skips them, which can only add exemptions. Upgrade
// path: report Exact policies as not auditable.
func matchesResource(rule admissionregv1.NamedRuleWithOperations, w watched) bool {
	groupOK := slices.Contains(rule.APIGroups, w.GVR.Group) || slices.Contains(rule.APIGroups, "*")
	resourceOK := slices.Contains(rule.Resources, w.GVR.Resource) ||
		slices.Contains(rule.Resources, "*") || slices.Contains(rule.Resources, "*/*")
	// A nil scope or "*" matches both; otherwise the scope must equal the resource's scope.
	scopeOK := rule.Scope == nil || *rule.Scope == admissionregv1.AllScopes ||
		(*rule.Scope == admissionregv1.NamespacedScope) == w.Namespaced

	return groupOK && resourceOK && scopeOK
}
