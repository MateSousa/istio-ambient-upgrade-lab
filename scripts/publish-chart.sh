#!/usr/bin/env bash
# Build, package, and push the umbrella chart to the private GHCR OCI registry.
#   - helm dependency build : vendor base/cni/istiod/ztunnel from upstream
#   - helm package          : produce istio-<version>.tgz
#   - helm registry login   : authenticate to ghcr.io with a PAT (write:packages)
#   - helm push             : push to oci://ghcr.io/matesousa/charts
# The package stays PRIVATE (GHCR default for a new package); we never flip it
# public. GHCR_TOKEN must be exported and is never written to disk or echoed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

: "${GHCR_TOKEN:?GHCR_TOKEN must be set to a PAT with write:packages scope}"
GHCR_USER="${GHCR_USER:-matesousa}"
GHCR_REPO="${GHCR_REPO:-ghcr.io/${GHCR_USER}/charts}"
CHART_DIR="charts/istio"
CHART_VERSION="$(awk '/^version:/ {print $2; exit}' "${CHART_DIR}/Chart.yaml")"
DIST="${ROOT}/dist"

echo "==> helm dependency build (${CHART_DIR})"
helm dependency build "${CHART_DIR}"

echo "==> helm package (version ${CHART_VERSION})"
mkdir -p "${DIST}"
helm package "${CHART_DIR}" --destination "${DIST}"

echo "==> helm registry login ghcr.io"
printf '%s' "${GHCR_TOKEN}" | helm registry login ghcr.io -u "${GHCR_USER}" --password-stdin

echo "==> helm push istio-${CHART_VERSION}.tgz -> oci://${GHCR_REPO}"
helm push "${DIST}/istio-${CHART_VERSION}.tgz" "oci://${GHCR_REPO}"

echo "PASS: published istio ${CHART_VERSION} to oci://${GHCR_REPO} (private)."
