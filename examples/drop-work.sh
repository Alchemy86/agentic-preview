#!/usr/bin/env bash
#
# Drop a WHOLE work id: you are finished with the change, and every service it
# previewed goes back to live in one call.
#
# This is the call that guarantees nothing is left behind - every intercept, and every
# Deployment and Service agentic-preview built under this work id, in one go. An
# intercept left in manager state with nobody holding its tunnel makes its own header
# hang rather than falling back to live - see the README's limits.
#
# It works even after agentic-preview has restarted and forgotten what it raised: the
# objects it built carry its own labels, and this finds them by those.
#
# Nothing calls this for you. A preview does not go away because a pull request merged;
# it goes when somebody asks, or when nothing has touched the work id for
# PREVIEW_LIFETIME (24 hours by default), which is a safety net rather than a policy.
#
#   ./drop-work.sh 1234
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-1234}"

curl -sS -XDELETE "$BASE/previews/$WORK_ID"
echo

# A 404 here means the work id had nothing live, which is the expected answer to a
# teardown that runs twice.
