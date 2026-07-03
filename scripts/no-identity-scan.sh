#!/usr/bin/env bash
# Fail if any proprietary identifier leaks into this public repo.
# Greps the whole tree (excluding .git and vendored subchart .tgz artifacts) for
# private org names, AWS account IDs, ECR hosts, internal FQDNs, and internal
# namespaces/emails. Exits non-zero on any hit. Runs pre-push and in verify.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PATTERN='example-org|123456789012|123456789012|123456789012|123456789012|dkr\.ecr|example\.dev|opentelemetry-operator-system|platform@example-org'

# -I skips binary files. Exclusions:
#   .git                  - version-control internals
#   *.tgz                 - vendored subchart tarballs from helm dependency build
#   no-identity-scan.sh   - this file; it necessarily contains the pattern literal
if grep -rInE -i "$PATTERN" . \
    --exclude-dir=.git \
    --exclude='*.tgz' \
    --exclude='no-identity-scan.sh'; then
  echo ""
  echo "FAIL: proprietary identifier(s) found above. Remove before pushing." >&2
  exit 1
fi

echo "PASS: no proprietary identifiers found."
