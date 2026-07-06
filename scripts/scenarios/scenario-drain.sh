#!/usr/bin/env bash
# scenario-drain.sh - the zero-drop cordon/drain mitigation. Draining the harness
# workloads off a node before its ztunnel rolls means that node's roll resets zero
# attributable connections, while the rest of the mesh takes its normal bounded
# per-node reset (the contrast this prints).
#
# It is NOT `kubectl drain`: the probe/load/echo pods are nodeName-pinned (bypassing
# the scheduler), so a plain drain would refuse to evict them or leave them Pending.
# Instead it cordons, then scales those Deployments to 0 and `wait --for=delete`s.
# selfHeal must be suspended on BOTH the probe and load Applications, else ArgoCD
# reverts the scale-to-0 back to replicas:1 from HEAD.
#
# Scoped claim: zero-drop covers the harness-controlled workloads (probe/load/echo).
# app-a/app-b/app-c are ordinary clients; one may sit on the target node and reset
# elsewhere - invisible to the verdict and not part of the claim.
#
# No GHCR: the roll is provoked with rollout-restart, so nothing is published or pushed.
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

NS="demo-app"
TARGET_NODE="${TARGET_WORKER:-istio-ambient-lab-worker3}"
SELECTOR='app in (echo,probe,load)'

# Restore snapshots (populated during setup; restore() is idempotent).
DRAIN_SNAP_PROBE=""
DRAIN_SNAP_LOAD=""
DRAIN_DEPLOYS=""
DRAIN_CORDONED="no"

# shellcheck disable=SC2329  # invoked indirectly by scen_install_trap's `trap`
restore() {
  # Best-effort and idempotent. ArgoCD restores replicas:1 once selfHeal is back on;
  # scaling back explicitly just makes convergence immediate.
  local d
  for d in ${DRAIN_DEPLOYS}; do
    kubectl -n "${NS}" scale deploy "${d}" --replicas=1 >/dev/null 2>&1 || true
  done
  [[ -n "${DRAIN_SNAP_PROBE}" ]] && scen_selfheal_set probe "${DRAIN_SNAP_PROBE}" 2>/dev/null || true
  [[ -n "${DRAIN_SNAP_LOAD}" ]] && scen_selfheal_set load "${DRAIN_SNAP_LOAD}" 2>/dev/null || true
  if [[ "${DRAIN_CORDONED}" == "yes" ]]; then
    kubectl uncordon "${TARGET_NODE}" >/dev/null 2>&1 || true
  fi
}
scen_install_trap restore

# (1) resolve target node -------------------------------------------------------
scen_info "resolving target node ${TARGET_NODE}"
TARGET_NODE="$(scen_resolve_node "${TARGET_NODE}")"

# (2) cordon --------------------------------------------------------------------
scen_info "cordoning ${TARGET_NODE} (nothing new schedules here during the window)"
kubectl cordon "${TARGET_NODE}"
DRAIN_CORDONED="yes"

# (3) suspend selfHeal on probe AND load ---------------------------------------
DRAIN_SNAP_PROBE="$(scen_selfheal_get probe)"; DRAIN_SNAP_PROBE="${DRAIN_SNAP_PROBE:-true}"
DRAIN_SNAP_LOAD="$(scen_selfheal_get load)";  DRAIN_SNAP_LOAD="${DRAIN_SNAP_LOAD:-true}"
scen_info "suspending selfHeal on probe/load (snapshots: probe=${DRAIN_SNAP_PROBE} load=${DRAIN_SNAP_LOAD})"
scen_selfheal_set probe false
scen_selfheal_set load false

# (4) scale the target node's echo/probe/load Deployments to 0 -----------------
read -r -a DRAIN_DEPLOY_ARR <<<"$(scen_deploys_on_node "${NS}" "${TARGET_NODE}" "${SELECTOR}")"
DRAIN_DEPLOYS="${DRAIN_DEPLOY_ARR[*]}"
if [[ -z "${DRAIN_DEPLOYS// /}" ]]; then
  scen_die "no echo/probe/load Deployments are pinned to ${TARGET_NODE}; nothing to drain (is this a worker node?)"
fi
scen_info "scaling to 0 on ${TARGET_NODE}: ${DRAIN_DEPLOYS}"
for d in ${DRAIN_DEPLOYS}; do
  kubectl -n "${NS}" scale deploy "${d}" --replicas=0
done

scen_info "waiting for the drained pods to terminate (held conns FIN cleanly)"
kubectl -n "${NS}" wait --for=delete pod -l "${SELECTOR}" \
  --field-selector "spec.nodeName=${TARGET_NODE}" --timeout=120s >/dev/null 2>&1 || true

# Confirm no harness pod (echo/probe/load) remains on the target node.
remaining="$(kubectl -n "${NS}" get pods -l "${SELECTOR}" \
  --field-selector "spec.nodeName=${TARGET_NODE}" \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}')"
if [[ -n "${remaining// /}" ]]; then
  scen_die "harness pods still on ${TARGET_NODE} after drain: ${remaining}"
fi
scen_pass "no echo/probe/load pods remain on ${TARGET_NODE} (drain complete)"

# (5) provoke the ztunnel roll and measure -------------------------------------
scen_info "firing rollout-restart and measuring (all ztunnels roll; target has no held conns)"
(
  cd "${ROOT}/harness"
  go run ./cmd/harness measure \
    --trigger rollout-restart --i-know-this-is-not-ac \
    --out ../results.json --out-clients ../clients.json
) || scen_info "measure exited non-zero (expected: other nodes take their normal bounded resets)"

RESULTS="${ROOT}/results.json"
[[ -f "${RESULTS}" ]] || scen_die "no results.json produced"

# (6) ASSERT the zero-drop proof for the drained node --------------------------
exists="$(scen_node_attr "${RESULTS}" "${TARGET_NODE}" exists)"
closed="$(scen_node_attr "${RESULTS}" "${TARGET_NODE}" closed)"
resets="$(scen_node_attr "${RESULTS}" "${TARGET_NODE}" distinctConnsReset)"

if [[ "${exists}" != "true" ]]; then
  scen_fail "no perNodeAttribution window for ${TARGET_NODE}: its ztunnel roll was not observed (cannot prove zero-drop)"
elif [[ "${closed}" != "true" ]]; then
  scen_fail "${TARGET_NODE} window is half-open (readyAt null): the roll did not complete, zero-drop unproven"
elif [[ "${resets}" != "0" ]]; then
  scen_fail "${TARGET_NODE} distinctConnsReset=${resets} (want 0): the drain did not eliminate all attributable resets"
else
  scen_pass "${TARGET_NODE}: window CLOSED and distinctConnsReset=0 (ZERO-DROP proven for the drained node)"
fi

# (7) contrast: a non-drained node takes its normal bounded reset --------------
scen_info "contrast - resets per node window (drained node must read 0):"
if command -v jq >/dev/null 2>&1; then
  jq -r '.perNodeAttribution[] | "  \(.node): distinctConnsReset=\(.distinctConnsReset)"' "${RESULTS}"
else
  python3 -c 'import json,sys
for e in json.load(open(sys.argv[1])).get("perNodeAttribution",[]):
    print("  %s: distinctConnsReset=%s" % (e.get("node"), e.get("distinctConnsReset")))' "${RESULTS}"
fi

# (8) render the report ---------------------------------------------------------
scen_info "rendering the Markdown report"
(
  cd "${ROOT}/harness"
  go run ./cmd/harness report --in ../results.json --clients ../clients.json --out ../report.md
) || scen_fail "report render failed"

# (9) restore runs via the EXIT trap.
scen_finish
