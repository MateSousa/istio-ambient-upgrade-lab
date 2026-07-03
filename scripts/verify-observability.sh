#!/usr/bin/env bash
# Observability gates (slice 6). Proves the Grafana OSS stack (Prometheus + Loki +
# Grafana + Alloy) is deployed, healthy, and actually observing the mesh:
#   - the four observability Applications are Synced/Healthy.
#   - Prometheus /api/v1/targets shows an UP target for the ztunnel + istiod jobs
#     (waypoint is best-effort/retried - it may lag wave-3/4 convergence).
#   - Prometheus /api/v1/query returns data for the REAL upstream metric names and
#     the lab:tcp_sockets_open recording rule.
#   - Loki is ingesting (label values include demo-app + istio-system; a broad
#     query returns > 0 streams) and the ECONNRESET|Connection terminated LogQL
#     EXECUTES (200) - gate on ingestion + query-executes, NOT on non-empty, since
#     no drop may have happened yet.
#   - Grafana /api/health is ok and both datasources + the dashboard are present.
#
# Invoked from verify.sh; standalone-runnable. Same PASS/FAIL + non-zero-on-fail
# convention as the other verify-*.sh scripts.
#
# IMPORTANT (source of truth): these are the operator-facing SigNoz-parity VIEW.
# The drill's verdict (new-conn failures, existing-conn RSTs, recovery) comes from
# the slice-3 HARNESS (results.json), not from Prometheus/Grafana. This script
# checks the view is wired up and honest, not that it decides PASS/FAIL.
#
# NETWORK DEPENDENCY: the observability charts are pulled from the PUBLIC Helm
# repos (prometheus-community.github.io, grafana.github.io) by ArgoCD's repo-server.
# If those are unreachable the apps never sync and these gates fail - see
# scripts/up.sh preflight + README.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FAILED=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILED=1; }
warn() { echo "WARN: $*" >&2; }

NS="observability"
PROM_SVC="prometheus-server"
PROM_PORT=80
LOKI_SVC="loki"
LOKI_PORT=3100
GRAF_SVC="grafana"
GRAF_PORT=80

# ---------------------------------------------------------------- helpers ------
# Run a curl against an in-cluster Service via a short-lived port-forward. Args:
#   $1 svc  $2 remote-port  $3 local-port  $4 path  [$5 extra curl args...]
pf_curl() {
  local svc="$1" rport="$2" lport="$3" path="$4"; shift 4
  local pf_out out
  pf_out="$(mktemp)"
  kubectl -n "${NS}" port-forward "svc/${svc}" "${lport}:${rport}" >"${pf_out}" 2>&1 &
  local pf_pid=$!
  out=""
  for _ in $(seq 1 15); do
    out="$(curl -sf "$@" "http://localhost:${lport}${path}" 2>/dev/null || true)"
    [[ -n "${out}" ]] && break
    sleep 1
  done
  kill "${pf_pid}" >/dev/null 2>&1 || true
  wait "${pf_pid}" 2>/dev/null || true
  rm -f "${pf_out}"
  printf '%s' "${out}"
}

# Prometheus instant query -> resultType/result present with at least one sample.
prom_has_data() {
  local q="$1" body
  body="$(pf_curl "${PROM_SVC}" "${PROM_PORT}" 19090 "/api/v1/query" --get --data-urlencode "query=${q}")"
  echo "${body}" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
r=d.get("data",{}).get("result",[])
sys.exit(0 if d.get("status")=="success" and len(r)>0 else 1)
' 2>/dev/null
}

# ----------------------------------------------- application health (retried) --
echo "== gate: observability Applications Synced/Healthy =="
for app in prometheus loki alloy grafana observability-config; do
  ok=0
  for _ in $(seq 1 30); do
    health="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.health.status}' 2>/dev/null)"
    sync="$(kubectl -n argocd get application "${app}" -o jsonpath='{.status.sync.status}' 2>/dev/null)"
    if [[ "${health}" == "Healthy" && "${sync}" == "Synced" ]]; then ok=1; break; fi
    sleep 5
  done
  if [[ "${ok}" -eq 1 ]]; then
    pass "application ${app}: Synced/Healthy"
  else
    fail "application ${app}: ${sync:-?}/${health:-?} (want Synced/Healthy)"
  fi
done

# ------------------------------------------------ Prometheus targets up --------
# ztunnel + istiod are required; waypoint is best-effort (retried, never hard-fail
# before wave 3/4 converge - see FIX 3).
echo "== gate: Prometheus scrape targets UP (ztunnel + istiod required, waypoint best-effort) =="
targets_json="$(pf_curl "${PROM_SVC}" "${PROM_PORT}" 19090 "/api/v1/targets")"
job_up() {
  local job="$1"
  echo "${targets_json}" | python3 -c '
import json,sys
job=sys.argv[1]
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(2)
act=d.get("data",{}).get("activeTargets",[])
up=[t for t in act if t.get("labels",{}).get("job")==job and t.get("health")=="up"]
sys.exit(0 if up else 1)
' "${job}" 2>/dev/null
}
for job in ztunnel istiod; do
  if job_up "${job}"; then
    pass "Prometheus job ${job} has an UP target"
  else
    fail "Prometheus job ${job} has no UP target"
  fi
done
# waypoint: poll, but only warn if still down (may lag; it is not a verdict signal)
wp_ok=0
for _ in $(seq 1 12); do
  targets_json="$(pf_curl "${PROM_SVC}" "${PROM_PORT}" 19090 "/api/v1/targets")"
  if job_up waypoint; then wp_ok=1; break; fi
  sleep 5
done
if [[ "${wp_ok}" -eq 1 ]]; then
  pass "Prometheus job waypoint has an UP target"
else
  warn "Prometheus job waypoint has no UP target yet (best-effort; not gated - waypoint may still be converging)"
fi

# ------------------------------------------------ REAL metric names present ----
echo "== gate: Prometheus has data for the REAL metric names + lab:tcp_sockets_open =="
# Only the KNOWN-real upstream series + the lab recording rule. istio_requests_total
# depends on L7 traffic + the waypoint being up, so it is best-effort (warn).
for q in \
  'istio_tcp_connections_opened_total' \
  'istio_tcp_connections_closed_total' \
  'pilot_xds' \
  'pilot_total_xds_rejects' \
  'lab:tcp_sockets_open'; do
  if prom_has_data "${q}"; then
    pass "Prometheus query returned data: ${q}"
  else
    fail "Prometheus query returned NO data: ${q}"
  fi
done
if prom_has_data 'istio_requests_total{namespace="demo-app"}'; then
  pass "Prometheus query returned data: waypoint istio_requests_total"
else
  warn "waypoint istio_requests_total has no data yet (needs L7 traffic + waypoint up; best-effort)"
fi

# ------------------------------------------------------- Loki ingesting --------
echo "== gate: Loki is ingesting (namespace label values include demo-app + istio-system) =="
labelvals="$(pf_curl "${LOKI_SVC}" "${LOKI_PORT}" 13100 "/loki/api/v1/label/namespace/values")"
have_ns() { echo "${labelvals}" | python3 -c '
import json,sys
ns=sys.argv[1]
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if ns in d.get("data",[]) else 1)
' "$1" 2>/dev/null; }
for ns in demo-app istio-system; do
  if have_ns "${ns}"; then
    pass "Loki has ingested logs labelled namespace=${ns}"
  else
    fail "Loki has NOT ingested logs labelled namespace=${ns}"
  fi
done

# broad query returns > 0 streams (ingestion sanity)
echo "== gate: Loki broad query returns > 0 streams =="
end_ns="$(date +%s)000000000"
start_ns="$(( $(date +%s) - 3600 ))000000000"
broad="$(pf_curl "${LOKI_SVC}" "${LOKI_PORT}" 13100 "/loki/api/v1/query_range" \
  --get \
  --data-urlencode 'query={namespace="demo-app"}' \
  --data-urlencode "start=${start_ns}" \
  --data-urlencode "end=${end_ns}" \
  --data-urlencode 'limit=5')"
if echo "${broad}" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
r=d.get("data",{}).get("result",[])
sys.exit(0 if d.get("status")=="success" and len(r)>0 else 1)
' 2>/dev/null; then
  pass "Loki {namespace=\"demo-app\"} returned > 0 streams"
else
  fail "Loki {namespace=\"demo-app\"} returned no streams"
fi

# ECONNRESET|Connection terminated LogQL EXECUTES (200) - may be empty until a drop
# occurs; gate on the query executing, not on it being non-empty.
echo "== gate: Loki ECONNRESET|Connection terminated LogQL executes (200, may be empty) =="
errq="$(pf_curl "${LOKI_SVC}" "${LOKI_PORT}" 13100 "/loki/api/v1/query_range" \
  --get \
  --data-urlencode 'query={namespace="demo-app"} |~ "ECONNRESET|Connection terminated"' \
  --data-urlencode "start=${start_ns}" \
  --data-urlencode "end=${end_ns}" \
  --data-urlencode 'limit=5')"
if echo "${errq}" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if d.get("status")=="success" else 1)
' 2>/dev/null; then
  pass "Loki error LogQL executed (status success; empty is OK until a drop occurs)"
else
  fail "Loki error LogQL did not execute cleanly"
fi

# --------------------------------------------------------- Grafana health ------
echo "== gate: Grafana /api/health ok + datasources + dashboard present =="
health="$(pf_curl "${GRAF_SVC}" "${GRAF_PORT}" 13000 "/api/health")"
if echo "${health}" | grep -q '"database"[[:space:]]*:[[:space:]]*"ok"'; then
  pass "Grafana /api/health database ok"
else
  fail "Grafana /api/health not ok (got: ${health:-<empty>})"
fi
# datasources + dashboard via admin/admin basic auth
ds="$(pf_curl "${GRAF_SVC}" "${GRAF_PORT}" 13000 "/api/datasources" -u admin:admin)"
for name in Prometheus Loki; do
  if echo "${ds}" | grep -q "\"name\":\"${name}\""; then
    pass "Grafana datasource ${name} provisioned"
  else
    fail "Grafana datasource ${name} NOT provisioned (got: ${ds:-<empty>})"
  fi
done
search="$(pf_curl "${GRAF_SVC}" "${GRAF_PORT}" 13000 "/api/search?query=Istio%20Ambient%20Upgrade" -u admin:admin)"
if echo "${search}" | grep -q 'Istio Ambient Upgrade'; then
  pass "Grafana dashboard 'Istio Ambient Upgrade' present (sidecar loaded the ConfigMap)"
else
  fail "Grafana dashboard not found via /api/search (got: ${search:-<empty>})"
fi

echo ""
if [[ "${FAILED}" -eq 0 ]]; then
  echo "ALL OBSERVABILITY GATES PASSED"
  exit 0
else
  echo "ONE OR MORE OBSERVABILITY GATES FAILED"
  exit 1
fi
