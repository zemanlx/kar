//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errTargets   = errors.New("up targets differ from running kar pods")
	errNoSeries  = errors.New("series missing")
	errPromQuery = errors.New("prometheus query failed")
)

// TestServiceMonitor checks that Prometheus scrapes every kar replica as job kar and keeps the violating namespace.
func TestServiceMonitor(t *testing.T) {
	t.Parallel()

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"})

	err := kube.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: karName}, sm)
	if apierrors.IsNotFound(err) {
		t.Skip("chart has no ServiceMonitor; the release predates it")
	}

	if err != nil {
		t.Fatalf("get ServiceMonitor: %v", err)
	}

	f := newFixture(t, "observe", "", 1, 0)
	ctx := t.Context()

	eventually(t, 3*time.Minute, func() error {
		pods, err := karPods(ctx)
		if err != nil {
			return err
		}

		up, err := promQuery(ctx, `up{job="kar"} == 1`)
		if err != nil {
			return err
		}

		if len(up) != len(pods) {
			return fmt.Errorf("%w: %d, want %d", errTargets, len(up), len(pods))
		}

		series, err := promQuery(ctx, fmt.Sprintf(`kar_violations{job="kar",namespace=%q,binding=%q}`, f.Violating[0], f.Name))
		if err != nil {
			return err
		}

		if len(series) == 0 {
			return fmt.Errorf("%w: %s in %s", errNoSeries, f.Name, f.Violating[0])
		}

		return nil
	})
}

// promQuery runs an instant query against prometheus-operated through the API server proxy and returns the labels of
// each series.
func promQuery(ctx context.Context, q string) ([]map[string]string, error) {
	b, err := clientset.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces/default/services/prometheus-operated:9090/proxy/api/v1/query").
		Param("query", q).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", q, err)
	}

	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("decode %s: %w", q, err)
	}

	if resp.Status != "success" {
		return nil, fmt.Errorf("%w: %s: %s", errPromQuery, q, resp.Error)
	}

	labels := make([]map[string]string, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		labels = append(labels, r.Metric)
	}

	return labels, nil
}
