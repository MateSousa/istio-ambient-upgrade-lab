#!/usr/bin/env bash
# Ensure the istio-waypoint GatewayClass is Accepted. istiod only scans for the
# Gateway API CRDs at startup, so on an incremental apply where istiod booted
# before the CRDs existed it must be rolled to register the class. Wait for the
# CRD + an Accepted class; if not Accepted within the timeout, roll istiod once
# and wait again. Idempotent (a fresh cluster is already Accepted).
set -uo pipefail

CRD="gatewayclasses.gateway.networking.k8s.io"
GC="istio-waypoint"
CRD_TIMEOUT="${CRD_TIMEOUT:-120}"
ACCEPT_TIMEOUT="${ACCEPT_TIMEOUT:-90}"

echo "==> ensure GatewayClass/${GC} is Accepted"

if ! kubectl wait --for=condition=Established "crd/${CRD}" --timeout="${CRD_TIMEOUT}s" >/dev/null 2>&1; then
  echo "WARN: CRD ${CRD} not Established after ${CRD_TIMEOUT}s (is the gateway-api-crds Application synced?)" >&2
fi

accepted() {
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
