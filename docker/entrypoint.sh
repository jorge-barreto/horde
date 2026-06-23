#!/bin/bash
set -uo pipefail

# ORC_SUBCMD selects the orc subcommand (default "run"). REPO_URL and TICKET
# are required only for `orc run` (the clone + ticket-driven run path); a
# non-run subcommand (eval/validate/test via `horde exec`) needs neither —
# it executes against an already-seeded/cloned workspace and takes its own
# args. GIT_TOKEN is always required (git credential helper).
ORC_SUBCMD="${ORC_SUBCMD:-run}"
if [ "$ORC_SUBCMD" = "run" ]; then
    if [ -z "${REPO_URL:-}" ]; then echo "ERROR: REPO_URL not set" >&2; exit 3; fi
    if [ -z "${TICKET:-}" ]; then echo "ERROR: TICKET not set" >&2; exit 3; fi
fi
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

# ECS resume: Fargate has no persistent filesystem, so a prior run's working
# tree only survives if it was synced to S3 (the terminate path below does
# that). Restore the whole /workspace — committed AND uncommitted work, plus
# .git — so `horde retry` resumes exactly where the run left off, the same way
# the on-host workspace persists for the Docker provider. If S3 has a prior
# workspace, this populates /workspace/.git and the clone below is skipped.
if [ -n "${ARTIFACTS_BUCKET:-}" ] && [ ! -d /workspace/.git ]; then
    mkdir -p /workspace
    WS_PREFIX="s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/workspace/"
    # Does a prior workspace exist in S3? (resume vs. first run)
    if aws s3 ls "$WS_PREFIX" >/dev/null 2>&1 && [ -n "$(aws s3 ls "$WS_PREFIX")" ]; then
        # Resume: restore is REQUIRED. A transient restore failure must NOT
        # fall through to a fresh clone — the terminate-path `s3 sync --delete`
        # would then mirror the clone over the prefix and destroy the prior
        # run's committed+uncommitted work irreversibly. Fail loudly instead.
        if ! aws s3 sync "$WS_PREFIX" /workspace/; then
            echo "ERROR: workspace restore failed for resume; refusing to continue and overwrite S3 state" >&2
            exit 3
        fi
    fi
    # First run (empty prefix): no-op; fall through to the clone below.
fi

if [ -d /workspace/.git ]; then
    # Restart/resume — workspace already exists (on-host bind mount for Docker,
    # or restored from S3 for ECS). Skip clone, go straight to running orc.
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

# Build orc command. ORC_SUBCMD selects the orc subcommand (default "run").
# The run-only flags (-w / --auto / --no-color) are injected ONLY for `run`;
# other subcommands (eval, validate, test, …) get exactly what horde passed
# in ORC_EXTRA_ARGS — horde owns their full argv. For `run`, the construction
# below is byte-identical to the historical one.
ORC_SUBCMD="${ORC_SUBCMD:-run}"
if [ "$ORC_SUBCMD" = "run" ]; then
    ORC_ARGS="--auto --no-color"
    if [ -n "${WORKFLOW:-}" ]; then
        ORC_CMD="orc run -w $WORKFLOW $TICKET $ORC_ARGS ${ORC_EXTRA_ARGS:-}"
    else
        ORC_CMD="orc run $TICKET $ORC_ARGS ${ORC_EXTRA_ARGS:-}"
    fi
else
    ORC_CMD="orc $ORC_SUBCMD ${ORC_EXTRA_ARGS:-}"
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
    # orc's output must reach BOTH the container log (CloudWatch, for `horde
    # logs`) AND a local file (so we can parse a rate-limit reset time after
    # it exits). Stream it through a FIFO: orc writes to the FIFO; a `tee`
    # drains the FIFO to real stdout + the log file. This avoids the
    # `... > >(tee log)` process-substitution form, which keeps the stdout
    # pipe open and deadlocks the script's final exit on Fargate (container
    # never stops). tee exits at FIFO EOF; we wait on it so no orc output is
    # lost before exit.
    ORC_LOG=/tmp/orc-output.log
    ORC_FIFO=$(mktemp -u)
    mkfifo "$ORC_FIFO"
    tee "$ORC_LOG" < "$ORC_FIFO" &
    TEE_PID=$!
    # `eval "exec $ORC_CMD"` makes the backgrounded subshell BECOME orc, so
    # ORC_PID is orc's real PID and a forwarded SIGTERM reaches orc directly.
    # (A plain `eval "$ORC_CMD" &` leaves $! pointing at the eval wrapper, so
    # the signal never reaches orc and its interrupt-save never runs.)
    { eval "exec $ORC_CMD"; } > "$ORC_FIFO" 2>&1 &
    ORC_PID=$!
    # On SIGTERM (ECS StopTask, incl. a spot interruption) forward it to orc
    # AND re-wait so orc finishes saving its interrupted session before the
    # upload/snapshot block below runs. Without the second wait, the initial
    # `wait` returns the moment the signal arrives (exit 143) and we would
    # race orc's save — uploading a half-written session/snapshot. The ECS
    # container stopTimeout (120s) gives orc room to finish before SIGKILL.
    #
    # Capture orc's TRUE exit code, not the interrupted-wait's 143: when a
    # signal interrupts the foreground `wait`, $? is 143, but the trap's
    # re-wait sees orc's real exit (e.g. 5 on signal). Record which path ran
    # via TERMED and grab each wait's $? immediately. Getting this wrong maps
    # a spot interruption to `failed` instead of `killed`/recoverable on ECS.
    TERMED=0
    ORC_RC=0
    term_handler() { TERMED=1; kill -TERM "$ORC_PID" 2>/dev/null; wait "$ORC_PID"; ORC_RC=$?; }
    trap term_handler TERM INT
    wait "$ORC_PID"
    BARE_RC=$?
    trap - TERM INT
    if [ "$TERMED" = 1 ]; then EXIT_CODE=$ORC_RC; else EXIT_CODE=$BARE_RC; fi
    wait "$TEE_PID" 2>/dev/null  # let tee drain orc's output to stdout+log
    rm -f "$ORC_FIFO"

    # Snapshot the whole working tree to S3 so a future `horde retry` resumes
    # exactly where the run left off (#32). This captures committed AND
    # uncommitted changes plus .git — the ECS analog of the Docker provider's
    # persistent on-host workspace. Uses the task role's S3 access (no git
    # push / repo write needed). `--delete` keeps the mirror exact so files
    # removed during the run don't linger on resume. Non-fatal.
    if [ -d /workspace ]; then
        aws s3 sync /workspace/ "s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/workspace/" --delete \
            || echo "WARNING: workspace upload failed (continuing)" >&2
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
