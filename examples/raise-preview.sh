#!/usr/bin/env bash
#
# Raise a preview: divert requests carrying one header value to a preview Service.
#
# This is the call a pipeline makes once it has deployed the PR build and given it a
# Service. agentic-preview does not create that Deployment or Service - it only routes
# to it.
#
# Calling this again with the same work id ADDS to that id's set, which is how a change
# spanning several repos ends up reachable behind one header. Calling it again with the
# same work id AND service is idempotent: a pipeline retry does not disturb a live
# preview.
#
#   ./raise-preview.sh 4821 checkout-api shop checkout-api-preview-pr4821
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-4821}"
WORKLOAD="${2:-checkout-api}"
NAMESPACE="${3:-shop}"
PREVIEW_SERVICE="${4:-checkout-api-preview-pr4821}"
# The port on the preview Service. agentic-preview resolves the Service by DNS and
# cannot read it to discover the port, which is why this is a request field.
PREVIEW_PORT="${5:-80}"
# The port identifier on the LIVE workload being intercepted: a service port name or
# number.
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

# The response's "work" field lists every service now live under this work id, and the
# header that reaches all of them. Send that header to the live ingress to hit the
# preview:
#
#   curl -H "x-preview: $WORK_ID" https://your-ingress/checkout
#
# `previewService` also accepts "name.namespace" if the preview Service sits in a
# different namespace from the workload - both namespaces must be in
# ALLOWED_NAMESPACES.
