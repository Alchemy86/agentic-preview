#!/usr/bin/env bash
#
# Drop ONE service from a work id: you are finished with that repository's part of the
# change, while the rest of it is still being looked at. The work id's other services
# keep routing.
#
# Teardown is explicit and this is one of the two ways to ask for it. A pull request
# merging is NOT one of them: work carries on against a preview after the merge, and
# agentic-preview has no notion of a pull request at all.
#
# This removes the intercept AND the Deployment and Service agentic-preview built for
# this service under this work id - and only those, found by the labels it put on them.
#
#   ./drop-service.sh 1234 shop checkout-api
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-1234}"
NAMESPACE="${2:-shop}"
WORKLOAD="${3:-checkout-api}"

curl -sS -XDELETE "$BASE/previews/$WORK_ID/$NAMESPACE/$WORKLOAD"
echo

# The node-agent Job behind this workload is released only once no OTHER work id is
# still previewing the same service - two work ids on one workload share a single Job
# and a single tunnel. If the response carries a "work" field, that id still has
# services live.
