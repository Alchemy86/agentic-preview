#!/usr/bin/env bash
#
# Raise a preview: build the PR's image as a preview of one service, and divert
# requests carrying one header value to it. One call does the whole job.
#
# agentic-preview reads the LIVE Deployment and copies it - the same environment, the
# same ConfigMap and Secret references, the same pull credentials, the same probes and
# the same service account - with your image in place of the live one. Then it creates a
# Service in front of the copy and points the intercept at it.
#
# The image reference is used VERBATIM. agentic-preview does not know or care how that
# image came to exist, what its tag means, or which registry it is in. Building it and
# pushing it is your pipeline's job; running what you name is this service's.
#
#   ./raise-preview.sh 1234 checkout-api shop registry.example.com/checkout-api:pr-1234
#
# Calling this again with the same work id ADDS to that id's set, which is how a change
# spanning several repos ends up reachable behind one header. Calling it again with the
# same work id AND service rolls that one preview forward to whatever image you name.
# Either way the whole work id's expiry is extended.
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

# Any string you like: an issue number, a branch name, somebody's initials. It is the
# header value, and agentic-preview never reads anything out of it.
WORK_ID="${1:-1234}"
WORKLOAD="${2:-checkout-api}"
NAMESPACE="${3:-shop}"
# The image to run, verbatim.
IMAGE="${4:-registry.example.com/checkout-api:pr-1234}"
# The port identifier on the LIVE workload being intercepted: a service port name or
# number. The port on the preview side is read off the Service that gets built.
PORT="${5:-http}"

curl -sS -XPOST "$BASE/previews" \
  -H 'content-type: application/json' \
  -d @- <<JSON
{
  "workId":    "$WORK_ID",
  "workload":  "$WORKLOAD",
  "namespace": "$NAMESPACE",
  "image":     "$IMAGE",
  "replicas":  1,
  "port":      "$PORT"
}
JSON
echo

# The response's "work" field lists every service now live under this work id, the
# header that reaches all of them, and when the id expires if nothing touches it. Send
# that header to the live ingress to hit the preview:
#
#   curl -H "x-preview: $WORK_ID" https://your-ingress/checkout
#
# Everything without the header reaches the live pod, unchanged and unaware.
