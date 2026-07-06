#!/usr/bin/env bash
# Bring up the lab end to end: preflight -> kind -> publish chart -> ArgoCD +
# OCI secret -> root app-of-apps -> wait for mesh -> verify.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLUSTER_NAME="istio-ambient-lab"
ARGOCD_INSTALL="gitops/argocd/install/argocd-install.yaml"
OCI_SECRET_TEMPLATE="gitops/argocd/install/ghcr-repo-secret.example.yaml"
OCI_SECRET_RENDERED="gitops/argocd/install/.ghcr-repo-secret.generated.yaml"
ROOT_APP="gitops/argocd/root-app-of-apps.yaml"

echo "==> preflight"
for bin in kind kubectl helm docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing required tool: $bin" >&2; exit 1; }
done
: "${GHCR_TOKEN:?GHCR_TOKEN must be set (PAT with read:packages+write:packages) - needed to publish and to pull the private chart}"
docker info >/dev/null 2>&1 || { echo "docker daemon not reachable" >&2; exit 1; }

# Warn (don't hard-fail) if the public Helm repos the observability apps need are
# unreachable - the mesh + app layers come up without them.
for repo in \
  "https://prometheus-community.github.io/helm-charts/index.yaml" \
  "https://grafana.github.io/helm-charts/index.yaml"; do
  if curl -sfI --max-time 10 "${repo}" >/dev/null 2>&1; then
    echo "    reachable: ${repo%/index.yaml}"
  else
    echo "WARN: cannot reach ${repo%/index.yaml} - the observability apps (wave 5) will not sync until ArgoCD can pull these public Helm charts. Check egress/proxy." >&2
  fi
done

echo "==> kind create cluster (${CLUSTER_NAME})"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo "cluster ${CLUSTER_NAME} already exists, reusing"
else
  kind create cluster --config kind/cluster.yaml --wait 120s
fi

echo "==> publish umbrella chart to GHCR"
scripts/publish-chart.sh

echo "==> install ArgoCD (vendored, pinned v3.3.12)"
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n argocd -f "${ARGOCD_INSTALL}"

echo "==> wait for ArgoCD core rollout"
kubectl -n argocd rollout status deploy/argocd-server --timeout=300s
kubectl -n argocd rollout status deploy/argocd-repo-server --timeout=300s
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s 2>/dev/null || \
  kubectl -n argocd rollout status deploy/argocd-application-controller --timeout=300s

echo "==> register private GHCR OCI repository secret"
# Render the Secret from the template, substituting ${GHCR_TOKEN}; python3 fallback
# so envsubst is not a hard dependency.
trap 'rm -f "${OCI_SECRET_RENDERED}"' EXIT
if command -v envsubst >/dev/null 2>&1; then
  GHCR_TOKEN="${GHCR_TOKEN}" envsubst '${GHCR_TOKEN}' < "${OCI_SECRET_TEMPLATE}" > "${OCI_SECRET_RENDERED}"
else
  GHCR_TOKEN="${GHCR_TOKEN}" python3 -c 'import os,sys; sys.stdout.write(sys.stdin.read().replace("${GHCR_TOKEN}", os.environ["GHCR_TOKEN"]))' \
    < "${OCI_SECRET_TEMPLATE}" > "${OCI_SECRET_RENDERED}"
fi
kubectl apply -f "${OCI_SECRET_RENDERED}"
rm -f "${OCI_SECRET_RENDERED}"

echo "==> build + kind-load demo app images"
# Must run before the app-of-apps: the app images use imagePullPolicy IfNotPresent
# with no registry prefix, so they must already be loaded when the apps sync.
scripts/build-app-images.sh

echo "==> apply root app-of-apps"
kubectl apply -f "${ROOT_APP}"

echo "==> wait for mesh to converge"
scripts/wait-mesh.sh

echo "==> ensure istio-waypoint GatewayClass is Accepted (roll istiod if needed)"
scripts/ensure-gatewayclass.sh

echo "==> verify"
scripts/verify.sh

echo "PASS: lab is up."
