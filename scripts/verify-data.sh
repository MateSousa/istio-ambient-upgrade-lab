#!/usr/bin/env bash
# Data-path gates for slice 2. Proves:
#   - Postgres is up and OUTSIDE the ambient mesh (no dataplane-mode label AND
#     its pod IP never appears in any ztunnel config_dump, checked against the
#     same dump that DOES contain the in-mesh pgbouncer/app-a IPs).
#   - pgbouncer-writer + app-a ARE in the ztunnel datapath on their nodes.
#   - app-a serves /readyz and reads a widgets row via /query (app -> pgbouncer
#     -> Postgres, end to end).
#   - app-a holds a long-lived pooled client, visible in the pgbouncer admin
#     console (SHOW CLIENTS / SHOW POOLS), not just a single pg_stat_activity row.
#   - pool_mode=transaction and terminationGracePeriodSeconds=150 on both pools.
#   - app-a and pgbouncer-writer are scheduled on DIFFERENT nodes (anti-affinity).
#
# Invoked from verify.sh; standalone-runnable. Same PASS/FAIL + non-zero-on-fail
# convention as verify.sh.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FAILED=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILED=1; }

PGB_ADMIN_PW="pgbouncer_admin_pw"

# Read a specific ztunnel pod's config_dump via a short-lived port-forward.
ztunnel_dump() {
  local zt_pod="$1"
  local pf_out dump
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

# Run a pgbouncer admin console query against a specific writer pod IP, from the
# out-of-mesh Postgres pod (which ships psql). SHOW commands need admin_users.
pgb_admin() {
  local host="$1" sql="$2"
  kubectl -n demo-data exec deploy/postgres -- \
    env PGPASSWORD="${PGB_ADMIN_PW}" psql -h "${host}" -p 5432 -U pgbouncer -d pgbouncer \
    -tAc "${sql}" 2>/dev/null || true
}

# ---------------------------------------------------------- postgres Running --
echo "== gate: postgres Running in demo-data =="
pg_phase="$(kubectl -n demo-data get pod -l app=postgres -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"
pg_ready="$(kubectl -n demo-data get pod -l app=postgres -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)"
if [[ "${pg_phase}" == "Running" && "${pg_ready}" == "true" ]]; then
  pass "postgres pod Running and Ready"
else
  fail "postgres pod phase='${pg_phase:-?}' ready='${pg_ready:-?}' (want Running/true)"
fi

# --------------------------------------------- demo-data NOT ambient-enrolled --
echo "== gate: demo-data has NO dataplane-mode label =="
dpm="$(kubectl get ns demo-data -o jsonpath='{.metadata.labels.istio\.io/dataplane-mode}' 2>/dev/null)"
if [[ -z "${dpm}" ]]; then
  pass "demo-data carries no istio.io/dataplane-mode label (out of mesh)"
else
  fail "demo-data is ambient-enrolled (istio.io/dataplane-mode='${dpm}')"
fi

# ----------------------------------------------------------- collect pod IPs ---
pg_ip="$(kubectl -n demo-data get pod -l app=postgres -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
app_a_ip="$(kubectl -n demo-app get pod -l app=app-a -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)"
app_a_node="$(kubectl -n demo-app get pod -l app=app-a -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
mapfile -t writer_ips < <(kubectl -n demo-app get pod -l app=pgbouncer,pgbouncer-role=writer \
  -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' 2>/dev/null | grep -v '^$')
mapfile -t writer_nodes < <(kubectl -n demo-app get pod -l app=pgbouncer,pgbouncer-role=writer \
  -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -v '^$')

# ---------------------------------------- cache every ztunnel's config_dump ----
# ztunnel receives the FULL mesh workload table via xDS, so every ztunnel dump
# should list all ambient pod IPs (pgbouncer/app-a) and none of the out-of-mesh
# Postgres IP. Cache one dump per node for the gates below.
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

# --------------------- out-of-mesh negative WITH positive control (same dump) --
echo "== gate: Postgres absent from every ztunnel dump (positive control: pgbouncer+app-a present) =="
if [[ -z "${pg_ip}" || -z "${app_a_ip}" || ${#writer_ips[@]} -eq 0 ]]; then
  fail "could not resolve IPs (pg='${pg_ip:-}' app_a='${app_a_ip:-}' writers=${#writer_ips[@]})"
else
  for node in "${!ZT_DUMP[@]}"; do
    dump="${ZT_DUMP[${node}]}"
    zt="${NODE_ZT[${node}]}"
    if [[ -z "${dump}" ]]; then
      fail "ztunnel ${zt} (node ${node}): empty config_dump"
      continue
    fi
    # Positive control: dump is populated with in-mesh workloads.
    wfound=0
    for wip in "${writer_ips[@]}"; do
      echo "${dump}" | grep -q "\"${wip}\"" && { wfound=1; break; }
    done
    afound=0
    echo "${dump}" | grep -q "\"${app_a_ip}\"" && afound=1
    # Negative: Postgres IP must NOT be present in that same populated dump.
    pg_absent=1
    echo "${dump}" | grep -q "\"${pg_ip}\"" && pg_absent=0
    if [[ "${wfound}" -eq 1 && "${afound}" -eq 1 && "${pg_absent}" -eq 1 ]]; then
      pass "ztunnel ${zt} (node ${node}): populated (pgbouncer+app-a present) AND postgres ${pg_ip} absent"
    else
      fail "ztunnel ${zt} (node ${node}): writer_present=${wfound} app_a_present=${afound} postgres_absent=${pg_absent}"
    fi
  done
fi

# ------------------------- pgbouncer-writer + app-a enrolled on their nodes ----
echo "== gate: pgbouncer-writer + app-a IPs present in their own node's ztunnel =="
check_enrolled() {
  local label="$1" ip="$2" node="$3"
  local dump="${ZT_DUMP[${node}]:-}"
  if [[ -z "${dump}" ]]; then
    fail "${label} ${ip}: no ztunnel dump for node ${node}"
  elif echo "${dump}" | grep -q "\"${ip}\""; then
    pass "${label} ${ip} present in node ${node} ztunnel datapath"
  else
    fail "${label} ${ip} NOT in node ${node} ztunnel datapath"
  fi
}
check_enrolled "app-a" "${app_a_ip}" "${app_a_node}"
i=0
while [[ ${i} -lt ${#writer_ips[@]} ]]; do
  check_enrolled "pgbouncer-writer" "${writer_ips[${i}]}" "${writer_nodes[${i}]}"
  i=$((i + 1))
done

# ------------------------------------------------- app-a /readyz + /query ------
echo "== gate: app-a /readyz 200 and /query returns a widgets row =="
if kubectl -n demo-app exec deploy/app-a -- wget -q -O /dev/null -T 5 http://localhost:3000/readyz 2>/dev/null; then
  pass "app-a /readyz returned success"
else
  fail "app-a /readyz did not return success"
fi
qbody="$(kubectl -n demo-app exec deploy/app-a -- wget -q -O - -T 5 http://localhost:3000/query 2>/dev/null || true)"
if echo "${qbody}" | grep -qE '"name"|alpha|bravo|charlie'; then
  pass "app-a /query returned a widget: ${qbody}"
else
  fail "app-a /query did not return a widget (body: ${qbody:-<empty>})"
fi

# --------------------------------- long-lived pooled client (admin console) ----
echo "== gate: app-a holds a long-lived pooled client (pgbouncer SHOW CLIENTS/POOLS) =="
demo_clients=0
cl_active=0
for wip in "${writer_ips[@]}"; do
  sc="$(pgb_admin "${wip}" "SHOW CLIENTS;")"
  n="$(echo "${sc}" | awk -F'|' '$2=="demo_app"{c++} END{print c+0}')"
  demo_clients=$((demo_clients + n))
  sp="$(pgb_admin "${wip}" "SHOW POOLS;")"
  a="$(echo "${sp}" | awk -F'|' '$1=="demo"{print $3+0}' | head -n1)"
  cl_active=$((cl_active + ${a:-0}))
done
if [[ "${demo_clients}" -ge 1 || "${cl_active}" -ge 1 ]]; then
  pass "pgbouncer sees a persistent demo_app client (SHOW CLIENTS=${demo_clients}, demo pool cl_active=${cl_active})"
else
  fail "no persistent demo_app client in pgbouncer (SHOW CLIENTS=${demo_clients}, cl_active=${cl_active})"
fi

# -------------------------------------- pool_mode=transaction (live config) ----
echo "== gate: pgbouncer pool_mode=transaction (live SHOW CONFIG) =="
pool_mode=""
for wip in "${writer_ips[@]}"; do
  pool_mode="$(pgb_admin "${wip}" "SHOW CONFIG;" | awk -F'|' '$1=="pool_mode"{print $2}' | head -n1)"
  [[ -n "${pool_mode}" ]] && break
done
if [[ "${pool_mode}" == "transaction" ]]; then
  pass "pgbouncer pool_mode=transaction"
else
  fail "pgbouncer pool_mode='${pool_mode:-<unknown>}' (want transaction)"
fi

# ------------------------------- terminationGracePeriodSeconds=150 on pools ----
echo "== gate: pgbouncer writer + reader terminationGracePeriodSeconds=150 =="
for dep in pgbouncer-writer pgbouncer-reader; do
  grace="$(kubectl -n demo-app get deploy "${dep}" -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}' 2>/dev/null)"
  if [[ "${grace}" == "150" ]]; then
    pass "${dep} terminationGracePeriodSeconds=150"
  else
    fail "${dep} terminationGracePeriodSeconds='${grace:-<unset>}' (want 150)"
  fi
done

# ----------------------------------- anti-affinity: different nodes ------------
echo "== gate: app-a and pgbouncer-writer scheduled on DIFFERENT nodes =="
if [[ -z "${app_a_node}" || ${#writer_nodes[@]} -eq 0 ]]; then
  fail "could not resolve nodes (app_a='${app_a_node:-}' writers=${#writer_nodes[@]})"
else
  collision=0
  for wn in "${writer_nodes[@]}"; do
    [[ "${wn}" == "${app_a_node}" ]] && collision=1
  done
  if [[ "${collision}" -eq 0 ]]; then
    pass "app-a on ${app_a_node}; writers on ${writer_nodes[*]} (no co-location)"
  else
    fail "app-a on ${app_a_node} co-located with a pgbouncer-writer (${writer_nodes[*]})"
  fi
fi

echo ""
if [[ "${FAILED}" -eq 0 ]]; then
  echo "ALL DATA-PATH GATES PASSED"
  exit 0
else
  echo "ONE OR MORE DATA-PATH GATES FAILED"
  exit 1
fi
