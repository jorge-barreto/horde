#!/bin/bash
set -uo pipefail

# GIT_ASKPASS is set in the Dockerfile for container-wide availability.
# gh CLI uses the same token as git push.
export GH_TOKEN="${GIT_TOKEN:-}"

if [ -d /workspace/.git ]; then
    # Restart — workspace already exists from a previous run.
    # Skip clone, go straight to running orc.
    cd /workspace
else
    # First run — clone the repo.
    # Use init+fetch instead of clone — volume mounts may pre-create /workspace/.
    mkdir -p /workspace
    cd /workspace || { echo "ERROR: cd /workspace failed" >&2; exit 3; }
    git init
    git remote add origin "https://${REPO_URL}"
    if ! git fetch origin; then
        echo "ERROR: git fetch failed" >&2
        exit 3
    fi
    if [ -n "${BRANCH:-}" ]; then
        if ! git checkout "$BRANCH"; then
            echo "ERROR: git checkout failed for branch ${BRANCH}" >&2
            exit 3
        fi
    else
        DEFAULT_BRANCH=$(git symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's|^refs/remotes/origin/||')
        if ! git checkout "${DEFAULT_BRANCH:-main}"; then
            echo "ERROR: git checkout failed" >&2
            exit 3
        fi
    fi
fi

# Run orc
ORC_ARGS="--auto --no-color"
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
    # ECS: exit normally so the task stops (billed per second)
    exit $EXIT_CODE
fi

# Docker: write exit code marker and keep container alive for shell/retry
echo "$EXIT_CODE" > /workspace/.horde-exit-code
exec sleep infinity
