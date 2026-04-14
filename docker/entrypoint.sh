#!/bin/bash
set -uo pipefail

# Clone repo using credential helper (avoids token in process args and .git/config)
export GIT_ASKPASS="/usr/local/bin/git-askpass.sh"
if ! git clone "https://${REPO_URL}" /workspace; then
    echo "ERROR: git clone failed" >&2
    exit 3
fi
cd /workspace || { echo "ERROR: cd /workspace failed" >&2; exit 3; }
if [ -n "${BRANCH:-}" ]; then
    if ! git checkout "$BRANCH"; then
        echo "ERROR: git checkout failed for branch ${BRANCH}" >&2
        exit 3
    fi
fi

# Restore state from a previous run (resume)
if [ -d "${RESUME_DIR:-}" ]; then
    echo "Restoring state from previous run..."
    mkdir -p .orc
    if [ -d "$RESUME_DIR/artifacts" ]; then
        cp -a "$RESUME_DIR/artifacts/." .orc/artifacts/
    fi
    if [ -d "$RESUME_DIR/audit" ]; then
        cp -a "$RESUME_DIR/audit/." .orc/audit/
    fi
fi

# Run orc
ORC_ARGS="--auto --no-color"
if [ -n "${RETRY_PHASE:-}" ]; then
    ORC_ARGS="$ORC_ARGS --retry $RETRY_PHASE"
fi
if [ -n "${WORKFLOW:-}" ]; then
    orc run -w "$WORKFLOW" "$TICKET" $ORC_ARGS
else
    orc run "$TICKET" $ORC_ARGS
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
