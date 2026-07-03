#!/usr/bin/env bash
# Tear the lab down: delete the kind cluster. Nothing else persists.
set -euo pipefail

CLUSTER_NAME="istio-ambient-lab"

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  kind delete cluster --name "${CLUSTER_NAME}"
  echo "PASS: deleted cluster ${CLUSTER_NAME}."
else
  echo "cluster ${CLUSTER_NAME} not found, nothing to do."
fi
