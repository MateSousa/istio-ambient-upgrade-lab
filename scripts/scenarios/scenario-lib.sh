#!/usr/bin/env bash
# scenario-lib.sh - shared helpers for the upgrade-scenario scripts (slice 8).
#
# This file is SOURCED, never executed. Each scenario sets `set -euo pipefail`
# itself, sources this lib, installs a restore trap, and drives one scenario.
#
# It provides: repo-root resolution; target-node resolution; ArgoCD selfHeal
# snapshot/patch/restore for named Applications; the ONE fresh-version authority
# (`harness next-version`, so a run never invents a chart version in shell); jq
# JSON assertion helpers with a python3 fallback; a trap-restore installer; and
# PASS/FAIL printers mirroring scripts/verify.sh.
#
# No secret is ever echoed here: GHCR_TOKEN is only ever read from the
# environment by the scripts this lib helps, never printed.

# shellcheck shell=bash

SCEN_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCEN_LIB_DIR}/../.." && pwd)"

SCEN_FAILED=0

# ---------------------------------------------------------------- printers ----
scen_info() { echo "==> $*"; }
scen_pass() { echo "PASS: $*"; }
scen_fail() { echo "FAIL: $*" >&2; SCEN_FAILED=1; }
scen_die()  { echo "FAIL: $*" >&2; exit 1; }

# scen_finish prints the summary and exits non-zero if any gate failed. Scenarios
# that carry their OWN verdict (minor is allowed to FAIL/ERROR informatively)
# should not call this; they exit explicitly.
scen_finish() {
  echo ""
  if [[ "${SCEN_FAILED}" -eq 0 ]]; then
    echo "SCENARIO PASSED"
    exit 0
  fi
  echo "SCENARIO FAILED"
  exit 1
}

# ------------------------------------------------------------- trap helper ----
# scen_install_trap <fn>: run <fn> on EXIT/INT/TERM. <fn> must be idempotent so a
# signal mid-scenario followed by the EXIT trap does not double-apply. Expanding
# <fn> NOW (double quotes) is intentional: it registers the named function so the
# trap calls it directly; single-quoting would defer expansion of $1, which is out
# of scope when the signal fires.
scen_install_trap() {
  # shellcheck disable=SC2064
  trap "$1" EXIT INT TERM
}

# ---------------------------------------------------------- node resolution ---
# scen_resolve_node <node>: verify the node exists, echo it back. Fails loudly so
# a typo'd TARGET_NODE never silently no-ops the drain.
scen_resolve_node() {
  local node="$1"
  if ! kubectl get node "${node}" >/dev/null 2>&1; then
    scen_die "target node '${node}' not found (kubectl get node)"
  fi
  echo "${node}"
}

# scen_deploys_on_node <namespace> <node> <label-selector>: echo (space-joined)
# the Deployment names in <namespace> matching <label-selector> whose pod
# template is pinned (nodeName) to <node>. Robust to kind's worker naming (the
# first worker has no numeric suffix) because it filters on the actual nodeName,
# not a name-to-suffix guess.
scen_deploys_on_node() {
  local ns="$1" node="$2" sel="$3"
  kubectl -n "${ns}" get deploy -l "${sel}" \
    -o jsonpath='{range .items[?(@.spec.template.spec.nodeName=="'"${node}"'")]}{.metadata.name}{" "}{end}'
}

# ------------------------------------------------------ selfHeal snapshot -----
# scen_selfheal_get <app>: echo the Application's spec.syncPolicy.automated.selfHeal
# ("true"/"false"; empty if unset).
scen_selfheal_get() {
  kubectl -n argocd get application "$1" \
    -o jsonpath='{.spec.syncPolicy.automated.selfHeal}' 2>/dev/null || true
}

# scen_selfheal_set <app> <true|false>: patch selfHeal.
scen_selfheal_set() {
  kubectl -n argocd patch application "$1" --type merge \
    -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":'"$2"'}}}}' >/dev/null
}

# ------------------------------------------------- fresh-version authority ----
# scen_chart_version: the current umbrella version straight from Chart.yaml.
scen_chart_version() {
  awk '/^version:/ {print $2; exit}' "${ROOT}/charts/istio/Chart.yaml"
}

# scen_fresh_version <patch|minor>: the ONE place a scenario obtains the next
# umbrella version. Delegates to `harness next-version` so the increment + the
# mandatory -dev<runTag> prerelease + the <2.0.0 bound all come from the Go
# authority, never from shell arithmetic. The run tag is a wall-clock timestamp,
# which also guarantees GHCR immutability (a never-before-published version).
scen_fresh_version() {
  local hop="$1" cur tag
  cur="$(scen_chart_version)"
  tag="$(date +%s)"
  ( cd "${ROOT}/harness" && go run ./cmd/harness next-version \
      --current "${cur}" --hop "${hop}" --run-tag "${tag}" )
}

# ------------------------------------------------- JSON assertion helpers -----
# scen_node_attr <result.json> <node> <field>: read one fact about a node's
# perNodeAttribution entry. <field> is:
#   exists              -> "true"/"false": is there an entry for this node?
#   closed              -> "true"/"false": entry(ies) exist AND every window has a
#                          non-null readyAt (i.e. NOT half-open)
#   distinctConnsReset  -> integer summed across the node's windows, or "MISSING"
# jq is the fast path; python3 (a documented repo prereq) is the fallback so the
# helpers work even where jq is absent.
scen_node_attr() {
  local file="$1" node="$2" field="$3"
  if command -v jq >/dev/null 2>&1; then
    case "${field}" in
      exists)
        jq -r --arg n "${node}" '[.perNodeAttribution[]? | select(.node==$n)] | length > 0' "${file}" ;;
      closed)
        jq -r --arg n "${node}" '[.perNodeAttribution[]? | select(.node==$n)] | (length > 0) and all(.[]; .window.readyAt != null)' "${file}" ;;
      distinctConnsReset)
        jq -r --arg n "${node}" '[.perNodeAttribution[]? | select(.node==$n) | .distinctConnsReset] | if length==0 then "MISSING" else add end' "${file}" ;;
      *) scen_die "scen_node_attr: unknown field '${field}'" ;;
    esac
  else
    python3 - "${file}" "${node}" "${field}" <<'PY'
import json, sys
f, node, field = sys.argv[1], sys.argv[2], sys.argv[3]
with open(f) as fh:
    d = json.load(fh)
ents = [e for e in d.get("perNodeAttribution", []) if e.get("node") == node]
if field == "exists":
    print("true" if ents else "false")
elif field == "closed":
    ok = bool(ents) and all(e.get("window", {}).get("readyAt") is not None for e in ents)
    print("true" if ok else "false")
elif field == "distinctConnsReset":
    print(sum(e.get("distinctConnsReset", 0) for e in ents) if ents else "MISSING")
else:
    sys.exit("unknown field " + field)
PY
  fi
}

# scen_set_target_revision <rev>: rewrite apps/mesh/mesh.yaml's targetRevision
# line to <rev>, preserving indentation. <rev> may be an exact pin (1.0.0) or a
# quoted floating range ('">=1.0.0 <2.0.0"'); the `|` sed delimiter keeps the
# `<`/`>`/spaces in a range literal safe.
scen_set_target_revision() {
  local rev="$1" tmp
  tmp="$(mktemp)"
  sed -E 's|^([[:space:]]*targetRevision:[[:space:]]*).*$|\1'"${rev}"'|' \
    "${ROOT}/apps/mesh/mesh.yaml" > "${tmp}"
  mv "${tmp}" "${ROOT}/apps/mesh/mesh.yaml"
}

# scen_mesh_history_len: number of recorded sync operations in the mesh
# Application's status.history (each ArgoCD sync appends one). The delta across a
# window counts how many rolls happened - the metric the atomic-sync scenario
# uses to contrast "uncontrolled floating auto-syncs" vs "one gated sync".
scen_mesh_history_len() {
  kubectl -n argocd get application mesh \
    -o jsonpath='{.status.history[*].revision}' 2>/dev/null | wc -w | tr -d ' '
}

# scen_result_verdict <result.json>: echo the top-level verdict (PASS/FAIL/ERROR).
scen_result_verdict() {
  local file="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r '.verdict' "${file}"
  else
    python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("verdict",""))' "${file}"
  fi
}
