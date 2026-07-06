#!/usr/bin/env bash
# Build the demo app images and kind-load them. The apps use imagePullPolicy
# IfNotPresent with no registry prefix, so this must run before the app-of-apps.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLUSTER_NAME="${CLUSTER_NAME:-istio-ambient-lab}"

# name : version : build-context (each context holds its own Dockerfile)
IMAGES=(
  "demo-app-a:0.1.0:demo/app-a/app"
  "demo-app-b:0.1.0:demo/app-b/app"
  "demo-app-c:0.1.0:demo/app-c/app"
  "harness:0.1.0:harness"
)

for spec in "${IMAGES[@]}"; do
  name="${spec%%:*}"
  rest="${spec#*:}"
  version="${rest%%:*}"
  context="${rest#*:}"
  tag="${name}:${version}"

  echo "==> docker build ${tag} (context ${context})"
  docker build -t "${tag}" "${context}"

  echo "==> kind load docker-image ${tag} -> ${CLUSTER_NAME}"
  kind load docker-image "${tag}" --name "${CLUSTER_NAME}"
done

echo "PASS: built and loaded demo app images."
