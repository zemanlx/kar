package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// deniedAnnotation is set only on kar's cached stubs: a JSON map of denying source binding name to the deny message.
const deniedAnnotation = "kar.zeman.dev/denied-by"

// gvrWatch is one informer over a watched resource and the violator index its handlers build.
type gvrWatch struct {
	w           watched
	fingerprint string // the auditors' inputs it was started with, see Reconciler.fingerprint
	started     time.Time
	cancel      context.CancelFunc
	synced      func() bool // the handler registration's HasSynced: the initial list reached the handlers
	store       toolscache.Store

	mu        sync.Mutex
	violators map[string]map[string]map[string]struct{} // source binding -> namespace ("" if cluster-scoped) -> object name
	watchErr  error                                     // the last list/watch error, for InformerFailed
}

// gvrSlot is the informer serving a resource and the one being built to replace it.
type gvrSlot struct {
	active, pending *gvrWatch
	dirty           bool // a params change needs a re-evaluation (debounced)
}

// startWatch starts an informer over w that evaluates each object as it arrives and indexes the
// violators. It does not wait for the initial list: a pass swaps the informer in once it has synced.
func (r *Reconciler) startWatch(ctx context.Context, w watched, fingerprint string) (*gvrWatch, error) {
	inf := dynamicinformer.NewFilteredDynamicInformer(r.dynamic, w.GVR, metav1.NamespaceAll, 0, toolscache.Indexers{}, nil).Informer()
	if err := inf.SetTransform(r.transform(ctx, w)); err != nil {
		return nil, fmt.Errorf("set transform: %w", err)
	}

	gw := &gvrWatch{
		w:           w,
		fingerprint: fingerprint,
		started:     time.Now(),
		store:       inf.GetStore(),
		violators:   map[string]map[string]map[string]struct{}{},
	}

	if err := inf.SetWatchErrorHandlerWithContext(func(hctx context.Context, _ *toolscache.Reflector, err error) {
		gw.mu.Lock()
		gw.watchErr = err
		gw.mu.Unlock()

		log.FromContext(hctx).V(1).Info("watch failed", "resource", w.GVR.String(), "error", err.Error())
	}); err != nil {
		return nil, fmt.Errorf("set watch error handler: %w", err)
	}

	reg, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.record(ctx, gw, obj) },
		UpdateFunc: func(_, obj any) { r.record(ctx, gw, obj) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}

			if stub, ok := obj.(*metav1.PartialObjectMetadata); ok {
				gw.mu.Lock()
				gw.unindexLocked(stub.Namespace, stub.Name)
				gw.mu.Unlock()
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("add event handler: %w", err)
	}

	gw.synced = reg.HasSynced

	wctx, cancel := context.WithCancel(ctx)
	gw.cancel = cancel

	go inf.RunWithContext(wctx)

	return gw, nil
}

// transform evaluates each object once, as the informer delivers it, and keeps only a stub that
// records the denials.
//
// ponytail: evaluation runs inside the transform, which client-go asks to be fast; the ceiling is
// about 1 ms per object on the reflector goroutine. Upgrade path: evaluate in a worker pool.
func (r *Reconciler) transform(ctx context.Context, w watched) toolscache.TransformFunc {
	return func(obj any) (any, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}

		stub, err := r.stubOf(ctx, w, u)
		if err != nil {
			return nil, err
		}

		return stub, nil
	}
}

// stubOf evaluates u against every auditor that watches w and returns the stub that records the denials.
func (r *Reconciler) stubOf(ctx context.Context, w watched, u *unstructured.Unstructured) (*metav1.PartialObjectMetadata, error) {
	denials := map[string]string{}

	for _, a := range r.snapshotAuditors() {
		if src, msg, denied := r.denial(ctx, a, w, u); denied {
			denials[src] = msg
		}
	}

	stub := &metav1.PartialObjectMetadata{}
	stub.SetGroupVersionKind(u.GroupVersionKind())
	stub.SetName(u.GetName())
	stub.SetNamespace(u.GetNamespace())
	stub.SetUID(u.GetUID())
	stub.SetResourceVersion(u.GetResourceVersion())

	if len(denials) > 0 {
		raw, err := json.Marshal(denials)
		if err != nil {
			return nil, fmt.Errorf("encode denials: %w", err)
		}

		stub.Annotations = map[string]string{deniedAnnotation: string(raw)}
	}

	return stub, nil
}

// denial reports whether a's policy denies u, with the source binding and the message. It skips an
// auditor that is not compiled or does not watch w. Evaluation runs without a.mu, on a copy: the pass
// replaces these fields and never changes them in place.
func (r *Reconciler) denial(ctx context.Context, a *auditor, w watched, u *unstructured.Unstructured) (string, string, bool) {
	a.mu.Lock()
	c := checked{source: a.source, policy: a.policy, compiled: a.compiled, watched: a.watched}
	a.mu.Unlock()

	if c.compiled == nil || !slices.ContainsFunc(c.watched, func(x watched) bool { return x.GVR == w.GVR }) {
		return "", "", false
	}

	denied, msg, err := r.evaluate(ctx, c, w, u)
	if err != nil {
		log.FromContext(ctx).V(1).Info("object not evaluated", "kind", w.GVK.Kind, "name", u.GetName(), "namespace", u.GetNamespace(), "error", err.Error())

		return "", "", false
	}

	return c.source.Name, msg, denied
}

// record replaces the object's entries in gw's index with the denials its stub records, then exempts
// the namespaces of new violations at once.
func (r *Reconciler) record(ctx context.Context, gw *gvrWatch, obj any) {
	r.recordIf(ctx, gw, obj, nil)
}

// recordIf is record when current, checked under gw.mu, reports true or is nil. The informer updates its
// store before it calls the handlers, so a newer version's record waits for gw.mu and wins.
func (r *Reconciler) recordIf(ctx context.Context, gw *gvrWatch, obj any, current func() bool) {
	stub, ok := obj.(*metav1.PartialObjectMetadata)
	if !ok {
		return
	}

	denials := deniedBy(ctx, stub)

	gw.mu.Lock()
	if current != nil && !current() {
		gw.mu.Unlock()

		return
	}

	gw.unindexLocked(stub.Namespace, stub.Name)

	for src := range denials {
		byNS := gw.violators[src]
		if byNS == nil {
			byNS = map[string]map[string]struct{}{}
			gw.violators[src] = byNS
		}

		names := byNS[stub.Namespace]
		if names == nil {
			names = map[string]struct{}{}
			byNS[stub.Namespace] = names
		}

		names[stub.Name] = struct{}{}
	}

	gw.mu.Unlock()

	for src, msg := range denials {
		r.noteViolation(ctx, gw.w, src, stub, msg)
	}
}

// deniedBy returns the denying sources and messages recorded on a stub.
func deniedBy(ctx context.Context, stub *metav1.PartialObjectMetadata) map[string]string {
	raw := stub.Annotations[deniedAnnotation]
	if raw == "" {
		return nil
	}

	var denials map[string]string
	if err := json.Unmarshal([]byte(raw), &denials); err != nil {
		log.FromContext(ctx).Error(err, "decode denials", "name", stub.Name)

		return nil
	}

	return denials
}

// unindexLocked drops the object's entries under every source. gw.mu must be held.
func (gw *gvrWatch) unindexLocked(ns, name string) {
	for src, byNS := range gw.violators {
		delete(byNS[ns], name)

		if len(byNS[ns]) == 0 {
			delete(byNS, ns)
		}

		if len(byNS) == 0 {
			delete(gw.violators, src)
		}
	}
}

// counts returns, per namespace ("" if cluster-scoped), the number of violating objects of source.
func (gw *gvrWatch) counts(source string) map[string]int {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	out := map[string]int{}
	for ns, names := range gw.violators[source] {
		out[ns] = len(names)
	}

	return out
}

// noteViolation queues the namespace of a violation for the next pass, so a short-lived violator is
// exempted at once. It takes only seenMu, so it does not wait for a pass.
func (r *Reconciler) noteViolation(ctx context.Context, w watched, source string, obj *metav1.PartialObjectMetadata, msg string) {
	r.mu.Lock()
	a, ok := r.auditors[source]
	r.mu.Unlock()

	if !ok {
		return
	}

	a.seenMu.Lock()
	defer a.seenMu.Unlock()

	// A failed loadState leaves watched set but no exemptions, so skip until a pass has loaded them.
	if a.exempt == nil {
		return
	}

	if !w.Namespaced {
		log.FromContext(ctx).V(1).Info("cluster-scoped violation", "binding", source, "kind", w.GVK.Kind, "name", obj.Name, "message", msg)

		return
	}

	// The latest time is kept, so the pass extends the exemption as each violation did.
	_, exempt := a.exempt[obj.Namespace]
	_, queued := a.pending[obj.Namespace]
	a.pending[obj.Namespace] = time.Now()

	if exempt || queued {
		return
	}

	log.FromContext(ctx).Info("exempting namespace", "binding", source, "namespace", obj.Namespace,
		"kind", w.GVK.Kind, "name", obj.Name, "message", msg)

	select {
	case r.events <- event.GenericEvent{Object: &admissionregv1.ValidatingAdmissionPolicyBinding{Name: source}}:
	default: // The next resync reconciles it.
	}
}

// refresh applies the namespaces noted since the last pass, then folds the violators of the active informers
// into a's state. It returns the denied object count per namespace, with cluster-scoped objects under "". A
// namespace with violators keeps its exemption alive. The caller holds a.mu.
func refresh(ctx context.Context, a *auditor, active []*gvrWatch, now time.Time) map[string]int {
	// The cap precedes drain: a noted sighting can be later than now and keeps its own window.
	a.state.capAt(now, a.ttl)
	a.drain()

	counts := map[string]int{}

	for _, gw := range active {
		for ns, n := range gw.counts(a.source.Name) {
			counts[ns] += n

			if !gw.w.Namespaced {
				continue
			}

			if a.state.see(ns, now, a.ttl) {
				log.FromContext(ctx).Info("exempting namespace", "binding", a.source.Name, "namespace", ns, "kind", gw.w.GVK.Kind)
			}
		}
	}

	return counts
}

// fingerprint names the auditors' inputs for gvr, so its informer restarts when one changes.
func (r *Reconciler) fingerprint(gvr schema.GroupVersionResource) string {
	var parts []string

	for _, a := range r.snapshotAuditors() {
		a.mu.Lock()
		if a.compiled != nil && slices.ContainsFunc(a.watched, func(w watched) bool { return w.GVR == gvr }) {
			parts = append(parts, a.source.Name+"="+a.inputs)
		}
		a.mu.Unlock()
	}

	slices.Sort(parts)

	return strings.Join(parts, ",")
}

// syncWatches swaps in the informers that have synced, starts the ones that are due, and retires the
// informers of resources no auditor watches. active holds the active informer of each of watch, in
// order, when ready: an active informer counts only once it audits this source's current inputs, part.
// pending reports whether a replacement is still being built.
func (r *Reconciler) syncWatches(ctx context.Context, watch []watched, part string) (active []*gvrWatch, ready, pending bool, err error) {
	ready = true

	for _, w := range watch {
		gw, busy, werr := r.syncWatch(ctx, w, part)
		if werr != nil {
			return nil, false, false, fmt.Errorf("informer for %s: %w", w.GVR.Resource, werr)
		}

		pending = pending || busy

		if gw == nil {
			ready = false

			continue
		}

		active = append(active, gw)
	}

	r.pruneWatches()

	return active, ready, pending, nil
}

// syncWatch is syncWatches for one resource. It returns the active informer, nil until one has
// synced and audits part, and whether a replacement is pending.
func (r *Reconciler) syncWatch(ctx context.Context, w watched, part string) (*gvrWatch, bool, error) {
	fp := r.fingerprint(w.GVR)

	r.mu.Lock()
	slot := r.slotLocked(w.GVR)
	slot.promote(w.GVR)
	stale, start := slot.plan(fp)
	r.mu.Unlock()

	if stale != nil {
		stale.cancel()
	}

	if start {
		gw, err := r.startWatch(ctx, w, fp)
		if err != nil {
			return nil, false, err
		}

		r.mu.Lock()
		slot.pending = gw
		r.mu.Unlock()
	}

	r.mu.Lock()
	active, pending := slot.active, slot.pending
	r.mu.Unlock()

	// An informer started under older inputs has not audited this source's current ones.
	if active != nil && !slices.Contains(strings.Split(active.fingerprint, ","), part) {
		active = nil
	}

	if active == nil && pending != nil {
		if err := pending.lastErr(); err != nil {
			return nil, true, err
		}
	}

	return active, pending != nil, nil
}

// slotLocked returns gvr's slot, creating it. r.mu must be held.
func (r *Reconciler) slotLocked(gvr schema.GroupVersionResource) *gvrSlot {
	slot, ok := r.watches[gvr]
	if !ok {
		slot = &gvrSlot{}
		r.watches[gvr] = slot
	}

	return slot
}

// promote swaps in the pending informer once it has synced, cancelling the active one. r.mu must be
// held.
func (s *gvrSlot) promote(gvr schema.GroupVersionResource) {
	if s.pending == nil || !s.pending.synced() {
		return
	}

	if s.active != nil {
		s.active.cancel()
		informerRestarts.WithLabelValues(gvr.String()).Inc()
	}

	// The resync clock starts at the swap, not at the start of the replacement's list.
	s.pending.started = time.Now()
	s.active, s.pending = s.pending, nil
}

// plan reports whether a replacement for fingerprint fp should start, and detaches the stale
// pending informer for the caller to cancel. r.mu must be held.
func (s *gvrSlot) plan(fp string) (stale *gvrWatch, start bool) {
	due := s.active == nil || s.active.fingerprint != fp || (s.dirty && time.Since(s.active.started) >= resync)
	if !due || (s.pending != nil && s.pending.fingerprint == fp) {
		return nil, false
	}

	stale, s.pending, s.dirty = s.pending, nil, false

	return stale, true
}

// lastErr returns the last list or watch error.
func (gw *gvrWatch) lastErr() error {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	return gw.watchErr
}

// pruneWatches cancels and drops the informers of resources that no auditor watches.
func (r *Reconciler) pruneWatches() {
	want := map[schema.GroupVersionResource]bool{}

	for _, a := range r.snapshotAuditors() {
		a.mu.Lock()
		for _, w := range a.watched {
			want[w.GVR] = true
		}
		a.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for gvr, slot := range r.watches {
		if want[gvr] {
			continue
		}

		if slot.active != nil {
			slot.active.cancel()
		}

		if slot.pending != nil {
			slot.pending.cancel()
		}

		delete(r.watches, gvr)
	}
}

// snapshotAuditors returns the auditors, taken under r.mu and released before any auditor lock.
func (r *Reconciler) snapshotAuditors() []*auditor {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Collect(maps.Values(r.auditors))
}

// resource returns the resource the slot serves, from its active or pending informer. r.mu must be held.
func (s *gvrSlot) resource() (watched, bool) {
	for _, gw := range []*gvrWatch{s.active, s.pending} {
		if gw != nil {
			return gw.w, true
		}
	}

	return watched{}, false
}

// markDirty flags the resources match accepts for a re-evaluation, then enqueues the source bindings
// that watch them, so a pass runs and restarts the informers once they are due.
func (r *Reconciler) markDirty(match func(gvr schema.GroupVersionResource, w watched) bool) {
	dirtied := map[schema.GroupVersionResource]bool{}

	r.mu.Lock()
	for gvr, slot := range r.watches {
		if w, ok := slot.resource(); ok && match(gvr, w) {
			slot.dirty = true
			dirtied[gvr] = true
		}
	}
	r.mu.Unlock()

	var sources []string

	for _, a := range r.snapshotAuditors() {
		a.mu.Lock()
		if a.source != nil && slices.ContainsFunc(a.watched, func(w watched) bool { return dirtied[w.GVR] }) {
			sources = append(sources, a.source.Name)
		}
		a.mu.Unlock()
	}

	for _, name := range sources {
		select {
		case r.events <- event.GenericEvent{Object: &admissionregv1.ValidatingAdmissionPolicyBinding{Name: name}}:
		default: // The next resync reconciles it.
		}
	}
}

// namespaceHandler queues a namespace whose labels or annotations change for reaudit, since a namespaceSelector
// or a CEL expression that reads namespaceObject can flip a verdict without the object changing.
func (r *Reconciler) namespaceHandler() handler.Funcs {
	return handler.Funcs{
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldNS, okOld := e.ObjectOld.(*corev1.Namespace)
			newNS, okNew := e.ObjectNew.(*corev1.Namespace)

			if !okOld || !okNew || (maps.Equal(oldNS.Labels, newNS.Labels) && maps.Equal(oldNS.Annotations, newNS.Annotations)) {
				return
			}

			r.relabeled.Add(newNS.Name)
		},
	}
}

// relabelWorker reaudits the namespaces queued by namespaceHandler, one at a time, until the manager stops.
func (r *Reconciler) relabelWorker(ctx context.Context) error {
	context.AfterFunc(ctx, r.relabeled.ShutDown)

	for {
		ns, shutdown := r.relabeled.Get()
		if shutdown {
			return nil
		}

		if err := r.reaudit(ctx, ns); err != nil {
			log.FromContext(ctx).Error(err, "reaudit namespace", "namespace", ns)
			r.relabeled.AddRateLimited(ns)
		} else {
			r.relabeled.Forget(ns)
		}

		r.relabeled.Done(ns)
	}
}

// reaudit re-records the objects of ns that the namespaced informers hold, so a relabel applies in seconds
// without restarting the informers.
func (r *Reconciler) reaudit(ctx context.Context, ns string) error {
	var gws []*gvrWatch

	r.mu.Lock()
	for _, slot := range r.watches {
		for _, gw := range []*gvrWatch{slot.active, slot.pending} {
			if gw != nil && gw.w.Namespaced {
				gws = append(gws, gw)
			}
		}
	}
	r.mu.Unlock()

	for _, gw := range gws {
		if err := r.reauditResource(ctx, gw, ns); err != nil {
			return err
		}
	}

	return nil
}

// relabelPageSize bounds each list a reaudit makes, so a namespace with many objects is paged.
const relabelPageSize = 500

// reauditResource evaluates the objects of ns that gw's store holds, from a paged list of the API server.
func (r *Reconciler) reauditResource(ctx context.Context, gw *gvrWatch, ns string) error {
	opts := metav1.ListOptions{Limit: relabelPageSize}

	for {
		list, err := r.dynamic.Resource(gw.w.GVR).Namespace(ns).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list %s in %s: %w", gw.w.GVR.Resource, ns, err)
		}

		for i := range list.Items {
			u := &list.Items[i]
			if !inStore(gw, u) {
				continue
			}

			stub, err := r.stubOf(ctx, gw.w, u)
			if err != nil {
				return fmt.Errorf("evaluate %s %s/%s: %w", gw.w.GVR.Resource, ns, u.GetName(), err)
			}

			r.recordIf(ctx, gw, stub, func() bool { return inStore(gw, u) })
		}

		opts.Continue = list.GetContinue()
		if opts.Continue == "" {
			return nil
		}
	}
}

// inStore reports whether gw's store holds u at u's resourceVersion. Otherwise the object was deleted, or
// the informer has not yet delivered this version and evaluates it itself.
func inStore(gw *gvrWatch, u *unstructured.Unstructured) bool {
	obj, ok, err := gw.store.GetByKey(u.GetNamespace() + "/" + u.GetName())
	if err != nil || !ok {
		return false
	}

	stub, ok := obj.(*metav1.PartialObjectMetadata)

	return ok && stub.ResourceVersion == u.GetResourceVersion()
}

// paramUsers matches the resources of the auditors whose policy reads params of kind gvk.
func (r *Reconciler) paramUsers(gvk schema.GroupVersionKind) func(schema.GroupVersionResource, watched) bool {
	gvrs := map[schema.GroupVersionResource]bool{}

	for _, a := range r.snapshotAuditors() {
		a.mu.Lock()
		if a.policy != nil && a.policy.Spec.ParamKind != nil {
			if pk, err := policyParamGVK(a.policy.Spec.ParamKind); err == nil && pk == gvk {
				for _, w := range a.watched {
					gvrs[w.GVR] = true
				}
			}
		}
		a.mu.Unlock()
	}

	return func(gvr schema.GroupVersionResource, _ watched) bool { return gvrs[gvr] }
}

// paramsHandler re-evaluates the resources of the policies that read params of kind gvk when a params
// object changes, since a verdict can change without the audited object changing.
func (r *Reconciler) paramsHandler(gvk schema.GroupVersionKind) toolscache.ResourceEventHandlerDetailedFuncs {
	return toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(_ any, isInInitialList bool) {
			if !isInInitialList {
				r.markDirty(r.paramUsers(gvk))
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			o, okOld := oldObj.(metav1.Object)
			n, okNew := newObj.(metav1.Object)

			if okOld && okNew && o.GetResourceVersion() == n.GetResourceVersion() {
				return // a periodic resync, not a change
			}

			r.markDirty(r.paramUsers(gvk))
		},
		DeleteFunc: func(_ any) {
			r.markDirty(r.paramUsers(gvk))
		},
	}
}

// policyParamGVK returns the GVK of a policy's params kind.
func policyParamGVK(pk *admissionregv1.ParamKind) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(pk.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("params apiVersion %q: %w", pk.APIVersion, err)
	}

	return gv.WithKind(pk.Kind), nil
}
