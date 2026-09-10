#!/usr/bin/env bash
#
# Drop a WHOLE work id: the change is done, every service it previewed goes back to
# live in one call.
#
# Worth wiring into the pipeline's teardown even when each PR already calls
# drop-service.sh, because this is the call that guarantees nothing is left behind. An
# intercept left in manager state with nobody holding its tunnel makes its own header
# hang rather than falling back to live - see the README's limits.
#
#   ./drop-work.sh 4821
#
set -euo pipefail

BASE="${AGENTIC_PREVIEW_URL:-http://agentic-preview.agentic-preview.svc.cluster.local}"

WORK_ID="${1:-4821}"

curl -sS -XDELETE "$BASE/previews/$WORK_ID"
echo

# A 404 here means the work id had nothing live, which is the expected answer to a
# teardown that runs twice.
