#!/usr/bin/env bash
# scenario-minor.sh - the minor hop (cross-minor, all four deps). RUN THIS LAST.
#
# Bumps all four deps + appVersion 1.29.2 -> 1.30.0 as a Git-synced bump. A minor
# hop is governed by the skew rule and touches CRDs, so it is run last and may end
# in FAIL/ERROR as a legitimate outcome - it emits a measured report; PASS is not
# required.
#
# Recovery hazard: this lab vendors Gateway API v1.2.1 CRDs, whose set differs from
# istio 1.30's; a minor hop can leave them mutually incompatible, and ArgoCD
# prune/selfHeal turn a CRD change into a cascade that can prune every CR of a kind.
# scenario-reset may not un-wedge that, so guaranteed recovery is `make down && make up`.
#
# Writes to main by design (or there is no roll), minting one more GHCR chart version.
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

: "${GHCR_TOKEN:?scenario-minor needs GHCR_TOKEN (write:packages to publish the bumped chart)}"

cat >&2 <<'WARN'
================================ MINOR HOP =====================================
This is the TERMINAL scenario. It crosses a minor (1.29 -> 1.30), touches CRDs,
and may wedge the cluster (Gateway API v1.2.1 <-> istio 1.30 CRD-compat + the
ArgoCD prune/selfHeal CRD cascade). scenario-reset may NOT recover it.
GUARANTEED recovery:  make down && make up
FAIL/ERROR here is a legitimate, measured outcome - not a script bug.
===============================================================================
WARN

CHART_VERSION_TO="$(scen_fresh_version minor)"
scen_info "minor hop: istio 1.29.2 -> 1.30.0 (all four deps + appVersion), publishing umbrella chart ${CHART_VERSION_TO}"

rc=0
(
  cd "${ROOT}/harness"
  go run ./cmd/harness measure \
    --trigger git-bump --hop minor \
    --version-from 1.29.2 --version-to 1.30.0 \
    --chart-version-to "${CHART_VERSION_TO}" \
    --repo-root .. \
    --out ../results.json --out-clients ../clients.json
) || rc=$?

RESULTS="${ROOT}/results.json"
if [[ ! -f "${RESULTS}" ]]; then
  scen_die "no results.json produced (measure rc=${rc}); the minor hop did not even reach a measurement"
fi

verdict="$(scen_result_verdict "${RESULTS}")"
scen_info "MEASURED verdict: ${verdict} (measure rc=${rc})"
scen_info "PASS is NOT required for the minor hop; the measured report is the deliverable"

scen_info "rendering the Markdown report"
(
  cd "${ROOT}/harness"
  go run ./cmd/harness report --in ../results.json --clients ../clients.json --out ../report.md
) || scen_info "report render failed (the raw results.json is still the measured record)"

echo ""
echo "MINOR HOP COMPLETE (verdict: ${verdict}). If the cluster is wedged, recover with: make down && make up"
# Exit 0: the job is to produce a measured report; the verdict lives in
# results.json / report.md, not in this script's exit code.
exit 0
