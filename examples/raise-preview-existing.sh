#!/usr/bin/env bash
#
# Raise a preview of a Service YOU deployed: routing only, nothing built.
#
# This is the original half of agentic-preview and it has not gone anywhere. Use it when
# the preview is not a copy of a live Deployment - a different chart, a stack of
# services stood up together, anything your pipeline already knows how to deploy.
# agentic-preview then does the intercept and nothing else.
#
#   ./raise-preview-existing.sh 1234 checkout-api shop checkout-api-preview
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-1234}"
WORKLOAD="${2:-checkout-api}"
NAMESPACE="${3:-shop}"
PREVIEW_SERVICE="${4:-checkout-api-preview}"
# The port on the preview Service. agentic-preview resolves the Service by DNS and
# cannot read a Service it did not create, which is why this is a request field here and
# not in raise-preview.sh.
PREVIEW_PORT="${5:-80}"
# The port identifier on the LIVE workload being intercepted.
PORT="${6:-80}"

curl -sS -XPOST "$BASE/previews" \
  -H 'content-type: application/json' \
  -d @- <<JSON
{
  "workId":         "$WORK_ID",
  "workload":       "$WORKLOAD",
  "namespace":      "$NAMESPACE",
  "previewService": "$PREVIEW_SERVICE",
  "previewPort":    $PREVIEW_PORT,
  "port":           "$PORT"
}
JSON
echo

# `previewService` also accepts "name.namespace" if the preview Service sits in a
# different namespace from the workload - both namespaces must be in
# ALLOWED_NAMESPACES.
