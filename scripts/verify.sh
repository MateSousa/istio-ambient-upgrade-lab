#!/usr/bin/env bash
# Assert the lab actually converged into a working ambient mesh with the demo
# pod enrolled in the datapath. Each gate prints PASS/FAIL; the script exits
# non-zero if any gate fails. The datapath gate is the load-bearing one: it
# proves the pod is in ztunnel's config, not merely Running with a label.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FAILED=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILED=1; }

# ------------------------------------------------------------------ nodes ----
echo "== gate: nodes Ready =="
not_ready="$(kubectl get nodes --no-headers 2>/dev/null | awk '$2 != "Ready" {print $1}')"
if [[ -z "${not_ready}" ]] && [[ -n "$(kubectl get nodes --no-headers 2>/dev/null)" ]]; then
  pass "all nodes Ready"
else
  fail "not all nodes Ready: ${not_ready:-<none found>}"
fi

# ------------------------------------------------------------ argocd rollout --
echo "== gate: ArgoCD rolled out =="
if kubectl -n argocd rollout status deploy/argocd-server --timeout=60s >/dev/null 2>&1 &&
   kubectl -n argocd rollout status deploy/argocd-repo-server --timeout=60s >/dev/null 2>&1; then
  pass "argocd-server + argocd-repo-server available"
else
  fail "ArgoCD core deployments not available"
fi

# ------------------------------------------------------- application health ---
echo "== gate: applications Synced/Healthy =="
for app in demo-root mesh; do
  health="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.health.status}' 2>/dev/null)"
  sync="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.sync.status}' 2>/dev/null)"
  if [[ "${health}" == "Healthy" && "${sync}" == "Synced" ]]; then
    pass "application ${app}: ${sync}/${health}"
  else
    fail "application ${app}: ${sync:-?}/${health:-?} (want Synced/Healthy)"
  fi
done

# ------------------------------------------------ mesh source + revision pin --
echo "== gate: mesh source is GHCR + resolved revision 1.0.0 =="
repo="$(kubectl -n argocd get application mesh -o jsonpath='{.spec.source.repoURL}' 2>/dev/null)"
rev="$(kubectl -n argocd get application mesh -o jsonpath='{.status.sync.revision}' 2>/dev/null)"
if [[ "${repo}" == ghcr.io/* ]]; then
  pass "mesh source repoURL is GHCR: ${repo}"
else
  fail "mesh source repoURL not GHCR: ${repo:-<none>}"
fi
if [[ "${rev}" == "1.0.0" ]]; then
  pass "mesh resolved revision is 1.0.0"
else
  fail "mesh resolved revision is '${rev:-<none>}' (want 1.0.0)"
fi

# --------------------------------------------------------------- istiod up ----
echo "== gate: istiod available =="
avail="$(kubectl -n istio-system get deploy istiod -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
if [[ "${avail:-0}" -ge 1 ]]; then
  pass "istiod has ${avail} available replica(s)"
else
  fail "istiod has no available replicas"
fi

# ---------------------------------------------- DaemonSet readiness (status) --
echo "== gate: ztunnel + istio-cni-node DaemonSets fully ready (status-based) =="
check_ds() {
  local ds="$1"
  local desired ready unavail
  if ! kubectl -n istio-system get daemonset "${ds}" >/dev/null 2>&1; then
    fail "daemonset ${ds} not found"; return
  fi
  desired="$(kubectl -n istio-system get daemonset "${ds}" -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null)"
  ready="$(kubectl -n istio-system get daemonset "${ds}" -o jsonpath='{.status.numberReady}' 2>/dev/null)"
  unavail="$(kubectl -n istio-system get daemonset "${ds}" -o jsonpath='{.status.numberUnavailable}' 2>/dev/null)"
  desired="${desired:-0}"; ready="${ready:-0}"; unavail="${unavail:-0}"
  if [[ "${desired}" -ge 1 && "${desired}" == "${ready}" && "${unavail}" -eq 0 ]]; then
    pass "daemonset ${ds}: desired=${desired} ready=${ready} unavailable=${unavail}"
  else
    fail "daemonset ${ds}: desired=${desired} ready=${ready} unavailable=${unavail}"
  fi
}
check_ds ztunnel
check_ds istio-cni-node

# ---------------------------------------------------- ztunnel grace = 120s ----
echo "== gate: ztunnel terminationGracePeriodSeconds = 120 =="
grace="$(kubectl -n istio-system get daemonset ztunnel -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}' 2>/dev/null)"
if [[ "${grace}" == "120" ]]; then
  pass "ztunnel terminationGracePeriodSeconds=120"
else
  fail "ztunnel terminationGracePeriodSeconds='${grace:-<unset>}' (want 120)"
fi

# ------------------------------------ CNI reconcileIptablesOnStartup == true --
echo "== gate: reconcileIptablesOnStartup effectively true =="
# The chart value cni.ambient.reconcileIptablesOnStartup (1.29 default: true)
# renders into the istio-cni-config ConfigMap as AMBIENT_RECONCILE_POD_RULES_ON_STARTUP.
# This is the precondition that lets the CNI DaemonSet roll without dropping
# existing ambient connections - assert the live value is exactly "true".
reconcile="$(kubectl -n istio-system get configmap istio-cni-config \
  -o jsonpath='{.data.AMBIENT_RECONCILE_POD_RULES_ON_STARTUP}' 2>/dev/null)"
if [[ "${reconcile,,}" == "true" ]]; then
  pass "istio-cni-config AMBIENT_RECONCILE_POD_RULES_ON_STARTUP=true"
else
  fail "istio-cni-config AMBIENT_RECONCILE_POD_RULES_ON_STARTUP='${reconcile:-<unset>}' (want true)"
fi

# --------------------------------- REAL datapath enrollment (ztunnel dump) ----
echo "== gate: hello pod is enrolled in ztunnel datapath (config_dump) =="
pod_ip="$(kubectl -n demo-app get pod -l app=hello -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
pod_node="$(kubectl -n demo-app get pod -l app=hello -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
if [[ -z "${pod_ip}" || -z "${pod_node}" ]]; then
  fail "could not resolve hello pod IP/node (ip='${pod_ip:-}' node='${pod_node:-}')"
else
  # ztunnel pod running on the SAME node as the hello pod
  zt_pod="$(kubectl -n istio-system get pod -l app=ztunnel \
    --field-selector "spec.nodeName=${pod_node}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  if [[ -z "${zt_pod}" ]]; then
    fail "no ztunnel pod found on node ${pod_node}"
  else
    echo "   hello pod ${pod_ip} on node ${pod_node}, querying ztunnel ${zt_pod}"
    # ztunnel's admin server (config_dump) is on localhost:15000 - port-forward
    # to this specific ztunnel and grep for the pod IP in its workload config.
    pf_out="$(mktemp)"
    kubectl -n istio-system port-forward "pod/${zt_pod}" 15000:15000 >"${pf_out}" 2>&1 &
    pf_pid=$!
    dump=""
    for _ in $(seq 1 15); do
      dump="$(curl -sf http://localhost:15000/config_dump 2>/dev/null || true)"
      [[ -n "${dump}" ]] && break
      sleep 1
    done
    kill "${pf_pid}" >/dev/null 2>&1 || true
    wait "${pf_pid}" 2>/dev/null || true
    rm -f "${pf_out}"
    if [[ -z "${dump}" ]]; then
      fail "could not read ztunnel config_dump on node ${pod_node}"
    elif echo "${dump}" | grep -q "\"${pod_ip}\""; then
      pass "hello pod IP ${pod_ip} present in ztunnel ${zt_pod} config_dump (datapath enrolled)"
    else
      fail "hello pod IP ${pod_ip} NOT in ztunnel ${zt_pod} config_dump (not enrolled in datapath)"
    fi
  fi
fi

# ------------------------------------------------------- hygiene re-scan ------
echo "== gate: no proprietary identifiers =="
if scripts/no-identity-scan.sh >/dev/null 2>&1; then
  pass "no-identity-scan clean"
else
  fail "no-identity-scan found proprietary identifiers"
fi

echo ""
if [[ "${FAILED}" -eq 0 ]]; then
  echo "ALL GATES PASSED"
  exit 0
else
  echo "ONE OR MORE GATES FAILED"
  exit 1
fi
