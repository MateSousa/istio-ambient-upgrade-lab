#!/usr/bin/env bash
# Build the local demo application images and load them into the kind cluster,
# so that when ArgoCD syncs the app workloads (imagePullPolicy: IfNotPresent,
# no registry prefix) the kubelet finds the image already present and never
# tries to pull it. Must run BEFORE the root app-of-apps is applied.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CLUSTER_NAME="${CLUSTER_NAME:-istio-ambient-lab}"

# name : version : build-context (each context holds its own Dockerfile)
IMAGES=(
  "demo-app-a:0.1.0:demo/app-a/app"
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
