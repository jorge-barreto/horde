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

# Save base ref for workspace patch generation on completion
git rev-parse HEAD > /workspace/.horde-base-ref

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
    # Apply code changes from previous run
    if [ -f "$RESUME_DIR/workspace.patch" ] && [ -s "$RESUME_DIR/workspace.patch" ]; then
        echo "Applying workspace changes from previous run..."
        if ! git apply "$RESUME_DIR/workspace.patch"; then
            echo "WARNING: failed to apply workspace patch — starting from clean checkout" >&2
        fi
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

# Push work to a horde branch so it survives container removal
HORDE_BRANCH="horde/${TICKET}"
if git rev-parse --verify HEAD >/dev/null 2>&1; then
    BASE_REF=$(cat /workspace/.horde-base-ref 2>/dev/null || true)
    # Only push if there are changes since the base
    if [ -n "$BASE_REF" ] && [ "$(git rev-parse HEAD)" != "$BASE_REF" ] || ! git diff --quiet HEAD 2>/dev/null; then
        git checkout -B "$HORDE_BRANCH" 2>/dev/null
        # Stage and commit any uncommitted work
        if ! git diff --quiet HEAD 2>/dev/null || ! git diff --cached --quiet HEAD 2>/dev/null; then
            git add -A
            git commit -m "horde: work in progress (run ${RUN_ID})" --no-verify 2>/dev/null || true
        fi
        git push origin "$HORDE_BRANCH" --force 2>/dev/null && echo "Pushed to branch $HORDE_BRANCH" || echo "WARNING: failed to push to $HORDE_BRANCH" >&2
    fi
fi

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
