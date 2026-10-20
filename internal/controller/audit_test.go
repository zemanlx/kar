package controller

import (
	"slices"
	"testing"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// matchRules matches pods with the given operations.
func matchRules(ops ...admissionregv1.OperationType) *admissionregv1.MatchResources {
	return &admissionregv1.MatchResources{ResourceRules: []admissionregv1.NamedRuleWithOperations{{
		Operations: ops, APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"},
	}}}
}

func TestAuditIssues(t *testing.T) {
	t.Parallel()

	createRules := matchRules(admissionregv1.Create)
	bad := []string{"matchConstraints does not match CREATE"}

	tests := []struct {
		name string
		spec admissionregv1.ValidatingAdmissionPolicySpec
		want []string
	}{
		{name: "auditable", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "object.spec.replicas > 0", MessageExpression: "'replicas: ' + string(object.spec.replicas)"}}}},
		{name: "wildcard operation", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: matchRules(admissionregv1.OperationAll)}},
		{name: "update only", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: matchRules(admissionregv1.Update)}, want: bad},
		{name: "no match constraints", want: bad},
		{name: "unparsable expression fails closed", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "object.("}}}, want: []string{"validations[0].expression cannot be checked: ERROR: <input>:1:8: Syntax error: no viable alternative at input '.('\n | object.(\n | .......^"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := auditIssues(&admissionregv1.ValidatingAdmissionPolicy{Spec: tt.spec})
			if !slices.Equal(got, tt.want) {
				t.Errorf("auditIssues() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuditReads(t *testing.T) {
	t.Parallel()

	createRules := matchRules(admissionregv1.Create)

	tests := []struct {
		name string
		spec admissionregv1.ValidatingAdmissionPolicySpec
		want []string
	}{
		{name: "oldObject in a validation", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "object.spec.x == oldObject.spec.x"}}}, want: []string{"validations[0].expression reads oldObject"}},
		{name: "oldObject once per field", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "oldObject.a == oldObject.b", MessageExpression: "string(oldObject.a)"}}}, want: []string{"validations[0].expression reads oldObject", "validations[0].messageExpression reads oldObject"}},
		{name: "request.userInfo in a variable", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Variables: []admissionregv1.Variable{{Name: "user", Expression: "request.userInfo.username"}}}, want: []string{"variables[0].expression reads request.userInfo"}},
		{name: "request operation and dryRun", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request.dryRun || request.operation == 'CREATE'"}}}, want: []string{"validations[0].expression reads request.dryRun", "validations[0].expression reads request.operation"}},
		{name: "authorizer in a match condition", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, MatchConditions: []admissionregv1.MatchCondition{{Name: "a", Expression: "authorizer.group('').resource('pods').check('get').allowed()"}}}, want: []string{"matchConditions[0].expression reads authorizer"}},
		{name: "oldObject in an audit annotation", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, AuditAnnotations: []admissionregv1.AuditAnnotation{{Key: "k", ValueExpression: "oldObject.metadata.name"}}}, want: []string{"auditAnnotations[0].valueExpression reads oldObject"}},
		{name: "other request fields and an oldObject field are fine", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request.resource.resource == 'pods' && object.oldObject == null"}}}},
		{name: "optional select on oldObject", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "oldObject.?spec.replicas.orValue(0) > 1"}}}, want: []string{"validations[0].expression reads oldObject"}},
		{name: "optional syntax in another operand", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request.userInfo.username == 'x' && object.?metadata.?labels.?team.hasValue()"}}}, want: []string{"validations[0].expression reads request.userInfo"}},
		{name: "optional select and index on request", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request.?userInfo.username == 'x' || request[?'dryRun'].orValue(false)"}}}, want: []string{"validations[0].expression reads request.userInfo", "validations[0].expression reads request.dryRun"}},
		{name: "cel.bind on request.userInfo", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "cel.bind(u, request.userInfo, u.username == 'x')"}}}, want: []string{"validations[0].expression reads request.userInfo"}},
		{name: "two-variable comprehension reads request.userInfo", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "object.spec.containers.all(i, c, c.name != request.userInfo.username)"}}}, want: []string{"validations[0].expression reads request.userInfo"}},
		{name: "authorizer check in a validation", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "authorizer.group('apps').resource('deployments').check('list').allowed()"}}}, want: []string{"validations[0].expression reads authorizer"}},
		{name: "index on request", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request['userInfo'].username == 'x' && request['name'] != ''"}}}, want: []string{"validations[0].expression reads request.userInfo"}},
		{name: "request bound to a name", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "cel.bind(r, request, r.userInfo.username == 'x')"}}}, want: []string{"validations[0].expression reads request"}},
		{name: "request in a list", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "[request].all(r, r.userInfo.username != 'x')"}}}, want: []string{"validations[0].expression reads request"}},
		{name: "index on request with a computed key", spec: admissionregv1.ValidatingAdmissionPolicySpec{MatchConstraints: createRules, Validations: []admissionregv1.Validation{{Expression: "request['user' + 'Info'].username == 'x'"}}}, want: []string{"validations[0].expression reads request"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := auditIssues(&admissionregv1.ValidatingAdmissionPolicy{Spec: tt.spec})
			if !slices.Equal(got, tt.want) {
				t.Errorf("auditIssues() = %q, want %q", got, tt.want)
			}
		})
	}
}

// res builds the watched resource for one discovery entry.
func res(group, version, resource, kind string, namespaced bool) watched {
	return watched{
		GVR:        schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
		GVK:        schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
		Namespaced: namespaced,
	}
}

// testDiscovery is a small discovery result covering a subresource, a cluster-scoped resource,
// a resource without the watch verb, and a non-core group.
func testDiscovery() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
			{Name: "pods/status", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
			{Name: "namespaces", Kind: "Namespace", Verbs: []string{"get", "list", "watch"}},
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: []string{"get", "list", "create"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"list", "watch"}},
		}},
	}
}

// namedRules wraps rules as a policy's resource rules.
func namedRules(rules ...admissionregv1.Rule) []admissionregv1.NamedRuleWithOperations {
	out := make([]admissionregv1.NamedRuleWithOperations, 0, len(rules))

	for _, r := range rules {
		out = append(out, admissionregv1.NamedRuleWithOperations{Rule: r})
	}

	return out
}

func TestDiscoveryBlocks(t *testing.T) {
	t.Parallel()

	failed := map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: apierrors.NewServiceUnavailable("unavailable"),
	}
	star := []string{"*"}

	tests := []struct {
		name    string
		rules   []admissionregv1.NamedRuleWithOperations
		wantErr bool
	}{
		{name: "unrelated group", rules: namedRules(admissionregv1.Rule{APIGroups: []string{""}, Resources: star})},
		{name: "failed group named", rules: namedRules(admissionregv1.Rule{APIGroups: []string{"metrics.k8s.io"}, Resources: star}), wantErr: true},
		{name: "star group", rules: namedRules(admissionregv1.Rule{APIGroups: star, Resources: star}), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := discoveryBlocks(tt.rules, failed); (err != nil) != tt.wantErr {
				t.Errorf("discoveryBlocks() = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestWatchedResources(t *testing.T) {
	t.Parallel()

	pods := res("", "v1", "pods", "Pod", true)
	namespaces := res("", "v1", "namespaces", "Namespace", false)
	deployments := res("apps", "v1", "deployments", "Deployment", true)

	star := []string{"*"}
	scope := func(s admissionregv1.ScopeType) *admissionregv1.ScopeType { return &s }

	tests := []struct {
		name  string
		rules []admissionregv1.NamedRuleWithOperations
		want  []watched
	}{
		{name: "core pods and apps deployments", rules: namedRules(admissionregv1.Rule{APIGroups: []string{"", "apps"}, Resources: []string{"pods", "deployments"}}), want: []watched{pods, deployments}},
		{name: "star group and star resource", rules: namedRules(admissionregv1.Rule{APIGroups: star, Resources: star}), want: []watched{namespaces, pods, deployments}},
		{name: "star group and star slash star resource", rules: namedRules(admissionregv1.Rule{APIGroups: star, Resources: []string{"*/*"}}), want: []watched{namespaces, pods, deployments}},
		{name: "subresource rule matches nothing", rules: namedRules(admissionregv1.Rule{APIGroups: []string{""}, Resources: []string{"pods/status"}})},
		{name: "cluster scope excludes namespaced", rules: namedRules(admissionregv1.Rule{APIGroups: star, Resources: star, Scope: scope(admissionregv1.ClusterScope)}), want: []watched{namespaces}},
		{name: "namespaced scope excludes cluster", rules: namedRules(admissionregv1.Rule{APIGroups: star, Resources: star, Scope: scope(admissionregv1.NamespacedScope)}), want: []watched{pods, deployments}},
		{name: "resource without watch verb is excluded", rules: namedRules(admissionregv1.Rule{APIGroups: []string{""}, Resources: []string{"configmaps"}})},
		{name: "overlapping rules yield one entry", rules: namedRules(admissionregv1.Rule{APIGroups: []string{""}, Resources: []string{"pods"}}, admissionregv1.Rule{APIGroups: star, Resources: []string{"pods"}}), want: []watched{pods}},
		{name: "no rule matches", rules: namedRules(admissionregv1.Rule{APIGroups: []string{"batch"}, Resources: star})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := watchedResources(tt.rules, testGroups(), testDiscovery()); !slices.Equal(got, tt.want) {
				t.Errorf("watchedResources() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// apiGroup returns an API group serving versions, the first of which is preferred.
func apiGroup(name string, versions ...string) *metav1.APIGroup {
	g := &metav1.APIGroup{Name: name, PreferredVersion: metav1.GroupVersionForDiscovery{Version: versions[0]}}
	for _, v := range versions {
		g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{Version: v})
	}

	return g
}

// testGroups describes the groups of testDiscovery.
func testGroups() []*metav1.APIGroup {
	return []*metav1.APIGroup{apiGroup("", "v1"), apiGroup("apps", "v1")}
}

// versionDiscovery serves kar.test widgets at v1 (preferred) and v1beta1, and gadgets at v1beta1 only.
func versionDiscovery() ([]*metav1.APIGroup, []*metav1.APIResourceList) {
	groups := []*metav1.APIGroup{apiGroup("kar.test", "v1", "v1beta1")}
	lists := []*metav1.APIResourceList{
		{GroupVersion: "kar.test/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"list", "watch"}},
		}},
		{GroupVersion: "kar.test/v1beta1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"list", "watch"}},
			{Name: "gadgets", Kind: "Gadget", Namespaced: true, Verbs: []string{"list", "watch"}},
		}},
	}

	return groups, lists
}

func TestWatchedResourcesVersions(t *testing.T) {
	t.Parallel()

	widgetsV1 := res("kar.test", "v1", "widgets", "Widget", true)
	widgetsV1beta1 := res("kar.test", "v1beta1", "widgets", "Widget", true)
	gadgetsV1beta1 := res("kar.test", "v1beta1", "gadgets", "Gadget", true)

	kar := []string{"kar.test"}
	star := []string{"*"}

	tests := []struct {
		name  string
		rules []admissionregv1.NamedRuleWithOperations
		want  []watched
	}{
		{name: "pinned non-preferred version", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: []string{"v1beta1"}, Resources: []string{"widgets"}}), want: []watched{widgetsV1beta1}},
		{name: "wildcard takes the preferred version", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: star, Resources: []string{"widgets"}}), want: []watched{widgetsV1}},
		{name: "wildcard falls back to the first served version", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: star, Resources: []string{"gadgets"}}), want: []watched{gadgetsV1beta1}},
		{name: "first listed served version wins", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: []string{"v9", "v1beta1", "v1"}, Resources: []string{"widgets"}}), want: []watched{widgetsV1beta1}},
		// The API server only maps a request onto served versions, so an unserved listed version matches nothing.
		{name: "listed versions not served", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: []string{"v9"}, Resources: []string{"widgets"}})},
		{name: "a version some resources lack", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: []string{"v1"}, Resources: star}), want: []watched{widgetsV1}},
		{name: "resource absent in every version", rules: namedRules(admissionregv1.Rule{APIGroups: kar, APIVersions: []string{"v9"}, Resources: []string{"sprockets"}})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			groups, lists := versionDiscovery()

			if got := watchedResources(tt.rules, groups, lists); !slices.Equal(got, tt.want) {
				t.Errorf("watchedResources() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
