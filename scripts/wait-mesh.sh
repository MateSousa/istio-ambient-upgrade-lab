#!/usr/bin/env bash
# Wait for the app-of-apps to converge the mesh. Polls ArgoCD Application health
# and sync status plus the core Istio workloads until healthy or timeout.
set -euo pipefail

TIMEOUT="${WAIT_TIMEOUT:-600}"
INTERVAL=10
deadline=$(( $(date +%s) + TIMEOUT ))

app_ok() {
  local app="$1"
  local health sync
  health="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
  sync="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
  [[ "${health}" == "Healthy" && "${sync}" == "Synced" ]]
}

echo "==> waiting up to ${TIMEOUT}s for demo-root, mesh, demo-hello, data, app-a to be Synced/Healthy"
while :; do
  if app_ok demo-root && app_ok mesh && app_ok demo-hello && app_ok data && app_ok app-a; then
    echo "all applications Synced/Healthy"
    break
  fi
  if (( $(date +%s) >= deadline )); then
    echo "TIMEOUT waiting for applications" >&2
    kubectl -n argocd get applications 2>/dev/null || true
    exit 1
  fi
  sleep "${INTERVAL}"
done

echo "==> waiting for istiod + ztunnel + istio-cni-node"
kubectl -n istio-system rollout status deploy/istiod --timeout="${TIMEOUT}s"
kubectl -n istio-system rollout status daemonset/ztunnel --timeout="${TIMEOUT}s"
kubectl -n istio-system rollout status daemonset/istio-cni-node --timeout="${TIMEOUT}s"

echo "==> waiting for hello pod"
kubectl -n demo-app rollout status deploy/hello --timeout="${TIMEOUT}s"

echo "==> waiting for data-path workloads"
kubectl -n demo-data rollout status deploy/postgres --timeout="${TIMEOUT}s"
kubectl -n demo-app rollout status deploy/pgbouncer-writer --timeout="${TIMEOUT}s"
kubectl -n demo-app rollout status deploy/pgbouncer-reader --timeout="${TIMEOUT}s"
kubectl -n demo-app rollout status deploy/app-a --timeout="${TIMEOUT}s"

echo "PASS: mesh converged."
