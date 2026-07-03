#!/usr/bin/env bash
# scenario-patch.sh - the PATCH hop (same-minor ztunnel bump), slice 8.
#
# Bumps ztunnel 1.29.2 -> 1.29.5 as a Git-synced version bump (the AC-satisfying
# path): the harness git-bump trigger rewrites the ztunnel dep + umbrella version
# in Chart.yaml, `helm dependency update`s, publishes a FRESH umbrella chart to
# GHCR, bumps the mesh Application targetRevision, and commits+pushes to main.
# ArgoCD then syncs the roll and the harness measures it. Skew rules are trivially
# met for a same-minor hop, so the expected outcome is PASS.
#
# GIT WRITES TO MAIN (by design): a throwaway branch is OFF the table. demo-root
# tracks HEAD and the mesh Application only rolls when the published chart +
# targetRevision bump actually LAND on the branch ArgoCD watches (main) - a bump
# stranded on a side branch produces no roll, which the analyzer surfaces as ERROR
# no-rollout-observed. Each run therefore commits to main and accumulates one more
# immutable GHCR chart version (prune later via the GHCR UI / `gh api` if desired).
set -euo pipefail

# shellcheck source=scripts/scenarios/scenario-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scenario-lib.sh"

: "${GHCR_TOKEN:?scenario-patch needs GHCR_TOKEN (write:packages to publish the bumped chart)}"

CHART_VERSION_TO="$(scen_fresh_version patch)"
scen_info "patch hop: ztunnel 1.29.2 -> 1.29.5, publishing umbrella chart ${CHART_VERSION_TO}"

rc=0
(
  cd "${ROOT}/harness"
  go run ./cmd/harness measure \
    --trigger git-bump --hop patch \
    --ztunnel-from 1.29.2 --ztunnel-to 1.29.5 \
    --chart-version-to "${CHART_VERSION_TO}" \
    --repo-root .. \
    --out ../results.json --out-clients ../clients.json
) || rc=$?

RESULTS="${ROOT}/results.json"
[[ -f "${RESULTS}" ]] || scen_die "no results.json produced (measure rc=${rc})"

verdict="$(scen_result_verdict "${RESULTS}")"
scen_info "verdict: ${verdict} (measure rc=${rc})"
if [[ "${verdict}" == "PASS" ]]; then
  scen_pass "patch hop PASS - a same-minor ztunnel roll stayed within the per-node reset bound"
else
  scen_fail "patch hop expected PASS, got ${verdict} (see results.json / report.md)"
fi

scen_info "rendering the Markdown report"
(
  cd "${ROOT}/harness"
  go run ./cmd/harness report --in ../results.json --clients ../clients.json --out ../report.md
) || scen_fail "report render failed"

scen_finish
