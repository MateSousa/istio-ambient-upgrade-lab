#!/usr/bin/env bash
# Topology gates for slice 5. Proves the full three-service chain and the L7
# tenant waypoint are actually wired up, not merely present:
#   - the istio-waypoint GatewayClass is Accepted (the CRD-registration gate;
#     without it istiod never reconciles the Gateway).
#   - app-b (Python/SQLAlchemy) and app-c (Go/pgx) are enrolled in their nodes'
#     ztunnel datapath, and each serves /readyz + /query with a widgets row.
#   - both are persistent demo_app clients in pgbouncer (SHOW CLIENTS by
#     application_name), i.e. each holds its own pool.
#   - app-a GET /chain returns rows stitched from a -> b -> c.
#   - the Gateway tenant-waypoint is Programmed=True and its auto-managed Envoy
#     Deployment has >= 2 available replicas.
#   - app-b + app-c Services carry istio.io/use-waypoint == the Gateway name.
#   - the DestinationRules exist with the _common timeouts.
#   - L7 is actually traversed: the waypoint's istio_requests_total increments
#     when /chain is driven.
#   - Postgres is STILL out of mesh (its IP absent from every ztunnel dump).
#
# Invoked from verify.sh; standalone-runnable. Same PASS/FAIL + non-zero-on-fail
# convention as verify.sh / verify-data.sh.
#
# NOTE (FIX 3): ArgoCD's built-in health check for a gateway.networking.k8s.io
# Gateway keys on the Programmed=True condition - so the "waypoint app Healthy"
# in ArgoCD and the Programmed gate below are the same signal.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FAILED=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILED=1; }

PGB_ADMIN_PW="pgbouncer_admin_pw"
WAYPOINT_NAME="tenant-waypoint"

# ------------------------------------------------------------------ helpers ----
# Read a specific ztunnel pod's config_dump via a short-lived port-forward.
ztunnel_dump() {
  local zt_pod="$1" pf_out dump
  pf_out="$(mktemp)"
  kubectl -n istio-system port-forward "pod/${zt_pod}" 15000:15000 >"${pf_out}" 2>&1 &
  local pf_pid=$!
  dump=""
  for _ in $(seq 1 15); do
    dump="$(curl -sf http://localhost:15000/config_dump 2>/dev/null || true)"
    [[ -n "${dump}" ]] && break
    sleep 1
  done
  kill "${pf_pid}" >/dev/null 2>&1 || true
  wait "${pf_pid}" 2>/dev/null || true
  rm -f "${pf_out}"
  printf '%s' "${dump}"
}

# pgbouncer admin console query against a writer pod IP, from the out-of-mesh
# Postgres pod (which ships psql).
pgb_admin() {
  local host="$1" sql="$2"
  kubectl -n demo-data exec deploy/postgres -- \
    env PGPASSWORD="${PGB_ADMIN_PW}" psql -h "${host}" -p 5432 -U pgbouncer -d pgbouncer \
    -tAc "${sql}" 2>/dev/null || true
}

# HTTP GET from inside the app-a container (node:alpine ships busybox wget), so
# app-b/app-c (which have no shell) are reached over the mesh via Service DNS.
appa_get() {
  local url="$1"
  kubectl -n demo-app exec deploy/app-a -- wget -q -O - -T 8 "${url}" 2>/dev/null || true
}

# Read the waypoint Envoy's merged Prometheus metrics (port 15020) and sum every
# istio_requests_total series into a single integer.
waypoint_requests_total() {
  local wp_pod="$1" pf_out out
  pf_out="$(mktemp)"
  kubectl -n demo-app port-forward "pod/${wp_pod}" 15020:15020 >"${pf_out}" 2>&1 &
  local pf_pid=$!
  out=""
  for _ in $(seq 1 15); do
    out="$(curl -sf http://localhost:15020/stats/prometheus 2>/dev/null || true)"
    [[ -n "${out}" ]] && break
    sleep 1
  done
  kill "${pf_pid}" >/dev/null 2>&1 || true
  wait "${pf_pid}" 2>/dev/null || true
  rm -f "${pf_out}"
  echo "${out}" | awk '/^istio_requests_total\{/ {s+=$NF} END {printf "%d", s+0}'
}

# ------------------------------------------- gate: GatewayClass Accepted (FIX 2) --
echo "== gate: GatewayClass/istio-waypoint Accepted =="
gc_accepted="$(kubectl get gatewayclass istio-waypoint \
  -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null)"
if [[ "${gc_accepted}" == "True" ]]; then
  pass "GatewayClass/istio-waypoint Accepted=True"
else
  fail "GatewayClass/istio-waypoint Accepted='${gc_accepted:-<none>}' (want True; scripts/ensure-gatewayclass.sh rolls istiod if needed)"
fi

# ---------------------------------------------------------- collect pod IPs ----
app_b_ip="$(kubectl -n demo-app get pod -l app=app-b -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
app_b_node="$(kubectl -n demo-app get pod -l app=app-b -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
app_c_ip="$(kubectl -n demo-app get pod -l app=app-c -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
app_c_node="$(kubectl -n demo-app get pod -l app=app-c -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
pg_ip="$(kubectl -n demo-data get pod -l app=postgres -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
mapfile -t writer_ips < <(kubectl -n demo-app get pod -l app=pgbouncer,pgbouncer-role=writer \
  -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' 2>/dev/null | grep -v '^$')

# ---------------------------------------- cache every ztunnel's config_dump ----
declare -A NODE_ZT
declare -A ZT_DUMP
while read -r ztp ztnode; do
  [[ -z "${ztp}" || -z "${ztnode}" ]] && continue
  NODE_ZT["${ztnode}"]="${ztp}"
done < <(kubectl -n istio-system get pod -l app=ztunnel \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' 2>/dev/null)
for node in "${!NODE_ZT[@]}"; do
  ZT_DUMP["${node}"]="$(ztunnel_dump "${NODE_ZT[${node}]}")"
done

# ----------------------------------------- app-b + app-c enrolled in datapath --
echo "== gate: app-b + app-c enrolled in their nodes' ztunnel datapath =="
check_enrolled() {
  local label="$1" ip="$2" node="$3"
  local dump="${ZT_DUMP[${node}]:-}"
  if [[ -z "${ip}" || -z "${node}" ]]; then
    fail "${label}: could not resolve pod IP/node (ip='${ip:-}' node='${node:-}')"
  elif [[ -z "${dump}" ]]; then
    fail "${label} ${ip}: no ztunnel dump for node ${node}"
  elif echo "${dump}" | grep -q "\"${ip}\""; then
    pass "${label} ${ip} present in node ${node} ztunnel datapath"
  else
    fail "${label} ${ip} NOT in node ${node} ztunnel datapath"
  fi
}
check_enrolled "app-b" "${app_b_ip}" "${app_b_node}"
check_enrolled "app-c" "${app_c_ip}" "${app_c_node}"

# ------------------------------------------- Postgres STILL out of mesh --------
echo "== gate: Postgres absent from every ztunnel dump (still out of mesh) =="
if [[ -z "${pg_ip}" ]]; then
  fail "could not resolve postgres pod IP"
else
  pg_absent_all=1
  for node in "${!ZT_DUMP[@]}"; do
    if echo "${ZT_DUMP[${node}]}" | grep -q "\"${pg_ip}\""; then
      fail "postgres ${pg_ip} PRESENT in node ${node} ztunnel dump (should be out of mesh)"
      pg_absent_all=0
    fi
  done
  [[ "${pg_absent_all}" -eq 1 ]] && pass "postgres ${pg_ip} absent from all ${#ZT_DUMP[@]} ztunnel dump(s)"
fi

# --------------------------------------- app-b + app-c /readyz + /query --------
echo "== gate: app-b + app-c /readyz 200 and /query returns a widgets row =="
check_app_http() {
  local svc="$1" port="$2"
  # /readyz: wget returns success only on HTTP 2xx.
  if kubectl -n demo-app exec deploy/app-a -- wget -q -O /dev/null -T 8 "http://${svc}:${port}/readyz" 2>/dev/null; then
    pass "${svc} /readyz returned success"
  else
    fail "${svc} /readyz did not return success"
  fi
  local body
  body="$(appa_get "http://${svc}:${port}/query")"
  if echo "${body}" | grep -qE '"name"|alpha|bravo|charlie'; then
    pass "${svc} /query returned a widget: ${body}"
  else
    fail "${svc} /query did not return a widget (body: ${body:-<empty>})"
  fi
}
check_app_http "app-b" "8000"
check_app_http "app-c" "8080"

# ----------------------------------- persistent demo_app clients (both apps) ---
echo "== gate: app-b + app-c are persistent demo_app clients in pgbouncer =="
if [[ ${#writer_ips[@]} -eq 0 ]]; then
  fail "no pgbouncer-writer IPs resolved"
else
  clients_all=""
  for wip in "${writer_ips[@]}"; do
    clients_all+="$(pgb_admin "${wip}" "SHOW CLIENTS;")"$'\n'
  done
  for app in demo-app-b demo-app-c; do
    if echo "${clients_all}" | grep -q "${app}"; then
      pass "pgbouncer SHOW CLIENTS shows a persistent ${app} client (application_name)"
    else
      fail "pgbouncer SHOW CLIENTS has no ${app} client"
    fi
  done
fi

# ------------------------------------------------ app-a /chain returns a+b+c ---
echo "== gate: app-a /chain stitches rows from a -> b -> c =="
chain_body="$(appa_get "http://localhost:3000/chain")"
if echo "${chain_body}" | grep -q '"service":"app-a"' \
   && echo "${chain_body}" | grep -q '"service":"app-b"' \
   && echo "${chain_body}" | grep -q '"service":"app-c"' \
   && echo "${chain_body}" | grep -qE 'alpha|bravo|charlie'; then
  pass "app-a /chain returned a+b+c widgets: ${chain_body}"
else
  fail "app-a /chain did not stitch a+b+c (body: ${chain_body:-<empty>})"
fi

# --------------------------------- Gateway Programmed + auto Deployment ready ---
echo "== gate: Gateway ${WAYPOINT_NAME} Programmed=True + auto Deployment availableReplicas>=2 =="
programmed="$(kubectl -n demo-app get gateway "${WAYPOINT_NAME}" \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)"
if [[ "${programmed}" == "True" ]]; then
  pass "Gateway ${WAYPOINT_NAME} Programmed=True"
else
  fail "Gateway ${WAYPOINT_NAME} Programmed='${programmed:-<none>}' (want True)"
fi
wp_avail="$(kubectl -n demo-app get deploy "${WAYPOINT_NAME}" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
if [[ "${wp_avail:-0}" -ge 2 ]]; then
  pass "waypoint Deployment ${WAYPOINT_NAME} availableReplicas=${wp_avail} (>=2)"
else
  fail "waypoint Deployment ${WAYPOINT_NAME} availableReplicas='${wp_avail:-0}' (want >=2, HPA minReplicas 2)"
fi

# --------------------------------------- use-waypoint labels == Gateway name ---
echo "== gate: app-b + app-c Services carry istio.io/use-waypoint == ${WAYPOINT_NAME} =="
for svc in app-b app-c; do
  uw="$(kubectl -n demo-app get svc "${svc}" \
    -o jsonpath='{.metadata.labels.istio\.io/use-waypoint}' 2>/dev/null)"
  if [[ "${uw}" == "${WAYPOINT_NAME}" ]]; then
    pass "Service ${svc} istio.io/use-waypoint=${uw} (matches Gateway name)"
  else
    fail "Service ${svc} istio.io/use-waypoint='${uw:-<none>}' (want ${WAYPOINT_NAME})"
  fi
done

# --------------------------------------- DestinationRules present w/ timeouts ---
echo "== gate: DestinationRules for app-b + app-c carry the _common timeouts =="
for dr in app-b app-c; do
  idle="$(kubectl -n demo-app get destinationrule "${dr}" \
    -o jsonpath='{.spec.trafficPolicy.connectionPool.http.idleTimeout}' 2>/dev/null)"
  mrpc="$(kubectl -n demo-app get destinationrule "${dr}" \
    -o jsonpath='{.spec.trafficPolicy.connectionPool.http.maxRequestsPerConnection}' 2>/dev/null)"
  lb="$(kubectl -n demo-app get destinationrule "${dr}" \
    -o jsonpath='{.spec.trafficPolicy.loadBalancer.simple}' 2>/dev/null)"
  if [[ "${idle}" == "55s" && "${mrpc}" == "1000" && "${lb}" == "LEAST_REQUEST" ]]; then
    pass "DestinationRule ${dr}: idleTimeout=${idle} maxRequestsPerConnection=${mrpc} lb=${lb}"
  else
    fail "DestinationRule ${dr}: idleTimeout='${idle:-?}' maxRequestsPerConnection='${mrpc:-?}' lb='${lb:-?}' (want 55s/1000/LEAST_REQUEST)"
  fi
done

# ------------------------------------------------ L7 traversal proof (waypoint) --
echo "== gate: waypoint istio_requests_total increments on /chain (L7 path exercised) =="
wp_pod="$(kubectl -n demo-app get pod -l gateway.networking.k8s.io/gateway-name="${WAYPOINT_NAME}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
if [[ -z "${wp_pod}" ]]; then
  fail "no waypoint pod found (label gateway.networking.k8s.io/gateway-name=${WAYPOINT_NAME})"
else
  before="$(waypoint_requests_total "${wp_pod}")"
  for _ in $(seq 1 10); do
    appa_get "http://localhost:3000/chain" >/dev/null
  done
  sleep 3
  after="$(waypoint_requests_total "${wp_pod}")"
  if [[ "${after:-0}" -gt "${before:-0}" ]]; then
    pass "waypoint ${wp_pod} istio_requests_total ${before} -> ${after} (L7 traffic traversed the waypoint)"
  else
    fail "waypoint ${wp_pod} istio_requests_total did not increase (${before} -> ${after}); L7 path not proven"
  fi
fi

echo ""
if [[ "${FAILED}" -eq 0 ]]; then
  echo "ALL TOPOLOGY GATES PASSED"
  exit 0
else
  echo "ONE OR MORE TOPOLOGY GATES FAILED"
  exit 1
fi
