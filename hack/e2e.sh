#!/usr/bin/env bash
# Extra args are test binary flags, e.g. -test.run=TestHardening.
set -euo pipefail
cd "$(dirname "$0")/.."

K8S_VERSION=${K8S_VERSION:-1.37}
case "$K8S_VERSION" in
  1.37) NODE_IMAGE="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5" ;;
  1.36) NODE_IMAGE="kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed" ;;
  *) echo "unsupported K8S_VERSION=$K8S_VERSION (supported: 1.36, 1.37)" >&2; exit 1 ;;
esac
command -v helm >/dev/null || { echo "helm not on PATH" >&2; exit 1; }

KIND_VERSION=v0.33.0
KPS_VERSION=92.14.1
KIND="bin/kind-$KIND_VERSION"
if [ ! -x "$KIND" ]; then
  mkdir -p bin
  GOBIN="$PWD/bin" go install "sigs.k8s.io/kind@$KIND_VERSION"
  mv bin/kind "$KIND"
fi

export KUBECONFIG="$PWD/bin/e2e-kubeconfig"

# VPNs can drop the host route to docker networks; the kind network is always reachable from a container.
incluster() {
  docker run --rm -i --network kind --user "$(id -u):$(id -g)" \
    -e HOME=/tmp -e KUBECONFIG="$PWD/bin/e2e-kubeconfig-internal" \
    -v "$PWD:$PWD" -w "$PWD" -v "$(command -v helm):/usr/local/bin/helm:ro" \
    --entrypoint "$1" "$NODE_IMAGE" "${@:2}"
}

collect() {
  rc=$?
  mkdir -p bin/e2e-logs || true
  incluster kubectl -n kar-system logs -l app.kubernetes.io/name=kar --all-containers --prefix --tail=-1 >bin/e2e-logs/kar.log 2>&1 || true
  incluster kubectl -n kar-system logs -l app.kubernetes.io/name=kar --all-containers --prefix --previous >bin/e2e-logs/kar-previous.log 2>&1 || true
  incluster kubectl -n default logs -l app.kubernetes.io/name=prometheus --all-containers --prefix --tail=-1 >bin/e2e-logs/prometheus.log 2>&1 || true
  incluster kubectl get events -A --sort-by=.lastTimestamp >bin/e2e-logs/events.txt 2>&1 || true
  if [ "${KEEP_CLUSTER:-0}" = 1 ]; then
    echo "cluster kar-e2e kept; use: export KUBECONFIG=$PWD/bin/e2e-kubeconfig" >&2
  else
    "$KIND" delete cluster --name kar-e2e >/dev/null 2>&1 || true
  fi
  exit "$rc"
}

# KAR_VERSION tests a published release: the chart from GHCR, which pulls its image from GHCR.
if [ -z "${KAR_VERSION:-}" ]; then
  # --network=host: build container cannot resolve DNS on some hosts.
  docker buildx build --network=host --load -t kar:e2e .
fi

"$KIND" delete cluster --name kar-e2e >/dev/null 2>&1 || true
trap collect EXIT
"$KIND" create cluster --name kar-e2e --image "$NODE_IMAGE" --kubeconfig "$KUBECONFIG" --wait 120s --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
"$KIND" get kubeconfig --internal --name kar-e2e >bin/e2e-kubeconfig-internal

[ -n "${KAR_VERSION:-}" ] || "$KIND" load docker-image kar:e2e --name kar-e2e

# Pulled on the host: a VPN can also cut the kind network off from the internet.
# The list follows the chart version.
PROMETHEUS_IMAGES=(
  "quay.io/prometheus-operator/prometheus-operator:v0.94.2"
  "quay.io/prometheus-operator/prometheus-config-reloader:v0.94.2"
  "quay.io/prometheus/prometheus:v3.15.1-distroless"
)
for img in "${PROMETHEUS_IMAGES[@]}"; do docker pull -q "$img" >/dev/null; done
# One platform only: kind imports with --all-platforms and fails on a multi-arch image missing the other platforms.
docker save --platform "$(docker version -f '{{.Server.Os}}/{{.Server.Arch}}')" -o bin/prometheus-images.tar "${PROMETHEUS_IMAGES[@]}"
"$KIND" load image-archive bin/prometheus-images.tar --name kar-e2e

# Restricted PSA: any hardening regression fails the install.
incluster kubectl create namespace kar-system
incluster kubectl label namespace kar-system pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted

[ -f "bin/kube-prometheus-stack-$KPS_VERSION.tgz" ] || helm pull oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack --version "$KPS_VERSION" -d bin/
incluster helm install prom "bin/kube-prometheus-stack-$KPS_VERSION.tgz" -n default --wait --timeout 3m --set alertmanager.enabled=false,grafana.enabled=false,nodeExporter.enabled=false,kubeStateMetrics.enabled=false,defaultRules.create=false,kubernetesServiceMonitors.enabled=false,prometheusOperator.admissionWebhooks.enabled=false,prometheusOperator.tls.enabled=false,prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false
# The config reloader only watches for changes once Prometheus answers; a ServiceMonitor created before that waits
# for its 3-minute resync.
incluster kubectl -n default wait --for=condition=Available prometheus --all --timeout=3m

if [ -n "${KAR_VERSION:-}" ]; then
  incluster helm install kar oci://ghcr.io/zemanlx/charts/kar --version "$KAR_VERSION" -n kar-system --set metrics.serviceMonitor.enabled=true --wait --timeout 3m
else
  incluster helm install kar charts/kar -n kar-system --set image.repository=kar,image.tag=e2e,image.pullPolicy=Never,metrics.serviceMonitor.enabled=true --wait --timeout 3m
fi

CGO_ENABLED=0 go test -c -tags e2e -o bin/e2e.test ./e2e/
incluster bin/e2e.test -test.count=1 -test.v -test.timeout=40m "$@"
