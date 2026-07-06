#!/usr/bin/env bash
# scenario-atomic-sync.sh - reproduce the floating auto-sync hazard, then mitigate it.
#
# Phase A (hazard): with targetRevision floating a bounded range (">=1.0.0 <2.0.0")
# and selfHeal on, simply publishing a chart auto-deploys it with no operator gate -
# so a real hop takes two uncontrolled rolls instead of one.
#
# Phase B (mitigation): pin targetRevision to an exact version and bump the chart
# version and targetRevision together in one commit - a single operator-gated roll.
#
# Writes to main by design (demo-root watches HEAD). Live-only: the roll counts come
# from the live ArgoCD Application history, so this needs a cluster + GHCR_TOKEN.
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

: "${GHCR_TOKEN:?scenario-atomic-sync needs GHCR_TOKEN (write:packages to publish charts)}"

MESH="${ROOT}/apps/mesh/mesh.yaml"
MESH_BACKUP="$(mktemp)"
cp "${MESH}" "${MESH_BACKUP}"

# shellcheck disable=SC2329  # invoked indirectly by scen_install_trap's `trap`
restore() {
  # Restore mesh.yaml from the backup so the baseline (exact pin) is what remains
  # checked out; the operator commits it if they want main re-pinned.
  cp "${MESH_BACKUP}" "${MESH}" 2>/dev/null || true
  rm -f "${MESH_BACKUP}" 2>/dev/null || true
}
scen_install_trap restore

git_push_mesh() {
  local msg="$1"
  git -C "${ROOT}" add apps/mesh/mesh.yaml
  git -C "${ROOT}" commit -m "${msg}"
  git -C "${ROOT}" push
}

# =============================== Phase A: hazard ===============================
scen_info "Phase A: FLOAT targetRevision to a bounded range (reproduce the hazard)"
scen_set_target_revision '">=1.0.0 <2.0.0"'
git_push_mesh "test(atomic-sync): float mesh targetRevision to reproduce the hazard"

before_a="$(scen_mesh_history_len)"
scen_info "mesh sync-history length before publishing (floating): ${before_a}"

FLOAT_A="$(scen_fresh_version patch)"
scen_info "publishing a FRESH chart ${FLOAT_A} WITHOUT touching targetRevision - a floating range auto-syncs it"
# Publish only; deliberately do NOT bump targetRevision - the floating range makes
# ArgoCD selfHeal the new version in, the uncontrolled roll.
tmp="$(mktemp)"
sed -E 's|^(version:[[:space:]]*).*$|\1'"${FLOAT_A}"'|' "${ROOT}/charts/istio/Chart.yaml" > "${tmp}"
mv "${tmp}" "${ROOT}/charts/istio/Chart.yaml"
( cd "${ROOT}" && helm dependency update charts/istio >/dev/null )
"${ROOT}/scripts/publish-chart.sh"

scen_info "waiting ~90s for ArgoCD to auto-sync the floating range..."
sleep 90
after_a="$(scen_mesh_history_len)"
rolls_a=$(( after_a - before_a ))
scen_info "UNCONTROLLED rolls under the floating range: ${rolls_a} (history ${before_a} -> ${after_a})"
if [[ "${rolls_a}" -ge 1 ]]; then
  scen_pass "hazard reproduced: publishing a chart auto-deployed with NO operator gate (${rolls_a} roll[s])"
else
  scen_fail "expected >=1 uncontrolled roll under the floating range, saw ${rolls_a}"
fi

# ============================= Phase B: mitigation ============================
scen_info "Phase B: PIN targetRevision exactly and bump version + targetRevision TOGETHER (one gated roll)"
before_b="$(scen_mesh_history_len)"

PIN_B="$(scen_fresh_version patch)"
tmp="$(mktemp)"
sed -E 's|^(version:[[:space:]]*).*$|\1'"${PIN_B}"'|' "${ROOT}/charts/istio/Chart.yaml" > "${tmp}"
mv "${tmp}" "${ROOT}/charts/istio/Chart.yaml"
( cd "${ROOT}" && helm dependency update charts/istio >/dev/null )
"${ROOT}/scripts/publish-chart.sh"
# ...and pin targetRevision to it, in ONE commit (the atomic sync).
scen_set_target_revision "${PIN_B}"
git -C "${ROOT}" add apps/mesh/mesh.yaml charts/istio/Chart.yaml
git -C "${ROOT}" commit -m "chore(atomic-sync): pin mesh to ${PIN_B} (version + targetRevision together)"
git -C "${ROOT}" push

scen_info "waiting ~90s for the single gated sync..."
sleep 90
after_b="$(scen_mesh_history_len)"
rolls_b=$(( after_b - before_b ))
scen_info "GATED rolls under the exact pin + combined bump: ${rolls_b} (history ${before_b} -> ${after_b})"
if [[ "${rolls_b}" -le 1 ]]; then
  scen_pass "mitigated: the exact-pin combined bump produced a single operator-gated roll (${rolls_b})"
else
  scen_fail "expected a single gated roll, saw ${rolls_b}"
fi

echo ""
scen_info "contrast: floating auto-sync = ${rolls_a} uncontrolled roll(s); atomic pin = ${rolls_b} gated roll(s)"
scen_finish
