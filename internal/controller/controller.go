package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrlsource "sigs.k8s.io/controller-runtime/pkg/source"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
	"github.com/zemanlx/kat/evaluator"
)

const (
	resync = time.Minute
	// informerSyncTimeout bounds the wait for the params informer, so a params kind kar cannot list
	// (RBAC forbidden, an unavailable aggregated API) fails the reconcile instead of blocking it.
	// ponytail: a slow first list longer than this retries with backoff, and the single worker waits meanwhile.
	informerSyncTimeout = 30 * time.Second
	// syncPoll is how soon a pass runs again while a watched resource's informer is still syncing.
	syncPoll    = 2 * time.Second
	eventBuffer = 1024
)

// errNotKar is returned when the derived binding's name is taken by a binding kar did not write.
var errNotKar = errors.New("exists and was not written by kar")

// Reconciler audits the opted-in ValidatingAdmissionPolicyBindings and keeps each one's derived
// binding and PolicyExemption in step with the audit.
type Reconciler struct {
	Client     client.Client
	Cache      cache.Cache
	Discovery  discovery.CachedDiscoveryInterface
	Evaluator  *evaluator.Evaluator
	DefaultTTL time.Duration

	dynamic   dynamic.Interface // reads the watched resources; built in SetupWithManager
	apiReader client.Reader     // reads a Namespace the cache has not seen yet; set in SetupWithManager

	mu         sync.Mutex
	auditors   map[string]*auditor // by source binding name
	watches    map[schema.GroupVersionResource]*gvrSlot
	paramKinds map[schema.GroupVersionKind]bool // params kinds whose informer has the change handler
	events     chan event.GenericEvent
	relabeled  workqueue.TypedRateLimitingInterface[string] // namespaces whose labels changed, for reaudit
	// discovered is when Discovery was last invalidated. Reconcile runs one at a time, so it needs no lock.
	discovered time.Time
}

// auditor is the audit state of one source binding. mu guards source through inputs. state is changed only
// by the source's reconcile, which never runs concurrently, under mu. seenMu guards pending and exempt, so
// violations never wait for a pass.
type auditor struct {
	mu       sync.Mutex
	source   *admissionregv1.ValidatingAdmissionPolicyBinding
	policy   *admissionregv1.ValidatingAdmissionPolicy
	compiled *evaluator.CompiledValidating
	watched  []watched
	ttl      time.Duration
	inputs   string // the UIDs and generations of the policy and the source, see Reconciler.fingerprint
	state    exemptions

	seenMu  sync.Mutex
	pending map[string]time.Time // namespaces noted since the last pass, with the latest time
	exempt  map[string]struct{}  // the namespaces of state at the last pass; nil until state is loaded
}

// drain applies the namespaces noted since the last pass to a.state, at the times they were noted.
// The caller holds a.mu.
func (a *auditor) drain() {
	a.seenMu.Lock()
	pending := a.pending
	a.pending = map[string]time.Time{}
	a.seenMu.Unlock()

	for ns, at := range pending {
		a.state.see(ns, at, a.ttl)
	}
}

// snapshot publishes the namespaces of a.state for noteViolation. The caller holds a.mu.
func (a *auditor) snapshot() {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()

	a.exempt = make(map[string]struct{}, len(a.state))
	for ns := range a.state {
		a.exempt[ns] = struct{}{}
	}
}

// SetupWithManager registers the controller. Call it once.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	d, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	r.dynamic = d
	r.apiReader = mgr.GetAPIReader()
	r.auditors = map[string]*auditor{}
	r.watches = map[schema.GroupVersionResource]*gvrSlot{}
	r.paramKinds = map[schema.GroupVersionKind]bool{}
	r.events = make(chan event.GenericEvent, eventBuffer)
	r.relabeled = workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "kar-relabel"})

	// A RunnableFunc needs leader election, so the worker runs on the leader only.
	if err := mgr.Add(manager.RunnableFunc(r.relabelWorker)); err != nil {
		return fmt.Errorf("add relabel worker: %w", err)
	}

	registerMetrics()

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("kar").
		For(&admissionregv1.ValidatingAdmissionPolicyBinding{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return isSource(e.Object) },
			// An update that opts a binding out must still reach Reconcile, to clean up.
			UpdateFunc: func(e event.UpdateEvent) bool { return isSource(e.ObjectOld) || isSource(e.ObjectNew) },
			DeleteFunc: func(e event.DeleteEvent) bool { return isSource(e.Object) },
		})).
		Watches(&admissionregv1.ValidatingAdmissionPolicy{}, handler.EnqueueRequestsFromMapFunc(r.sourcesOf)).
		Watches(&corev1.Namespace{}, r.namespaceHandler()).
		WatchesRawSource(ctrlsource.Channel(r.events, &handler.EnqueueRequestForObject{})).
		Complete(r); err != nil {
		return fmt.Errorf("register controller: %w", err)
	}

	return nil
}

// Reconcile audits one source binding, then writes its status and derived binding.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	source := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := r.Client.Get(ctx, req.NamespacedName, source); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get binding: %w", err)
		}
		// Owner references garbage-collect the derived binding and the PolicyExemption.
		r.forget(req.Name)

		return ctrl.Result{}, nil
	}

	if !isSource(source) {
		r.forget(source.Name)

		return ctrl.Result{}, r.cleanup(ctx, source.Name)
	}

	pe, err := r.exemption(ctx, source)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.sync(ctx, source, pe)
}

// sourcesOf maps a ValidatingAdmissionPolicy to the opted-in bindings that name it.
func (r *Reconciler) sourcesOf(ctx context.Context, p client.Object) []reconcile.Request {
	var bindings admissionregv1.ValidatingAdmissionPolicyBindingList
	if err := r.Client.List(ctx, &bindings); err != nil {
		// The periodic resync catches up.
		log.FromContext(ctx).Error(err, "list bindings for policy")

		return nil
	}

	var reqs []reconcile.Request

	for i := range bindings.Items {
		b := &bindings.Items[i]
		if b.Spec.PolicyName == p.GetName() && isSource(b) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
		}
	}

	return reqs
}

// exemption returns source's PolicyExemption, creating it owned by source when absent.
func (r *Reconciler) exemption(ctx context.Context, source *admissionregv1.ValidatingAdmissionPolicyBinding) (*karv1alpha1.PolicyExemption, error) {
	pe := &karv1alpha1.PolicyExemption{}

	err := r.Client.Get(ctx, client.ObjectKey{Name: source.Name}, pe)
	if err == nil {
		return pe, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get policy exemption: %w", err)
	}

	pe = &karv1alpha1.PolicyExemption{Name: source.Name}
	if err := controllerutil.SetControllerReference(source, pe, r.Client.Scheme()); err != nil {
		return nil, fmt.Errorf("own policy exemption: %w", err)
	}

	if err := r.Client.Create(ctx, pe); err != nil {
		return nil, fmt.Errorf("create policy exemption: %w", err)
	}

	return pe, nil
}

// pass is one sync of a source binding. It writes the PolicyExemption status only when it changed.
type pass struct {
	r      *Reconciler
	source *admissionregv1.ValidatingAdmissionPolicyBinding
	pe     *karv1alpha1.PolicyExemption
	saved  *karv1alpha1.PolicyExemption // the status as last read or written
}

// checked is a source whose policy can be audited, with its watched resources.
type checked struct {
	source   *admissionregv1.ValidatingAdmissionPolicyBinding
	policy   *admissionregv1.ValidatingAdmissionPolicy
	compiled *evaluator.CompiledValidating
	watched  []watched
}

// sync audits source, writes its status, then applies or removes the derived binding. The status
// goes first, so a restart recovers at least what the binding lists.
func (r *Reconciler) sync(ctx context.Context, source *admissionregv1.ValidatingAdmissionPolicyBinding, pe *karv1alpha1.PolicyExemption) (ctrl.Result, error) {
	p := &pass{r: r, source: source, pe: pe, saved: pe.DeepCopy()}

	// Observe must not enforce, so it drops the Deny binding before the checks and the informers below can return early.
	if mode(source) == modeObserve {
		if err := r.remove(ctx, source.Name); err != nil {
			setCondition(pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonApplyFailed, err.Error())

			return ctrl.Result{}, errors.Join(err, p.save(ctx))
		}

		setCondition(pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonObserve, "observe mode: no derived binding")
	}

	c, res, err := p.check(ctx)
	if c == nil {
		return res, err
	}

	return p.publish(ctx, c)
}

// check decides whether the source can be audited and starts its informers. When it cannot, it
// returns nil and the result to return.
func (p *pass) check(ctx context.Context) (*checked, ctrl.Result, error) {
	policy := &admissionregv1.ValidatingAdmissionPolicy{}
	if err := p.r.Client.Get(ctx, client.ObjectKey{Name: p.source.Spec.PolicyName}, policy); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, ctrl.Result{}, fmt.Errorf("get policy: %w", err)
		}

		msg := fmt.Sprintf("ValidatingAdmissionPolicy %q not found", p.source.Spec.PolicyName)
		p.r.forget(p.source.Name)
		setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionFalse, karv1alpha1.ReasonPolicyNotFound, msg)
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonPolicyNotFound, msg)

		return nil, ctrl.Result{RequeueAfter: resync}, errors.Join(p.r.remove(ctx, p.source.Name), p.save(ctx))
	}

	compiled, issues, err := p.r.compile(policy, p.source)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	if len(issues) > 0 {
		return p.notAuditable(ctx, strings.Join(issues, "; "))
	}

	setCondition(p.pe, karv1alpha1.ConditionAuditable, metav1.ConditionTrue, karv1alpha1.ReasonAuditable, "the policy can be audited")

	if time.Since(p.r.discovered) > resync {
		p.r.Discovery.Invalidate()
		p.r.discovered = time.Now()
	}

	groups, lists, err := p.r.Discovery.ServerGroupsAndResources()
	if failed, ok := errors.AsType[*discovery.ErrGroupDiscoveryFailed](err); ok {
		// A partial result is usable unless a failed group could hold something the policy matches.
		err = discoveryBlocks(policy.Spec.MatchConstraints.ResourceRules, failed.Groups)
	}

	if err != nil {
		return nil, ctrl.Result{}, p.fail(ctx, karv1alpha1.ReasonDiscoveryFailed, err)
	}

	watch := watchedResources(policy.Spec.MatchConstraints.ResourceRules, groups, lists)

	if err := p.r.ensureParamsInformer(ctx, policy.Spec.ParamKind); err != nil {
		return nil, ctrl.Result{}, p.fail(ctx, karv1alpha1.ReasonInformerFailed, err)
	}

	return &checked{source: p.source, policy: policy, compiled: compiled, watched: watch}, ctrl.Result{}, nil
}

// notAuditable forgets the source's audit state, reports msg as why the policy cannot be audited, and
// removes the derived binding.
func (p *pass) notAuditable(ctx context.Context, msg string) (*checked, ctrl.Result, error) {
	p.r.forget(p.source.Name)
	setCondition(p.pe, karv1alpha1.ConditionAuditable, metav1.ConditionFalse, karv1alpha1.ReasonNotAuditable, msg)
	setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonNotAuditable, msg)

	return nil, ctrl.Result{RequeueAfter: resync}, errors.Join(p.r.remove(ctx, p.source.Name), p.save(ctx))
}

// publish audits the checked source under its auditor, writes the status, then binds.
func (p *pass) publish(ctx context.Context, c *checked) (ctrl.Result, error) {
	p.r.mu.Lock()

	a, ok := p.r.auditors[p.source.Name]
	if !ok {
		a = &auditor{pending: map[string]time.Time{}}
		p.r.auditors[p.source.Name] = a
	}

	p.r.mu.Unlock()

	now := time.Now()
	inputs := fmt.Sprintf("%s/%d/%s/%d", c.policy.UID, c.policy.Generation, p.source.UID, p.source.Generation)

	a.mu.Lock()
	a.source, a.policy, a.compiled, a.watched, a.ttl = p.source, c.policy, c.compiled, c.watched, ttlOf(p.source, p.r.DefaultTTL)
	a.inputs = inputs

	if a.exempt == nil {
		if err := p.loadState(ctx, a, now); err != nil {
			a.mu.Unlock()

			return ctrl.Result{}, p.fail(ctx, karv1alpha1.ReasonReadFailed, err)
		}
	}

	a.mu.Unlock()

	// No lock is held while waiting on informers: their transforms take a.mu.
	active, ready, pending, err := p.r.syncWatches(ctx, c.watched, p.source.Name+"="+inputs)
	if err != nil {
		return ctrl.Result{}, p.fail(ctx, karv1alpha1.ReasonInformerFailed, err)
	}

	if !ready {
		setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionFalse, karv1alpha1.ReasonInformerSyncing, "waiting for the initial list of watched resources")

		return ctrl.Result{RequeueAfter: syncPoll}, p.save(ctx)
	}

	// Only this pass changes a.state, so the API writes below run without a.mu, and violations do not wait for them.
	a.mu.Lock()
	counts := refresh(ctx, a, active, now)

	a.state.expire(now, func(ns string) bool { return p.r.namespaceExists(ctx, ns) })
	a.snapshot()
	a.mu.Unlock()

	recordPass(p.source.Name, counts, len(a.state))

	latched, err := p.latched(ctx, c.policy)
	if err != nil {
		return ctrl.Result{}, p.fail(ctx, karv1alpha1.ReasonReadFailed, err)
	}

	p.clock(inputs, latched, now)

	res, err := p.writeAndBind(ctx, a, c.policy.UID, latched, now)

	if pending && res.RequeueAfter > syncPoll {
		res.RequeueAfter = syncPoll // pick up the swap soon
	}

	return res, err
}

// writeAndBind writes the status, then binds the mode the binding is in now. An auto binding still observing
// wakes at its deadline, so it does not wait for the next resync.
func (p *pass) writeAndBind(ctx context.Context, a *auditor, uid types.UID, latched bool, now time.Time) (ctrl.Result, error) {
	stored, err := p.writeState(ctx, a)
	if err != nil {
		return ctrl.Result{}, err
	}

	var since time.Time
	if p.pe.Status.ObservingSince != nil {
		since = p.pe.Status.ObservingSince.Time
	}

	deadline := since.Add(a.ttl)
	eff := effectiveMode(mode(p.source), since, a.ttl, latched, now)

	res := ctrl.Result{RequeueAfter: resync}
	if stored {
		res, err = p.bindAndReport(ctx, uid, a.state.names(), eff, deadline)
	}

	// A zero RequeueAfter means no requeue, so the wait is at least a second.
	if mode(p.source) == modeAuto && eff == modeObserve && !since.IsZero() {
		res.RequeueAfter = min(res.RequeueAfter, max(time.Until(deadline), time.Second))
	}

	return res, err
}

// latched reports whether kar has enforced p's binding under policy. A Deny binding left by an earlier policy
// or source binding of the same name does not count: the garbage collector removes the latter only some time
// after its owner is gone, and a reset clock can be saved before the former is removed.
func (p *pass) latched(ctx context.Context, policy *admissionregv1.ValidatingAdmissionPolicy) (bool, error) {
	derived, err := p.r.derived(ctx, p.source.Name)
	if err != nil {
		return false, err
	}

	if !owned(derived, p.source.Name) || metav1.GetControllerOf(derived).UID != p.source.UID {
		return false, nil
	}

	uid, ok := derived.Annotations[policyUIDAnnotation]
	if !ok {
		// ponytail: a Deny binding from a kar that did not record the policy UID counts when it is not older
		// than the policy. Ceiling: a policy recreated within a second of it. Upgrade path: drop this once
		// every supported kar records the UID.
		return !derived.CreationTimestamp.Before(&policy.CreationTimestamp), nil
	}

	return uid == string(policy.UID), nil
}

// clock keeps the observation window of a binding. The window restarts when the inputs change and is cleared
// under enforce. A latched auto binding enforces regardless, so it keeps the window as it is.
func (p *pass) clock(inputs string, latched bool, now time.Time) {
	st := &p.pe.Status

	switch m := mode(p.source); {
	case m == modeEnforce:
		st.ObservingSince, st.ObservedInputs = nil, ""
	case m == modeAuto && latched:
	case st.ObservingSince == nil || st.ObservedInputs != inputs:
		st.ObservingSince, st.ObservedInputs = new(metav1.NewTime(now)), inputs
	}
}

// loadState recovers a's exemptions after a restart, from the status and the derived binding.
func (p *pass) loadState(ctx context.Context, a *auditor, now time.Time) error {
	derived, err := p.r.derived(ctx, p.source.Name)
	if err != nil {
		return err
	}

	if !owned(derived, p.source.Name) {
		derived = nil
	}

	a.state = recoverState(p.pe.Status.Namespaces, derived, p.source, now, a.ttl)
	a.snapshot()

	return nil
}

// writeState records a's exemptions in the status. When the API server rejects the status as too large,
// it keeps the last exemptions that fit and reports false, so the Deny binding is not updated.
func (p *pass) writeState(ctx context.Context, a *auditor) (bool, error) {
	if needsWrite(p.saved.Status.Namespaces, a.state, a.ttl) {
		p.pe.Status.Namespaces = toStatus(a.state)
	}

	clocked := p.pe.Status.ObservingSince != nil

	// Synced is set after the save, so an outdated CRD that prunes the clock leaves the status as it was and the
	// API server skips the write.
	err := p.save(ctx)
	if err == nil {
		// An outdated CRD prunes the clock, which would otherwise restart on every pass and never enforce.
		if clocked && p.pe.Status.ObservingSince == nil {
			setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionFalse, karv1alpha1.ReasonCRDOutdated,
				"the PolicyExemption CRD is outdated and drops status.observingSince; apply the chart's CRDs")

			return false, p.save(ctx)
		}

		setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionTrue, karv1alpha1.ReasonSynced, "the exemptions match the audit")

		return true, p.save(ctx)
	}

	if !tooLarge(err) {
		return false, err
	}

	log.FromContext(ctx).Error(err, "exemption state too large", "binding", p.source.Name)

	p.pe.Status.Namespaces = p.saved.Status.Namespaces
	setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionFalse, karv1alpha1.ReasonStateTooLarge,
		fmt.Sprintf("the status is too large: %v", err))

	return false, p.save(ctx)
}

// tooLarge reports whether err rejects an object as too large: a 413 from the API server, or the 500s
// that etcd and gRPC return above their request limits.
func tooLarge(err error) bool {
	return apierrors.IsRequestEntityTooLargeError(err) ||
		strings.Contains(err.Error(), "request is too large") ||
		strings.Contains(err.Error(), "trying to send message larger than max")
}

// bindAndReport applies or removes the derived binding for the exempt namespaces in mode eff and records it.
// deadline is when an auto binding observing so far starts enforcing.
func (p *pass) bindAndReport(ctx context.Context, uid types.UID, exempt []string, eff string, deadline time.Time) (ctrl.Result, error) {
	if err := p.r.bind(ctx, p.source, uid, exempt, eff); err != nil {
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonApplyFailed, err.Error())

		return ctrl.Result{}, errors.Join(err, p.save(ctx))
	}

	switch {
	case eff == modeEnforce:
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionTrue, karv1alpha1.ReasonEnforcing, "the derived binding exempts the listed namespaces")
	case mode(p.source) == modeAuto && p.pe.Status.ObservingSince == nil:
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonObserve, "auto: the observation window has not started")
	case mode(p.source) == modeAuto:
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonObserve, "auto: enforcing after "+deadline.UTC().Format(time.RFC3339))
	default:
		setCondition(p.pe, karv1alpha1.ConditionEnforcing, metav1.ConditionFalse, karv1alpha1.ReasonObserve, "observe mode: no derived binding")
	}

	return ctrl.Result{RequeueAfter: resync}, p.save(ctx)
}

// save writes the status when it differs from the last read or write.
func (p *pass) save(ctx context.Context) error {
	if equality.Semantic.DeepEqual(p.pe.Status, p.saved.Status) {
		return nil
	}

	if err := p.r.Client.Status().Update(ctx, p.pe); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	p.saved = p.pe.DeepCopy()

	return nil
}

// fail sets Synced to false for reason, writes the status, and returns err with any write error.
func (p *pass) fail(ctx context.Context, reason string, err error) error {
	setCondition(p.pe, karv1alpha1.ConditionSynced, metav1.ConditionFalse, reason, err.Error())

	return errors.Join(err, p.save(ctx))
}

// compile compiles policy with source's binding as Deny. A policy the API server would reject is an
// issue rather than an error, so it is reported on the status.
func (r *Reconciler) compile(policy *admissionregv1.ValidatingAdmissionPolicy, source *admissionregv1.ValidatingAdmissionPolicyBinding) (*evaluator.CompiledValidating, []string, error) {
	issues := auditIssues(policy)

	compiled, err := r.Evaluator.CompileValidating(policy, denySemantics(source))
	if rejected, ok := errors.AsType[*evaluator.InvalidPolicyError](err); ok {
		return nil, append(issues, "the API server would reject the policy: "+rejected.Error()), nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("compile policy: %w", err)
	}

	return compiled, issues, nil
}

// ensureParamsInformer ensures the informer for the params kind, if any. It is synced here, so the
// first param read never blocks without a timeout.
func (r *Reconciler) ensureParamsInformer(ctx context.Context, pk *admissionregv1.ParamKind) error {
	if pk == nil {
		return nil
	}

	gvk, err := policyParamGVK(pk)
	if err != nil {
		return err
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	syncCtx, cancel := context.WithTimeout(ctx, informerSyncTimeout)
	defer cancel()

	inf, err := r.Cache.GetInformer(syncCtx, u)
	if err != nil {
		return fmt.Errorf("informer for params %s: %w", pk.Kind, err)
	}

	// The informer is shared and outlives the pass, so its change handler is added once per kind.
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.paramKinds[gvk] {
		return nil
	}

	if _, err := inf.AddEventHandler(r.paramsHandler(gvk)); err != nil {
		return fmt.Errorf("watch params %s: %w", pk.Kind, err)
	}

	r.paramKinds[gvk] = true

	return nil
}

// evaluate runs c's policy on obj as a CREATE and reports whether it denies, with the message.
func (r *Reconciler) evaluate(ctx context.Context, c checked, w watched, obj *unstructured.Unstructured) (bool, string, error) {
	req := evaluator.Request{Object: obj, Resource: w.GVR}

	if w.Namespaced {
		ns, err := r.namespaceObject(ctx, obj.GetNamespace())
		if err != nil {
			return false, "", fmt.Errorf("namespace %q: %w", obj.GetNamespace(), err)
		}

		req.Namespace = ns
	}

	params, err := r.params(ctx, c, obj)
	if err != nil {
		return false, "", err
	}

	req.Params = params

	res, err := c.compiled.Evaluate(ctx, req)
	if err != nil {
		return false, "", fmt.Errorf("evaluate: %w", err)
	}

	return !res.Allowed, res.Message, nil
}

// params resolves the params object for obj as the API server does. A missing params object yields
// none, so the policy's parameterNotFoundAction applies.
func (r *Reconciler) params(ctx context.Context, c checked, obj *unstructured.Unstructured) ([]runtime.Object, error) {
	ref := c.source.Spec.ParamRef
	if c.policy.Spec.ParamKind == nil || ref == nil {
		return nil, nil
	}

	gv, err := schema.ParseGroupVersion(c.policy.Spec.ParamKind.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("param kind: %w", err)
	}

	kind := gv.WithKind(c.policy.Spec.ParamKind.Kind)

	ns, err := r.paramNamespace(kind, ref.Namespace, obj)
	if err != nil {
		return nil, err
	}

	switch {
	case ref.Name != "":
		return r.paramByName(ctx, kind, ns, ref.Name)
	case ref.Selector != nil:
		return r.paramsBySelector(ctx, kind, ns, ref.Selector)
	}

	return nil, nil
}

// paramNamespace returns the namespace a namespaced params kind is read in: paramRef's, else the
// object's. A cluster-scoped kind has none, so it returns "".
func (r *Reconciler) paramNamespace(kind schema.GroupVersionKind, refNamespace string, obj *unstructured.Unstructured) (string, error) {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(kind)

	namespaced, err := r.Client.IsObjectNamespaced(probe)
	if err != nil {
		return "", fmt.Errorf("param kind scope: %w", err)
	}

	switch {
	case !namespaced:
		return "", nil
	case refNamespace != "":
		return refNamespace, nil
	}

	return obj.GetNamespace(), nil
}

// paramByName returns the params object named name, or none when it does not exist.
func (r *Reconciler) paramByName(ctx context.Context, kind schema.GroupVersionKind, ns, name string) ([]runtime.Object, error) {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(kind)

	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get params: %w", err)
	}

	return []runtime.Object{p}, nil
}

// paramsBySelector returns the params objects that match selector.
func (r *Reconciler) paramsBySelector(ctx context.Context, kind schema.GroupVersionKind, ns string, selector *metav1.LabelSelector) ([]runtime.Object, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("param selector: %w", err)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

	if err := r.Client.List(ctx, list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("list params: %w", err)
	}

	out := make([]runtime.Object, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}

	return out, nil
}

// namespaceObject returns the named Namespace as the evaluator takes it. It reads the cached typed
// object, so the audit does not call the API server per object. The Namespace informer is separate
// from the objects' informers, so a new namespace can be missing from the cache when its objects
// arrive; NotFound falls back to a live read.
// ponytail: a namespace that is gone costs one live GET per object. Ceiling: a burst of objects in a
// deleted namespace. Upgrade path: remember NotFound for the pass.
func (r *Reconciler) namespaceObject(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	key := client.ObjectKey{Name: name}
	ns := &corev1.Namespace{}

	err := r.Client.Get(ctx, key, ns)
	if apierrors.IsNotFound(err) {
		err = r.apiReader.Get(ctx, key, ns)
	}

	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}

	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ns)
	if err != nil {
		return nil, fmt.Errorf("convert namespace: %w", err)
	}

	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Namespace"))

	return u, nil
}

// namespaceExists reports whether the namespace exists. Any error other than NotFound counts as
// existing, so a transient failure does not drop an exemption.
func (r *Reconciler) namespaceExists(ctx context.Context, name string) bool {
	return !apierrors.IsNotFound(r.Client.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{}))
}

// bind applies the derived binding of the policy with UID uid when eff is enforce and removes it otherwise.
func (r *Reconciler) bind(ctx context.Context, source *admissionregv1.ValidatingAdmissionPolicyBinding, uid types.UID, exempt []string, eff string) error {
	if eff != modeEnforce {
		return r.remove(ctx, source.Name)
	}

	existing, err := r.derived(ctx, source.Name)
	if err != nil {
		return err
	}

	if existing != nil && !owned(existing, source.Name) {
		return fmt.Errorf("binding %q %w", derivedName(source.Name), errNotKar)
	}

	// Always apply: an unchanged apply writes nothing, and comparing with server defaults would not match anyway.
	return r.apply(ctx, derive(source, uid, exempt))
}

// apply server-side applies desired as field manager kar. Kar owns the derived binding, so it takes
// back any field another manager changed.
func (r *Reconciler) apply(ctx context.Context, desired *admissionregv1.ValidatingAdmissionPolicyBinding) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return fmt.Errorf("convert derived binding: %w", err)
	}

	u := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: m})
	if err := r.Client.Apply(ctx, u, client.FieldOwner("kar"), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply derived binding: %w", err)
	}

	return nil
}

// remove deletes the derived binding of name when kar wrote it.
func (r *Reconciler) remove(ctx context.Context, name string) error {
	b, err := r.derived(ctx, name)
	if err != nil || !owned(b, name) {
		return err
	}

	return r.deleteIfExists(ctx, b)
}

// cleanup removes what kar wrote for a binding that is gone or no longer opted in.
func (r *Reconciler) cleanup(ctx context.Context, name string) error {
	if err := r.remove(ctx, name); err != nil {
		return err
	}

	return r.deleteIfExists(ctx, &karv1alpha1.PolicyExemption{Name: name})
}

// deleteIfExists deletes obj, treating NotFound as done.
func (r *Reconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", obj.GetName(), err)
	}

	return nil
}

// derived returns the derived binding of name, or nil when there is none.
//
//nolint:nilnil // A nil binding with no error is the "absent" answer every caller checks for.
func (r *Reconciler) derived(ctx context.Context, name string) (*admissionregv1.ValidatingAdmissionPolicyBinding, error) {
	b := &admissionregv1.ValidatingAdmissionPolicyBinding{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: derivedName(name)}, b); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get derived binding: %w", err)
	}

	return b, nil
}

// forget drops the audit state of a binding that is gone or no longer auditable.
func (r *Reconciler) forget(name string) {
	r.mu.Lock()
	delete(r.auditors, name)

	var gws []*gvrWatch

	for _, slot := range r.watches {
		for _, gw := range []*gvrWatch{slot.active, slot.pending} {
			if gw != nil {
				gws = append(gws, gw)
			}
		}
	}
	r.mu.Unlock()

	for _, gw := range gws {
		gw.mu.Lock()
		delete(gw.violators, name)
		gw.mu.Unlock()
	}

	r.pruneWatches()
	forgetMetrics(name)
}

// isSource reports whether obj is a binding kar audits.
func isSource(obj client.Object) bool {
	b, ok := obj.(*admissionregv1.ValidatingAdmissionPolicyBinding)

	return ok && optedIn(b)
}

// optedIn reports whether b carries a valid kar.zeman.dev/mode. Derived bindings carry none, so they
// are never sources.
func optedIn(b *admissionregv1.ValidatingAdmissionPolicyBinding) bool {
	m := mode(b)

	return m == modeObserve || m == modeEnforce || m == modeAuto
}

// owned reports whether kar wrote b, the derived binding of source: b's controller is source.
func owned(b *admissionregv1.ValidatingAdmissionPolicyBinding, source string) bool {
	if b == nil {
		return false
	}

	c := metav1.GetControllerOf(b)

	return c != nil && c.APIVersion == admissionregv1.SchemeGroupVersion.String() &&
		c.Kind == "ValidatingAdmissionPolicyBinding" && c.Name == source
}

// setCondition sets one PolicyExemption condition. An unchanged status keeps its LastTransitionTime.
func setCondition(pe *karv1alpha1.PolicyExemption, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&pe.Status.Conditions, metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg})
}
