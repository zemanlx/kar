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
  incluster kubectl get events -A --sort-by=.lastTimestamp >bin/e2e-logs/events.txt 2>&1 || true
  if [ "${KEEP_CLUSTER:-0}" = 1 ]; then
    echo "cluster kar-e2e kept; use: export KUBECONFIG=$PWD/bin/e2e-kubeconfig" >&2
  else
    "$KIND" delete cluster --name kar-e2e >/dev/null 2>&1 || true
  fi
  exit "$rc"
}

# --network=host: build container cannot resolve DNS on some hosts.
docker buildx build --network=host --load -t kar:e2e .

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

"$KIND" load docker-image kar:e2e --name kar-e2e

# Restricted PSA: any hardening regression fails the install.
incluster kubectl create namespace kar-system
incluster kubectl label namespace kar-system pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted

incluster helm install kar charts/kar -n kar-system --set image.repository=kar,image.tag=e2e,image.pullPolicy=Never --wait --timeout 3m

CGO_ENABLED=0 go test -c -tags e2e -o bin/e2e.test ./e2e/
incluster bin/e2e.test -test.count=1 -test.v -test.timeout=40m "$@"
