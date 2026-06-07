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

    # Resume: if a prior run of THIS run-id snapshotted its committed working
    # tree to refs/horde/snapshot/<run-id> (see the terminate path below),
    # recover it so committed-but-unpushed work survives a task stop (e.g. a
    # phase timeout or a spot interruption). The snapshot wins over the
    # branch checkout. Workflow-agnostic and non-fatal — if the ref is absent
    # (first run) or the fetch fails, fall through to the normal checkout.
    SNAPSHOT_REF="refs/horde/snapshot/${RUN_ID}"
    if [ -n "${RUN_ID:-}" ] && \
       git fetch origin "${SNAPSHOT_REF}:${SNAPSHOT_REF}" 2>/dev/null && \
       git rev-parse --verify --quiet "${SNAPSHOT_REF}" >/dev/null; then
        echo "Recovering snapshot ${SNAPSHOT_REF}" >&2
        if ! git checkout -f "${SNAPSHOT_REF}"; then
            echo "ERROR: git checkout of snapshot ref failed" >&2
            exit 3
        fi
    elif [ -n "${BRANCH:-}" ]; then
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
    mkdir -p /home/horde/.claude
    aws s3 sync "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/sessions/" /home/horde/.claude/ \
        || echo "WARNING: session restore failed (continuing)" >&2

    # Run orc in the background so a SIGTERM/SIGINT (ECS StopTask, incl. a
    # spot interruption) can be forwarded to it; the upload block below must
    # still run so artifacts, session state, and the snapshot aren't lost
    # when the task is stopped mid-run. Tee orc's output to a log so we can
    # parse a rate-limit reset time after it exits (orc emits a line like
    # "usage limit reached, resets at HH:MM" on exit code 4).
    # Capture orc's output to a log file (so we can parse a rate-limit reset
    # time after it exits) and stream it live to the container log via a
    # background `tail`. We deliberately do NOT use `... > >(tee log)`: the
    # process-substitution tee keeps the stdout pipe open and deadlocks the
    # script's final `exit` on Fargate — the container then never stops.
    ORC_LOG=/tmp/orc-output.log
    : > "$ORC_LOG"
    tail -f "$ORC_LOG" 2>/dev/null &
    TAIL_PID=$!
    # `eval "exec $ORC_CMD"` makes the backgrounded subshell BECOME orc, so
    # ORC_PID is orc's real PID and a forwarded SIGTERM reaches orc directly.
    # (A plain `eval "$ORC_CMD" &` leaves $! pointing at the eval wrapper, so
    # the signal never reaches orc and its interrupt-save never runs.)
    { eval "exec $ORC_CMD"; } > "$ORC_LOG" 2>&1 &
    ORC_PID=$!
    # On SIGTERM (ECS StopTask, incl. a spot interruption) forward it to orc
    # AND re-wait so orc finishes saving its interrupted session before the
    # upload/snapshot block below runs. Without the second wait, the initial
    # `wait` returns the moment the signal arrives (exit 143) and we would
    # race orc's save — uploading a half-written session/snapshot. The ECS
    # container stopTimeout (120s) gives orc room to finish before SIGKILL.
    term_handler() { kill -TERM "$ORC_PID" 2>/dev/null; wait "$ORC_PID"; }
    trap term_handler TERM INT
    wait "$ORC_PID"
    EXIT_CODE=$?
    trap - TERM INT
    kill "$TAIL_PID" 2>/dev/null  # stop streaming; tee-free so the container can exit

    # Snapshot committed-but-unpushed work to a per-run ref so a future
    # `horde retry` recovers the working tree (#32). Workflow-agnostic: it
    # pushes whatever HEAD points at; it does NOT assume the workflow created
    # a branch or ran a push phase. Force-push so a re-snapshot of the same
    # run overwrites the prior one. Non-fatal.
    if [ -n "${RUN_ID:-}" ] && git -C /workspace rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
        git -C /workspace push --force origin "HEAD:refs/horde/snapshot/${RUN_ID}" \
            || echo "WARNING: snapshot push failed (continuing)" >&2
    fi

    # Record a machine-readable stop reason so the store reflects WHY the run
    # ended without re-deriving it from logs (#31). The status Lambda records
    # the ECS-level stopCode/stoppedReason; this captures the orc-level
    # rate-limit reset time, which only the run's own output carries.
    STOP_REASON_FILE=/tmp/stop-reason.json
    RESETS_AT=$(grep -oiE 'resets? at[: ]+[0-9]{1,2}:[0-9]{2}([ap]m)?' "$ORC_LOG" 2>/dev/null | tail -1 | grep -oiE '[0-9]{1,2}:[0-9]{2}([ap]m)?' || true)
    printf '{"exit_code":%d,"resets_at":"%s"}\n' "$EXIT_CODE" "${RESETS_AT:-}" > "$STOP_REASON_FILE"
    aws s3 cp "$STOP_REASON_FILE" "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/stop-reason.json" \
        || echo "WARNING: stop-reason upload failed" >&2

    # Always persist session state, even on failure, so retry can resume.
    if [ -d /home/horde/.claude ]; then
        aws s3 sync /home/horde/.claude/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/sessions/" \
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
