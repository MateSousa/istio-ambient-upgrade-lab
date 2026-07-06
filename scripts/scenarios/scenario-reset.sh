#!/usr/bin/env bash
# scenario-reset.sh - roll the running cluster back to the 1.29.2 baseline.
#
# Cluster-deep, not a working-tree `git checkout`: it rewrites Chart.yaml back to
# 1.29.2 but sets a fresh -dev<timestamp> umbrella version (so GHCR immutability
# holds and ArgoCD is forced to re-pull), repoints targetRevision, re-vendors,
# publishes, and commits+pushes to main - so ArgoCD actually rolls the live cluster
# back. A local file revert alone would not move the running mesh.
#
# Reliable after a patch hop. After a minor hop it may not recover (CRD prune
# cascade can delete CRs a chart rollback cannot un-prune); the guaranteed clean
# slate is `make down && make up`.
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

: "${GHCR_TOKEN:?scenario-reset needs GHCR_TOKEN (write:packages to publish the baseline chart)}"

CHART="${ROOT}/charts/istio/Chart.yaml"
BASELINE="1.29.2"
FRESH="$(scen_fresh_version patch)"

scen_info "resetting all four deps + appVersion to ${BASELINE}; fresh umbrella baseline ${FRESH}"

# Indented `version:` lines + appVersion -> baseline; the col-0 umbrella `version:`
# -> fresh baseline. The indent-vs-col-0 anchoring keeps the three line kinds apart.
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
