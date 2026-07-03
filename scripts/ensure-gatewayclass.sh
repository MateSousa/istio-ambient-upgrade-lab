#!/usr/bin/env bash
# Ensure the istio-waypoint GatewayClass is registered and Accepted.
#
# The Gateway API CRDs are delivered by ArgoCD (apps/mesh/gateway-api-crds.yaml,
# sync-wave "-1", ahead of the mesh at 0). On a FRESH `make up` istiod boots
# AFTER the CRDs exist and registers the istio-waypoint GatewayClass on its own.
#
# But on an INCREMENTAL apply to an already-running slice-1-4 cluster, istiod was
# already up (booted before these CRDs existed) and will NOT register the
# GatewayClass until it is restarted (it only scans for the Gateway API CRDs at
# startup). This helper closes that gap: wait for the CRD + a Programmed/Accepted
# GatewayClass; if it is not Accepted within the timeout, roll istiod once and
# wait again. Idempotent and safe on a fresh cluster (the class is already
# Accepted, so the restart branch is never taken).
set -uo pipefail

CRD="gatewayclasses.gateway.networking.k8s.io"
GC="istio-waypoint"
CRD_TIMEOUT="${CRD_TIMEOUT:-120}"
ACCEPT_TIMEOUT="${ACCEPT_TIMEOUT:-90}"

echo "==> ensure GatewayClass/${GC} is Accepted"

# 1) Wait for the CRD to be Established (ArgoCD applies it on wave -1).
if ! kubectl wait --for=condition=Established "crd/${CRD}" --timeout="${CRD_TIMEOUT}s" >/dev/null 2>&1; then
  echo "WARN: CRD ${CRD} not Established after ${CRD_TIMEOUT}s (is the gateway-api-crds Application synced?)" >&2
fi

accepted() {
  # GatewayClass carries an Accepted condition once a controller (istiod) claims it.
  local status
  status="$(kubectl get gatewayclass "${GC}" \
    -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null)"
  [[ "${status}" == "True" ]]
}

wait_accepted() {
  local timeout="$1" deadline
  deadline=$(( $(date +%s) + timeout ))
  while (( $(date +%s) < deadline )); do
    if accepted; then return 0; fi
    sleep 3
  done
  return 1
}

if wait_accepted "${ACCEPT_TIMEOUT}"; then
  echo "PASS: GatewayClass/${GC} Accepted"
  exit 0
fi

echo "GatewayClass/${GC} not Accepted after ${ACCEPT_TIMEOUT}s; rolling istiod to force registration"
kubectl -n istio-system rollout restart deploy/istiod
kubectl -n istio-system rollout status deploy/istiod --timeout=180s

if wait_accepted "${ACCEPT_TIMEOUT}"; then
  echo "PASS: GatewayClass/${GC} Accepted after istiod restart"
  exit 0
fi

echo "FAIL: GatewayClass/${GC} still not Accepted after istiod restart" >&2
exit 1
