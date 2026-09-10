#!/usr/bin/env bash
#
# Drop ONE service from a work id: that repo's PR merged, while the rest of the change
# is still in flight. The work id's other services keep routing.
#
#   ./drop-service.sh 4821 shop checkout-api
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-4821}"
NAMESPACE="${2:-shop}"
WORKLOAD="${3:-checkout-api}"

curl -sS -XDELETE "$BASE/previews/$WORK_ID/$NAMESPACE/$WORKLOAD"
echo

# The node-agent Job behind this workload is released only once no OTHER work id is
# still previewing the same service - two work ids on one workload share a single Job
# and a single tunnel. If the response carries a "work" field, that id still has
# services live.
