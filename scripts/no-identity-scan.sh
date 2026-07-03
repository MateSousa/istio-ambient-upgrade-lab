#!/usr/bin/env bash
# Thin wrapper over the tested Go hygiene scanner (harness/internal/scan, run via
# `harness scan`). This script only locates the repo root + harness module and
# execs the subcommand; all detection logic lives in Go (no grep fallback, so
# there is a single implementation that cannot drift).
#
# Exit-code contract (the only thing any caller relies on: 0 vs non-zero;
# verify.sh pipes stdout to /dev/null, the Makefile ignores it):
#   0        clean tree
#   non-zero a finding, or the scan could not run -> the push/gate is blocked
# `harness scan` itself returns 0/1/2 (clean/finding/IO-error), but `go run`
# collapses any non-zero program exit to 1; that is fine here because no caller
# distinguishes 1 from 2. If the Go toolchain is absent this FAILS CLOSED
# (exit 2, this script's own exit) rather than passing an unscanned tree.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v go >/dev/null 2>&1; then
  echo "FAIL: go toolchain not found; cannot run the hygiene scanner (failing closed)." >&2
  exit 2
fi

cd "$ROOT/harness"
exec go run ./cmd/harness scan --repo-root ..
