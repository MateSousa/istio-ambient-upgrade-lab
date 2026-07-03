#!/usr/bin/env bash
# scenario-minor.sh - the MINOR hop (cross-minor, all four deps), slice 8.
# ==============================================================================
# TERMINAL SCENARIO - RUN THIS LAST.
# ==============================================================================
#
# Bumps ALL FOUR deps (base/cni/istiod/ztunnel) + appVersion 1.29.2 -> 1.30.0 and
# the umbrella version, as a Git-synced bump. A minor hop crosses a minor, is
# governed by the skew rule (istiod-first, <=1-minor data-plane skew), and TOUCHES
# CRDs - which is why it is deliberately run last and is allowed to end in
# FAIL/ERROR as a legitimate, INFORMATIVE outcome (it emits a MEASURED report;
# PASS is NOT required here).
#
# HAZARD WARNING (why recovery may need a full rebuild):
#   * This lab vendors Gateway API v1.2.1 CRDs. Istio 1.30's bundled/expected
#     Gateway API CRD set differs; a minor hop can leave the Gateway API and istio
#     CRDs in a mutually-incompatible state.
#   * ArgoCD prune + selfHeal make CRD changes a CASCADE: a renamed/removed CRD can
#     prune every CR of that kind mesh-wide.
#   * Because of that cascade, scenario-reset.sh may NOT be able to un-wedge a
#     minor-hop cluster (it rolls the chart back, but cannot always un-prune CRs).
#   * GUARANTEED recovery after a minor hop is a clean rebuild:  make down && make up
#
# GIT WRITES TO MAIN (by design): like the patch scenario, the bump must land on
# the branch ArgoCD watches (main) or there is no roll. This accumulates one more
# immutable GHCR chart version.
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
# Exit 0: the scenario's job is to PRODUCE a measured report, which it did. The
# verdict lives in results.json / report.md, not in this script's exit code.
exit 0
