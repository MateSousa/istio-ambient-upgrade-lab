#!/usr/bin/env bash
# scenario-reset.sh - roll the RUNNING cluster back to the 1.29.2 baseline. Slice 8.
#
# CLUSTER-DEEP (board-mandated): this is NOT a working-tree-only `git checkout`.
# It rewrites Chart.yaml back to the 1.29.2 baseline (all four deps + appVersion),
# sets the umbrella version to a FRESH -dev<timestamp> baseline (so GHCR
# immutability is satisfied and ArgoCD is FORCED to re-pull even though the istio
# contents are the old 1.29.2), repoints mesh.yaml targetRevision at it, re-vendors
# and PUBLISHES the chart, and commits+pushes to main - so ArgoCD actually rolls
# the LIVE cluster back to 1.29.2. A local file revert alone would not move the
# running mesh.
#
# After a PATCH hop this reliably un-does the roll. After a MINOR hop it may NOT:
# a minor touches CRDs and ArgoCD prune/selfHeal can cascade-delete CRs, which a
# chart-version rollback cannot un-prune. The GUARANTEED clean slate - and the
# REQUIRED recovery after a minor hop - is a full rebuild:
#
#     make down && make up
#
# GIT WRITES TO MAIN (by design): the targetRevision bump must land on the branch
# ArgoCD watches (main) or the mesh Application never re-pulls the baseline chart.
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

: "${GHCR_TOKEN:?scenario-reset needs GHCR_TOKEN (write:packages to publish the baseline chart)}"

CHART="${ROOT}/charts/istio/Chart.yaml"
BASELINE="1.29.2"
FRESH="$(scen_fresh_version patch)"

scen_info "resetting all four deps + appVersion to ${BASELINE}; fresh umbrella baseline ${FRESH}"

# Rewrite the four INDENTED dep `version:` lines + the top-level appVersion back
# to the baseline, then set the col-0 umbrella `version:` to the fresh baseline.
# The indented-vs-col-0 anchoring keeps the three version-ish line kinds separate
# (same invariant the Go bumpChart tests pin).
tmp="$(mktemp)"
sed -E \
  -e 's|^([[:space:]]+version:[[:space:]]*).*$|\1'"${BASELINE}"'|' \
  -e 's|^(appVersion:[[:space:]]*").*(")|\1'"${BASELINE}"'\2|' \
  "${CHART}" > "${tmp}"
mv "${tmp}" "${CHART}"
tmp="$(mktemp)"
sed -E 's|^(version:[[:space:]]*).*$|\1'"${FRESH}"'|' "${CHART}" > "${tmp}"
mv "${tmp}" "${CHART}"

scen_info "repointing mesh targetRevision at ${FRESH}"
scen_set_target_revision "${FRESH}"

scen_info "re-vendoring subcharts (helm dependency update)"
( cd "${ROOT}" && helm dependency update charts/istio )

scen_info "publishing the baseline umbrella chart ${FRESH} to GHCR"
"${ROOT}/scripts/publish-chart.sh"

scen_info "committing + pushing to main (ArgoCD rolls the live cluster back to ${BASELINE})"
git -C "${ROOT}" add charts/istio/Chart.yaml apps/mesh/mesh.yaml
git -C "${ROOT}" commit -m "chore(reset): roll mesh back to istio ${BASELINE} (chart ${FRESH})"
git -C "${ROOT}" push

scen_pass "reset published + pushed; ArgoCD will re-pull ${FRESH} and roll to istio ${BASELINE}"
echo ""
echo "If the cluster was wedged by a MINOR hop, this reset may not fully recover it."
echo "Guaranteed clean slate:  make down && make up"
scen_finish
