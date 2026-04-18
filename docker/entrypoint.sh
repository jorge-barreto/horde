#!/bin/bash
set -uo pipefail

# Explicit guards before set -u would trip with exit 1: horde uses exit 3
# to distinguish setup errors from run failures when mapping status.
if [ -z "${REPO_URL:-}" ]; then echo "ERROR: REPO_URL not set" >&2; exit 3; fi
if [ -z "${TICKET:-}" ]; then echo "ERROR: TICKET not set" >&2; exit 3; fi
if [ -z "${GIT_TOKEN:-}" ]; then echo "ERROR: GIT_TOKEN not set" >&2; exit 3; fi

# GIT_ASKPASS is set in the Dockerfile for container-wide availability.
# gh CLI uses the same token as git push.
export GH_TOKEN="${GIT_TOKEN:-}"

# Prevent git from hanging on an interactive credential prompt if
# GIT_ASKPASS silently fails. Non-TTY containers would otherwise block
# forever waiting for input that can never arrive.
export GIT_TERMINAL_PROMPT=0

# The workspace directory is bind-mounted from the host and may be owned by a
# different UID. Git ≥ 2.35.2 rejects operations in such directories unless
# they are explicitly marked safe.
git config --global --add safe.directory /workspace

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

# Build orc command
ORC_ARGS="--auto --no-color"
if [ -n "${WORKFLOW:-}" ]; then
    ORC_CMD="orc run -w $WORKFLOW $TICKET $ORC_ARGS ${ORC_EXTRA_ARGS:-}"
else
    ORC_CMD="orc run $TICKET $ORC_ARGS ${ORC_EXTRA_ARGS:-}"
fi

# ECS path: sync session state and artifacts through S3 so retries can
# resume. Docker mode skips this block — bind mounts handle persistence.
if [ -n "${ARTIFACTS_BUCKET:-}" ]; then
    # Restore prior agent session state (Claude CLI reads ~/.claude/projects/).
    # First-run prefix is empty; sync handles that as a no-op. Failures are
    # non-fatal — a missing prior session means orc --resume falls back to a
    # fresh start, which is better than aborting the run.
    mkdir -p /root/.claude
    aws s3 sync "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/sessions/" /root/.claude/ \
        || echo "WARNING: session restore failed (continuing)" >&2

    # Run orc in the background so a SIGTERM/SIGINT (ECS StopTask) can be
    # forwarded to it; the upload block below must still run so artifacts
    # and session state aren't lost when the task is stopped mid-run.
    eval "$ORC_CMD" &
    ORC_PID=$!
    trap 'kill -TERM "$ORC_PID" 2>/dev/null' TERM INT
    wait "$ORC_PID"
    EXIT_CODE=$?
    trap - TERM INT

    # Always persist session state, even on failure, so retry can resume.
    if [ -d /root/.claude ]; then
        aws s3 sync /root/.claude/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/sessions/" \
            || echo "WARNING: session upload failed" >&2
    fi
    if [ -d .orc/artifacts/ ]; then
        aws s3 cp .orc/artifacts/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/artifacts/" --recursive || echo "WARNING: artifact upload failed" >&2
    fi
    if [ -d .orc/audit/ ]; then
        aws s3 cp .orc/audit/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/audit/" --recursive || echo "WARNING: audit upload failed" >&2
    fi
    exit $EXIT_CODE
fi

# Docker: exec orc directly so it receives signals from docker stop.
# With --init on docker run, tini reaps zombies and forwards signals.
# horde reads the exit code from docker inspect, not a marker file.
exec $ORC_CMD
