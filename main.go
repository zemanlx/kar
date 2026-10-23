// Command kar audits existing resources against opted-in ValidatingAdmissionPolicy bindings.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	karv1alpha1 "github.com/zemanlx/kar/api/v1alpha1"
	"github.com/zemanlx/kar/internal/controller"
	"github.com/zemanlx/kat/evaluator"
)

const defaultTTL = 192 * time.Hour // 8 days

var errNonPositiveTTL = errors.New("must be positive")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1) //nolint:gocritic // the process exits either way; stop() has nothing to release.
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	var (
		ttl         time.Duration
		metricsAddr string
		probeAddr   string
		leaderElect bool
	)

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.DurationVar(&ttl, "default-ttl", defaultTTL, "Exemption TTL when a binding sets no kar.zeman.dev/ttl annotation.")
	fs.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	fs.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health and readiness probes bind to.")
	fs.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so only one replica reconciles.")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return fmt.Errorf("parse flags: %w", err)
	}

	if ttl <= 0 {
		return fmt.Errorf("--default-ttl %v %w", ttl, errNonPositiveTTL)
	}

	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(stderr, nil)))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 newScheme(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "kar.zeman.dev",
		// Safe because run returns, and the process exits, as soon as the manager stops.
		LeaderElectionReleaseOnCancel: true,
		Client:                        client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Cache:                         cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := setup(mgr, cfg, ttl); err != nil {
		return err
	}

	ctrl.Log.WithName("setup").Info("starting", "defaultTTL", ttl)

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}

	return nil
}

// newScheme returns a scheme with the built-in and kar types.
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(karv1alpha1.AddToScheme(scheme))

	return scheme
}

// setup registers the audit controller and the health probes with mgr.
func setup(mgr ctrl.Manager, cfg *rest.Config, ttl time.Duration) error {
	ev, err := evaluator.New()
	if err != nil {
		return fmt.Errorf("create evaluator: %w", err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}

	rec := &controller.Reconciler{Client: mgr.GetClient(), Cache: mgr.GetCache(), Discovery: memory.NewMemCacheClient(dc), Evaluator: ev, DefaultTTL: ttl}
	if err := rec.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	return nil
}
