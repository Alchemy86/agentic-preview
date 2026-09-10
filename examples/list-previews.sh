#!/usr/bin/env bash
#
# What is live: every work id, the services it spans, and the header that reaches them.
#
# With a work id argument, just that one:
#
#   ./list-previews.sh          # everything
#   ./list-previews.sh 1234     # one work id
#
# Each work id reports `expiresAt`, and so does each service under it: when the preview
# is swept if nothing touches it before then. It is reported so you can see you are
# about to lose one and raise it again to extend it, rather than find out afterwards.
#
# Each preview agentic-preview built also reports the `image` it is running. If you run
# an image-retention policy, that is the list to exclude from it - a preview that
# outlives its image survives until its pod is replaced and then cannot pull.
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

if [ $# -gt 0 ]; then
  curl -sS "$BASE/previews/$1"
  echo
  exit 0
fi

echo "=== previews ==="
curl -sS "$BASE/previews"
echo

# /readyz is worth reading alongside it. It reports the manager session, whether a
# session credential was obtained, the allowed-namespace list, and one entry per live
# tunnel keyed by "<workload>.<namespace>". A preview whose disposition is ACTIVE but
# which has no tunnel here is the rollout window described in the README.
echo
echo "=== session and tunnels ==="
curl -sS "$BASE/readyz"
echo
