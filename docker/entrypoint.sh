#!/bin/bash
set -uo pipefail

# Clone repo using credential helper (avoids token in process args and .git/config)
export GIT_ASKPASS="/usr/local/bin/git-askpass.sh"
if ! git clone "https://${REPO_URL}" /workspace; then
    echo "ERROR: git clone failed" >&2
    exit 3
fi
cd /workspace
if [ -n "${BRANCH:-}" ]; then
    if ! git checkout "$BRANCH"; then
        echo "ERROR: git checkout failed for branch ${BRANCH}" >&2
        exit 3
    fi
fi

# Run orc
if [ -n "${WORKFLOW:-}" ]; then
    orc run -w "$WORKFLOW" "$TICKET" --auto --no-color
else
    orc run "$TICKET" --auto --no-color
fi
EXIT_CODE=$?

# Upload artifacts to S3 (ECS only — env vars are absent in docker mode)
if [ -n "${ARTIFACTS_BUCKET:-}" ]; then
    if [ -d .orc/artifacts/ ]; then
        aws s3 cp .orc/artifacts/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/artifacts/" --recursive || echo "WARNING: artifact upload failed" >&2
    fi
    if [ -d .orc/audit/ ]; then
        aws s3 cp .orc/audit/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/audit/" --recursive || echo "WARNING: audit upload failed" >&2
    fi
fi

exit $EXIT_CODE
