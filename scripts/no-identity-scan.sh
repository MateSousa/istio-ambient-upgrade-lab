#!/usr/bin/env bash
# Thin wrapper over the Go hygiene scanner (harness/internal/scan, `harness scan`);
# all detection logic lives in Go. Exit 0 = clean tree, non-zero = a finding or the
# scan could not run (fail-closed: a missing Go toolchain exits 2 rather than
# passing an unscanned tree).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "FAIL: go toolchain not found; cannot run the hygiene scanner (failing closed)." >&2
  exit 2
fi

cd "$ROOT/harness"
exec go run ./cmd/harness scan --repo-root ..
