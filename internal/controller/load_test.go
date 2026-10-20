//go:build load

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//nolint:gochecknoglobals // command-line flags
var (
	loadStep   = flag.Int("load.step", 1000, "namespaces added per ramp step")
	loadMax    = flag.Int("load.max", 10000, "stop the ramp after this many namespaces")
	loadBudget = flag.Duration("load.budget", 3*time.Minute, "per-step convergence budget")
	loadStrict = flag.Bool("load.strict", false, "fail when a step breaks its budget, for regression runs below a known limit")

	loadObjectsStep  = flag.Int("load.objects.step", 50000, "TestLoadWatchedObjects: compliant ConfigMaps added per step")
	loadObjectsMax   = flag.Int("load.objects.max", 1000000, "TestLoadWatchedObjects: stop after this many compliant ConfigMaps")
	loadPoliciesStep = flag.Int("load.policies.step", 50, "TestLoadPolicies: source bindings added per step")
	loadPoliciesMax  = flag.Int("load.policies.max", 2000, "TestLoadPolicies: stop after this many source bindings")
	loadRate         = flag.String("load.rate", "800,1600,3200,6400,12800", "TestLoadChangeRate: comma-separated updates per second, one step each")

	loadPods      = flag.Int("load.pods", 100000, "TestLoadPods: Running Pods spread over load.pods.ns namespaces")
	loadPodsNS    = flag.Int("load.pods.ns", 4000, "TestLoadPods: namespaces, each with one violating Pod")
	loadPodsPprof = flag.String("load.pods.heapprofile", "", "TestLoadPods: write a heap profile here after the first pass")
)

// rateWindow is how long each TestLoadChangeRate step updates.
const rateWindow = 20 * time.Second

// Outcomes of the ramp budgets, beyond those in helpers_test.go.
const (
	outcomeHeap   = "heap"
	outcomeLag    = "lag"
	outcomeClient = "client-bound"
)

// Budgets that end a ramp: a step over one is the limit. heapBudgetMiB is 90% of the chart's 512Mi limit.
const (
	coldStartBudget = 2 * time.Minute
	heapBudgetMiB   = 460
	policiesBudget  = 30 * time.Second
	rateBudget      = 10 * time.Second
	rateShare       = 0.9 // an achieved rate below this share of the target is the client's limit, not kar's
)

// TestLoadExemptNamespaces ramps violating namespaces until the controller converges, reports
// StateTooLarge, or runs out of budget. The controller runs in this process, so HeapInuse includes it
// and the test client, not the controller alone.
func TestLoadExemptNamespaces(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	// The first two objects are the "bad" namespace and its ConfigMap, which the load does not need.
	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce})[2:])

	step, limit := *loadStep, *loadMax

	t.Logf("%10s %9s %12s %12s %9s  %s", "namespaces", "converge", "status-bytes", "binding-bytes", "heapMiB", "outcome")

	last, stopped := 0, "reached load.max"

	for from := 0; from < limit; {
		to := min(from+step, limit)

		addNamespaces(t, c, from, to)
		poke(t, c, to)

		outcome, took := converge(ctx, c, names(to), *loadBudget)

		statusBytes, bindingBytes, err := sizes(ctx, c)
		if err != nil {
			t.Fatalf("size state: %v", err)
		}

		runtime.GC()

		var ms runtime.MemStats

		runtime.ReadMemStats(&ms)
		t.Logf("%10d %9.1f %12d %12d %9.1f  %s", to, took.Seconds(), statusBytes, bindingBytes,
			float64(ms.HeapInuse)/(1<<20), outcome)

		if outcome != outcomeConverged {
			stopped = fmt.Sprintf("%s at %d namespaces", outcome, to)
			breached(t, stopped)

			break
		}

		admit(t, c, to)

		last = to
		from = to
	}

	t.Logf("limit: last converged %d namespaces; stopped: %s", last, stopped)
}

// breached fails the test under -load.strict, where every step is expected to stay within its budget.
func breached(t *testing.T, stopped string) {
	t.Helper()

	if *loadStrict {
		t.Errorf("budget breached: %s", stopped)
	}
}

// admit checks that, with n namespaces exempt, a dry-run violating ConfigMap is allowed in an exempt
// namespace and denied in a fresh non-exempt one. It retries briefly, as the API server's binding
// cache lags the controller's write.
func admit(t *testing.T, c client.Client, n int) {
	t.Helper()

	exempt, fresh := nsName(0), fmt.Sprintf("probe-%d", n)
	createAll(t, c, []client.Object{fixtureNS(fresh)})

	eventually(t, time.Minute, func() error {
		if err := c.Create(t.Context(), probe(exempt), client.DryRunAll); err != nil {
			return fmt.Errorf("exempt namespace %s: %w", exempt, err)
		}

		if err := c.Create(t.Context(), probe(fresh), client.DryRunAll); !isDenied(err) {
			return fmt.Errorf("fresh namespace %s: %w", fresh, errors.Join(errNotDenied, err))
		}

		return nil
	})
}

// probe is a violating ConfigMap in ns, named apart from the "app" the load puts in each namespace.
func probe(ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Name: "probe", Namespace: ns}
}

// TestLoadWatchedObjects ramps compliant ConfigMaps over 10 namespaces, with one violating namespace added per
// step. Each step stops the controller, adds its objects, and starts a fresh one, as a new leader does. It times
// the cold start until the state converges, then the live heap with that controller running.
func TestLoadWatchedObjects(t *testing.T) {
	t.Parallel()

	cfg := startAPIServer(t, nil)
	c := newClient(t, cfg)
	ctx := t.Context()

	stop := startController(t, cfg)

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce})[2:])
	addNamespaces(t, c, 0, 1)
	watchNamespaces(t, c)

	t.Logf("%10s %9s %9s  %s", "objects", "seconds", "heapMiB", "outcome")

	objects, next := 0, 1 // next is the index of the namespace the step's violation goes in
	stopped := "reached load.objects.max"

	for objects < *loadObjectsMax {
		to := min(objects+*loadObjectsStep, *loadObjectsMax)

		stop()
		runtime.GC()

		createEach(t, c, objects, to, compliant)
		addNamespaces(t, c, next, next+1)

		started := time.Now()
		stop = startController(t, cfg)

		outcome, _ := converge(ctx, c, names(next+1), coldStartBudget)
		took := time.Since(started)
		heap := heapMiB()

		switch {
		case took > coldStartBudget:
			outcome = outcomeLag
		case outcome == outcomeConverged && heap > heapBudgetMiB:
			outcome = outcomeHeap
		}

		t.Logf("%10d %9.1f %9.1f  %s", to, took.Seconds(), heap, outcome)

		if outcome != outcomeConverged {
			stopped = fmt.Sprintf("%s at %d objects", outcome, to)
			breached(t, stopped)

			break
		}

		objects, next = to, next+1
	}

	t.Logf("limit: last passed %d objects; stopped: %s", objects, stopped)
}

// container is an app container as a Deployment's Pod carries it after admission: env, probes,
// resources, a hardened security context and the service account token mount.
func container(name, image string, port int32) corev1.Container {
	keys := []string{"LOG_LEVEL", "LOG_FORMAT", "HTTP_PORT", "DB_HOST", "DB_NAME", "CACHE_URL", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME", "FEATURE_FLAGS", "REGION"}
	env := make([]corev1.EnvVar, 0, 2+len(keys))
	env = append(env,
		corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
	)

	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: "value-of-" + strings.ToLower(k)})
	}

	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{
			HTTPGet:       &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)},
			PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3,
		}
	}

	return corev1.Container{
		Name:           name,
		Image:          image,
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		Env:            env,
		LivenessProbe:  probe("/healthz"),
		ReadinessProbe: probe("/readyz"),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/etc/app", ReadOnly: true},
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "kube-api-access-7x2kq", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			RunAsNonRoot:             new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// runningStatus is what the kubelet reports for a Pod whose containers all run.
func runningStatus(containers []corev1.Container, i int) corev1.PodStatus {
	now := metav1.Now()
	ip := fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)
	hostIP := fmt.Sprintf("192.168.%d.%d", i/250%250, i%250)

	st := corev1.PodStatus{
		Phase:     corev1.PodRunning,
		HostIP:    hostIP,
		HostIPs:   []corev1.HostIP{{IP: hostIP}},
		PodIP:     ip,
		PodIPs:    []corev1.PodIP{{IP: ip}},
		StartTime: &now,
		QOSClass:  corev1.PodQOSBurstable,
	}
	for _, c := range []corev1.PodConditionType{"PodReadyToStartContainers", corev1.PodInitialized, corev1.PodReady, corev1.ContainersReady, corev1.PodScheduled} {
		st.Conditions = append(st.Conditions, corev1.PodCondition{Type: c, Status: corev1.ConditionTrue, LastTransitionTime: now})
	}

	for _, c := range containers {
		st.ContainerStatuses = append(st.ContainerStatuses, corev1.ContainerStatus{
			Name:         c.Name,
			Image:        c.Image,
			ImageID:      c.Image + "@sha256:" + strings.Repeat("3f9a", 16),
			ContainerID:  fmt.Sprintf("containerd://%064d", i),
			Ready:        true,
			Started:      new(true),
			State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}},
			RestartCount: 0,
		})
	}

	return st
}

// realisticPod is the i-th Running Deployment Pod in ns: an app and a proxy sidecar, volumes, an owner
// ReplicaSet and kubelet status. It violates the require-owner policy when violating is set.
func realisticPod(ns string, i int, violating bool) *corev1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/name": "app", "app.kubernetes.io/instance": "app", "app.kubernetes.io/version": "1.27.3",
		"pod-template-hash": "5d8f7c9b6d", "team": "payments",
	}
	if !violating {
		labels["owner"] = "team"
	}

	containers := []corev1.Container{
		container("app", "registry.example.com/payments/app:1.27.3", 8080),
		container("proxy", "registry.example.com/mesh/proxy:1.24.1", 15090),
	}

	return &corev1.Pod{
		Name:      fmt.Sprintf("app-5d8f7c9b6d-%06d", i),
		Namespace: ns,
		Labels:    labels,
		Annotations: map[string]string{
			"kubectl.kubernetes.io/restartedAt": "2026-10-01T09:00:00Z",
			"prometheus.io/scrape":              "true",
			"prometheus.io/port":                "15090",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app-5d8f7c9b6d",
			UID: "0b6f2d1e-8c4a-4f3e-9a7b-2c5d8e1f4a6b", Controller: new(true), BlockOwnerDeletion: new(true),
		}},
		Spec: corev1.PodSpec{
			Containers:         containers,
			ServiceAccountName: "app",
			NodeName:           fmt.Sprintf("ip-192-168-%d-%d.eu-west-1.compute.internal", i/250%250, i%250),
			SecurityContext:    &corev1.PodSecurityContext{RunAsUser: new(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Volumes: []corev1.Volume{
				{Name: "config", ConfigMap: &corev1.ConfigMapVolumeSource{Name: "app-config"}},
				{Name: "tmp", EmptyDir: &corev1.EmptyDirVolumeSource{}},
				{Name: "kube-api-access-7x2kq", Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: new(int64(3607)), Path: "token"}},
					{ConfigMap: &corev1.ConfigMapProjection{Name: "kube-root-ca.crt", Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
				}}},
			},
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: new(int64(300))},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: new(int64(300))},
			},
		},
		Status: runningStatus(containers, i),
	}
}

// procStatusMiB returns the given field of /proc/self/status (VmRSS, VmHWM) in MiB, or -1 off Linux.
func procStatusMiB(field string) float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}

	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(line, field+":"); ok {
			kib, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			if err != nil {
				return -1
			}

			return float64(kib) / 1024
		}
	}

	return -1
}

// TestLoadPods measures kar's memory for one enforce policy over load.pods realistic Running Pods in
// load.pods.ns namespaces, with one violating Pod per namespace. The Pods exist before the policy, so
// this is the cold start a new leader goes through. The test client caches nothing, so the heap
// growth after the policy is the controller's: the Pod and Namespace informers, the audit pass and
// the exemption state.
func TestLoadPods(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()
	nss, pods := *loadPodsNS, *loadPods

	createEach(t, c, 0, nss, func(i int) client.Object { return fixtureNS(nsName(i)) })

	start := time.Now()

	createPods(t, c, nss, pods)
	t.Logf("created %d namespaces and %d pods in %.0fs", nss, pods, time.Since(start).Seconds())

	podBytes := storedPodBytes(t, c)

	debug.FreeOSMemory()

	baseHeap := heapMiB()
	baseRSS := procStatusMiB("VmRSS")
	// Writing 5 resets VmHWM to the current RSS, so the peak below is the controller's, not the setup's.
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0); err != nil {
		t.Logf("cannot reset peak RSS: %v", err)
	}

	peakHeap, stopSampling := samplePeakHeap(ctx)

	base := violatingFixture(map[string]string{modeAnnotation: modeEnforce})
	policy, okPolicy := base[2].(*admissionregv1.ValidatingAdmissionPolicy)

	binding, okBinding := base[3].(*admissionregv1.ValidatingAdmissionPolicyBinding)
	if !okPolicy || !okBinding {
		t.Fatal("violatingFixture no longer has a policy and binding at 2 and 3")
	}

	policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"pods"}
	createAll(t, c, []client.Object{policy, binding})

	outcome, took := converge(ctx, c, names(nss), *loadBudget)
	firstHeap, firstPeak, firstHWM := heapMiB(), peakHeap(), procStatusMiB("VmHWM")

	if *loadPodsPprof != "" {
		writeHeapProfile(t, *loadPodsPprof)
	}

	// One more full pass, so the peak covers a periodic resync and not only the cold start.
	time.Sleep(resync + 10*time.Second)
	stopSampling()

	t.Logf("pod JSON %d bytes (managedFields stripped); %d namespaces, %d pods; first pass %s in %.1fs",
		podBytes, nss, pods, outcome, took.Seconds())
	t.Logf("%-24s %9s %9s %9s", "MiB", "live-heap", "peak-heap", "peak-RSS")
	t.Logf("%-24s %9.1f %9s %9.1f", "before policy", baseHeap, "-", baseRSS)
	t.Logf("%-24s %9.1f %9.1f %9.1f", "after first pass", firstHeap, firstPeak, firstHWM)
	t.Logf("%-24s %9.1f %9.1f %9.1f", "after one resync", heapMiB(), peakHeap(), procStatusMiB("VmHWM"))
	t.Logf("live heap per pod: %.1f KiB", (heapMiB()-baseHeap)*1024/float64(pods))

	if outcome != outcomeConverged {
		t.Errorf("%d namespaces did not converge: %s", nss, outcome)
	}
}

// storedPodBytes returns the JSON size of the first Pod as the API server stores it, managedFields stripped.
func storedPodBytes(t *testing.T, c client.Client) int {
	t.Helper()

	sample := &corev1.Pod{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: nsName(0), Name: realisticPod(nsName(0), 0, true).Name}, sample); err != nil {
		t.Fatalf("get sample pod: %v", err)
	}

	sample.ManagedFields = nil

	b, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("marshal sample pod: %v", err)
	}

	return len(b)
}

func writeHeapProfile(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create heap profile: %v", err)
	}

	if err := pprof.WriteHeapProfile(f); err != nil {
		t.Errorf("write heap profile: %v", err)
	}

	_ = f.Close()
}

// createPods creates pods realistic Running Pods spread over nss namespaces; the first nss violate.
func createPods(t *testing.T, c client.Client, nss, pods int) {
	t.Helper()

	jobs := make(chan int)

	var wg sync.WaitGroup

	for range 32 {
		wg.Go(func() {
			for i := range jobs {
				p := realisticPod(nsName(i%nss), i, i < nss)
				status := p.Status

				if err := c.Create(t.Context(), p); err != nil {
					t.Errorf("create pod %d: %v", i, err)

					continue
				}

				p.Status = status
				if err := c.Status().Update(t.Context(), p); err != nil {
					t.Errorf("set status of pod %d: %v", i, err)
				}
			}
		})
	}

	for i := range pods {
		jobs <- i
	}

	close(jobs)
	wg.Wait()
}

// samplePeakHeap samples HeapInuse every 100ms until stop is called. peak returns the highest sample in MiB.
func samplePeakHeap(ctx context.Context) (peak func() float64, stop func()) {
	var high atomic.Uint64

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var ms runtime.MemStats

				runtime.ReadMemStats(&ms)

				if ms.HeapInuse > high.Load() {
					high.Store(ms.HeapInuse)
				}
			}
		}
	}()

	return func() float64 { return float64(high.Load()) / (1 << 20) }, func() { cancel(); <-done }
}

// TestLoadPolicies ramps source bindings, each with its own exemption and derived binding, over the same
// rule. The objects are fixed: 1000 compliant ConfigMaps and 10 violating namespaces.
func TestLoadPolicies(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	base := violatingFixture(map[string]string{modeAnnotation: modeEnforce})
	policy, okPolicy := base[2].(*admissionregv1.ValidatingAdmissionPolicy)

	binding, okBinding := base[3].(*admissionregv1.ValidatingAdmissionPolicyBinding)
	if !okPolicy || !okBinding {
		t.Fatal("violatingFixture no longer has a policy and binding at 2 and 3")
	}

	addNamespaces(t, c, 0, 10)
	watchNamespaces(t, c)
	createEach(t, c, 0, 1000, compliant)

	want := names(10)

	t.Logf("%9s %9s %9s  %s", "policies", "seconds", "heapMiB", "outcome")

	count, stopped := 0, "reached load.policies.max"

	for count < *loadPoliciesMax {
		to := min(count+*loadPoliciesStep, *loadPoliciesMax)

		for i := count; i < to; i++ {
			p, b := policy.DeepCopy(), binding.DeepCopy()
			p.Name, b.Name, b.Spec.PolicyName = policyName(i), policyName(i), policyName(i)
			createAll(t, c, []client.Object{p, b})
		}

		outcome, took := settle(ctx, policiesBudget, func(ctx context.Context) error {
			return policiesSettled(ctx, c, to, want)
		})
		if outcome == outcomeBudgetSpent {
			outcome = outcomeLag
		}

		t.Logf("%9d %9.1f %9.1f  %s", to, took.Seconds(), heapMiB(), outcome)

		if outcome != outcomeConverged {
			stopped = fmt.Sprintf("%s at %d policies", outcome, to)
			breached(t, stopped)

			break
		}

		count = to
	}

	t.Logf("limit: last passed %d policies; stopped: %s", count, stopped)
}

// updateLoop updates the ConfigMap "app" in nss at rate per second until ctx ends. Updates run on 32
// workers, so one slow call does not cap the rate; a tick with no idle worker is dropped, which shows as a
// lower achieved rate. It returns the completed updates and the time they ran for.
func updateLoop(ctx context.Context, c client.Client, rate int, nss []string) (int64, time.Duration) {
	start := time.Now()
	jobs := make(chan int)

	var done atomic.Int64

	var wg sync.WaitGroup

	for range 32 {
		wg.Go(func() {
			for n := range jobs {
				cm := &corev1.ConfigMap{Name: "app", Namespace: nss[n%len(nss)], Data: map[string]string{"tick": strconv.Itoa(n)}}
				if c.Update(ctx, cm) == nil {
					done.Add(1)
				}
			}
		})
	}

	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()

			return done.Load(), time.Since(start)
		case <-ticker.C:
			select {
			case jobs <- n:
			default: // every worker is busy
			}
		}
	}
}

// parseRates parses the comma-separated positive rates of -load.rate.
func parseRates(t *testing.T, s string) []int {
	t.Helper()

	var rates []int

	for field := range strings.SplitSeq(s, ",") {
		r, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || r <= 0 {
			t.Fatalf("load.rate: %q is not a positive rate", field)
		}

		rates = append(rates, r)
	}

	return rates
}

// TestLoadChangeRate exempts 1000 namespaces, then runs each update rate for rateWindow. The updates go to
// the ConfigMaps of those exempt namespaces. Updates of already-exempt namespaces only re-extend their
// expiration in memory, so this measures the audit path, not new exemptions. During each window, one new
// violating namespace is added and timed until it is recorded.
func TestLoadChangeRate(t *testing.T) {
	t.Parallel()

	c := startEnv(t)
	ctx := t.Context()

	createAll(t, c, violatingFixture(map[string]string{modeAnnotation: modeEnforce})[2:])
	addNamespaces(t, c, 0, 1000)
	poke(t, c, 1000)

	exempt := names(1000)
	if outcome, _ := converge(ctx, c, exempt, *loadBudget); outcome != outcomeConverged {
		t.Fatalf("1000 namespaces did not converge: %s", outcome)
	}

	t.Logf("%8s %9s %9s %9s  %s", "rate", "achieved", "seconds", "heapMiB", "outcome")

	last, stopped := 0, "reached the last load.rate step"

	for i, rate := range parseRates(t, *loadRate) {
		windowCtx, cancel := context.WithTimeout(ctx, rateWindow)

		var (
			updates int64
			window  time.Duration
			wg      sync.WaitGroup
		)

		wg.Go(func() { updates, window = updateLoop(windowCtx, c, rate, exempt) })

		ns := 1000 + i
		addNamespaces(t, c, ns, ns+1)
		poke(t, c, ns+1)

		outcome, took := converge(ctx, c, names(ns+1), rateBudget)

		wg.Wait()
		cancel()

		achieved := float64(updates) / window.Seconds()

		switch {
		case outcome == outcomeBudgetSpent:
			outcome = outcomeLag
		case outcome == outcomeConverged && achieved < rateShare*float64(rate):
			outcome = outcomeClient
		}

		t.Logf("%8d %9.0f %9.1f %9.1f  %s", rate, achieved, took.Seconds(), heapMiB(), outcome)

		if outcome != outcomeConverged {
			stopped = fmt.Sprintf("%s at %d updates/s", outcome, rate)
			breached(t, stopped)

			break
		}

		last = rate
	}

	t.Logf("limit: last passed %d updates/s; stopped: %s", last, stopped)
}
