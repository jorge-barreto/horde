package docs

var topics = []Topic{
	{
		Name:    "quickstart",
		Title:   "Quick Start",
		Summary: "Setup through first launch",
		Content: topicQuickstart,
	},
	{
		Name:    "config",
		Title:   "Project Configuration",
		Summary: ".horde/config.yaml schema and options",
		Content: topicConfig,
	},
	{
		Name:    "worker-image",
		Title:   "Worker Image System",
		Summary: "Base and project image layers, auto-build, rebuild triggers",
		Content: topicWorkerImage,
	},
	{
		Name:    "providers",
		Title:   "Providers",
		Summary: "Docker (local) and AWS ECS (Fargate), including recoverable runs",
		Content: topicProviders,
	},
	{
		Name:    "retry",
		Title:   "Retrying and Inspecting Runs",
		Summary: "Retry recoverable runs (Docker in place, ECS from S3), shell access, cleanup",
		Content: topicRetry,
	},
	{
		Name:    "hydrate",
		Title:   "Hydrating Run Results",
		Summary: "Copy run artifacts locally for orc improve / orc doctor",
		Content: topicHydrate,
	},
	{
		Name:    "json",
		Title:   "Machine-Readable Output (--json)",
		Summary: "JSON output, launch status enum, exit codes for scripting",
		Content: topicJSON,
	},
	{
		Name:    "install",
		Title:   "Installing and Updating horde",
		Summary: "Install via script or Homebrew; self-update with 'horde update'",
		Content: topicInstall,
	},
	{
		Name:    "env",
		Title:   "Environment Setup",
		Summary: "Required secrets, .env file, token permissions",
		Content: topicEnv,
	},
	{
		Name:    "bootstrap",
		Title:   "AWS Bootstrap",
		Summary: "Provision AWS infrastructure via CloudFormation (bootstrap init/deploy/destroy)",
		Content: topicBootstrap,
	},
	{
		Name:    "ecs-integration",
		Title:   "ECS Integration Tests",
		Summary: "How to run the end-to-end ECS test suite against a real AWS account",
		Content: topicECSIntegration,
	},
	{
		Name:    "cdk",
		Title:   "CDK Construct (@horde.io/cdk)",
		Summary: "Provision AWS infrastructure from your existing CDK app",
		Content: topicCDK,
	},
	{
		Name:    "labels",
		Title:   "Run Labels and List Filtering",
		Summary: "Tag runs with --label and filter/aggregate horde list",
		Content: topicLabels,
	},
}

const topicQuickstart = `Quick Start
===========

1. Install horde:

    make install

2. Create a .env file in your project root:

    cp .env.example .env

   Fill in the two required values:

    CLAUDE_CODE_OAUTH_TOKEN=<token from 'claude setup-token'>
    GIT_TOKEN=<GitHub fine-grained personal access token>

   See 'horde docs env' for details on token setup.

3. Launch a run:

    horde launch --workflow implement-ticket PROJ-123

   --workflow is required and selects an orc workflow from .orc/workflows/.

   On the first launch, horde builds the worker Docker image automatically.
   This takes a few minutes. Subsequent launches reuse the cached image
   unless the Dockerfile changes.

   The command prints a run ID to stdout (e.g. j87pi2i5tzqd).

4. Monitor the run:

    horde logs j87pi2i5tzqd --follow    # stream logs in real time
    horde status j87pi2i5tzqd            # check current state

5. When the run completes:

    horde results j87pi2i5tzqd           # view results summary

   Results include status, total cost, duration, and a per-phase breakdown.

6. List all runs for the current repo:

    horde list            # active runs (pending/running)
    horde list --all      # include terminal runs (success/failed/killed/timed_out/rate_limited)

   Tag runs at launch and filter the list by those tags (and by status,
   workflow, ticket, or time):

    horde launch --workflow implement-ticket PROJ-123 --label epic=KS-100
    horde list --label epic=KS-100 --all
    horde list --status running --workflow qa-pr --since 24h

   See 'horde docs labels' for the full filtering and cohort-cost story.

Other useful commands:

    horde launch ... --env KEY=VALUE  # set a per-launch env var (repeatable;
                                      # see 'horde docs config')
    horde kill <run-id>      # stop a running run
    horde retry <run-id>     # restart — orc picks up where it left off
    horde retry <run-id> -- --resume  # pass extra flags through to orc
    horde shell <run-id>     # interactive shell into the container
    horde clean [run-id]     # remove stopped containers
    horde docs <topic>       # read detailed documentation

Need a database, headless browser, or mock service alongside the worker? If you
deploy via the @horde.io/cdk construct you can attach sidecar containers to the
worker task (reachable on localhost) with the 'sidecars' prop — see
'horde docs cdk'.
`

const topicConfig = `Project Configuration
=====================

horde reads optional project-level settings from .horde/config.yaml in
your project root. If the file is missing, horde uses empty defaults.

Schema
------

    mounts:
      - <host-path>:<container-path>

    secrets:
      <CONTAINER_ENV_VAR>:
        env: <HOST_ENV_VAR>            # docker provider source
        aws-secret: <SECRETS_MANAGER_NAME>  # ECS provider source

Fields
------

mounts (list of strings, optional):

    Volume mounts passed to the Docker container. Each entry uses
    Docker's host:container format.

    The host side can be relative to the project root or absolute.
    Relative paths are resolved against the project directory. Entries
    where the host path does not exist are silently skipped.

    Examples:

        mounts:
          - .beads:/workspace/.beads
          - /etc/ssl/certs:/etc/ssl/certs
          - data:/workspace/data

    The container's project root is /workspace — this is where horde
    clones the repo inside the container.

secrets (map, optional):

    Caller-declared secrets injected into the worker container as env
    vars. The map key is the env-var name as seen inside the container.
    Each entry can declare multiple per-provider sources; the active
    provider picks the matching one.

    Two source kinds are supported in v0.2:

        env: <NAME>          The host env-var name to read from .env on
                             the docker provider.
        aws-secret: <NAME>   The AWS Secrets Manager secret name (no
                             ARN) for the ECS provider. Caller is
                             responsible for creating the secret before
                             running 'horde bootstrap deploy'.

    Two canonical secrets — CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN —
    are auto-seeded with their default sources, so you do not need to
    declare them unless overriding. Re-declaring a canonical replaces
    the default source map entirely (no field-level merge).

    Validation: at 'horde launch' time, every declared secret must
    have a source matching the active provider, and (for docker) the
    referenced .env keys must exist. horde refuses to launch otherwise
    with a single error message naming every missing secret/key.

    Docker provider note: horde injects all keys from .env via
    'docker run --env-file', so the host-side name is also visible
    inside the container. A remap (host MY_REVIEW_TOKEN -> container
    REVIEW_GIT_TOKEN) makes the container env include both names with
    the same value. The contract is that the spec-declared container
    name is set; horde does not isolate host-side names from the
    container.

    ECS note: extra aws-secret entries are baked into the task
    definition by 'horde bootstrap init' / 'horde bootstrap deploy';
    redeploy the stack after editing this list. Fargate caps secret
    references per task definition at roughly 16 — practical projects
    will not hit this, but mention it for transparency.

    Example:

        secrets:
          REVIEW_GIT_TOKEN:
            env: REVIEW_GIT_TOKEN
            aws-secret: horde/review-git-token
          STRIPE_API_KEY:
            env: STRIPE_API_KEY
            aws-secret: prepdesk/stripe-api-key

Per-launch env vars (--env)
---------------------------

    Everything above is project-level: every run for a repo gets the same
    set. For values that vary per launch — a feature flag, an experiment
    variant, an orchestrator's run ID — pass --env KEY=VALUE on the launch.
    The flag is repeatable:

        horde launch PROJ-1 --workflow build \
          --env PROMPT_VARIANT=v3 --env DEBUG=1

    Keys must be valid env-var names ([A-Za-z_][A-Za-z0-9_]*). VALUE may be
    empty (--env FLAG=) and may itself contain '='. On a duplicate key the
    last --env wins. The horde-managed control vars (REPO_URL, TICKET,
    BRANCH, WORKFLOW, RUN_ID, ARTIFACTS_BUCKET, ORC_EXTRA_ARGS) are reserved
    and rejected.

    Override of project secrets:

        docker — a per-launch --env value overrides a project secret (or
                 any .env value) of the same key. Use it to point one run at
                 a staging key without editing .env.

        ECS    — a declared secret of the same name takes precedence: the
                 secret lives on the task definition and AWS wins over the
                 RunTask environment override. horde injects the --env value
                 anyway and prints a warning that it will not take effect.
                 Overriding a non-secret key, and setting brand-new keys,
                 work the same on both providers.

    --env applies to 'horde launch' only. Per-launch values are not stored
    on the run record, so 'horde retry' does not carry them forward — pass
    --env again on a fresh launch if a resumed run needs them.

File Location
-------------

    <project-root>/.horde/config.yaml

The .horde/ directory should be gitignored (except config.yaml itself).
A typical .gitignore entry:

    .horde/*
    !.horde/config.yaml
`

const topicWorkerImage = `Worker Image System
===================

horde uses a two-layer Docker image system:

    horde-worker-base:latest    Built from embedded files (docker/)
    horde-worker:latest         Built from worker/Dockerfile, or tagged from base

Base Image
----------

The base image is built from files embedded in the horde binary itself
(the docker/ directory at build time). It includes:

    - debian:bookworm-slim
    - git, jq, bash, curl, unzip, gpg
    - GitHub CLI (gh)
    - AWS CLI v2
    - orc (installed via go install)
    - bd (installed via go install)
    - Claude CLI
    - entrypoint.sh and git-askpass.sh

These files are synced to ~/.horde/workerfiles/ at launch time. The sync
is content-aware — files whose content hasn't changed keep their original
modification times, so the staleness check only triggers on real changes.

Project Image
-------------

If your project has a worker/Dockerfile, horde builds horde-worker:latest
from it. This Dockerfile typically extends the base:

    FROM horde-worker-base:latest
    COPY --from=golang:1.25-bookworm /usr/local/go /usr/local/go
    ENV PATH="/usr/local/go/bin:${PATH}"
    RUN apt-get update && apt-get install -y make gcc libc6-dev \
        && rm -rf /var/lib/apt/lists/*

If no worker/Dockerfile exists, horde tags the base image directly as
horde-worker:latest.

Auto-Build and Rebuild Triggers
-------------------------------

horde builds images automatically on launch. No manual build step needed.

Every build stamps the resulting image with a horde.built_at label
(RFC3339Nano timestamp). The staleness check reads this label rather
than the image's Created time — cache-hit builds still refresh the label,
so the check is cache-safe.

Base image rebuilds when:
    - The image does not exist
    - The image has no horde.built_at label (pre-label horde build)
    - Any file in ~/.horde/workerfiles/ is newer than the label

Project image rebuilds when:
    - The image does not exist or has no horde.built_at label
    - The base image's label is newer than the project image's label
    - Any file in worker/ is newer than the project image's label

After 'make install', the embedded files get a fresh mtime, so the next
launch detects them as newer and rebuilds. This means upgrading horde
automatically picks up Dockerfile changes.
`

const topicProviders = `Providers
=========

horde uses a provider interface to abstract container/task lifecycle.
The provider handles launching, monitoring, log streaming, killing, and
reading files from worker instances.

Docker Provider (v0.1)
----------------------

The Docker provider runs horde-worker:latest locally via 'docker run'.

    horde launch --workflow implement-ticket PROJ-123                    # uses docker by default
    horde launch --workflow implement-ticket PROJ-123 --provider docker  # explicit

How it works:

    1. Creates a persistent workspace at ~/.horde/workspaces/<run-id>/
       and mounts it into the container at /workspace.
    2. Creates a persistent sessions dir at
       ~/.horde/workspaces/<run-id>-sessions/ and mounts it into the
       container at /home/horde/.claude so agent session history survives
       across retries (orc --resume needs these files).
    3. Runs the container in detached mode with environment variables
       for repo URL, ticket, branch, workflow, and run ID.
    4. Secrets from .env are passed via --env-file.
    5. Volume mounts from .horde/config.yaml are applied with -v flags.
    6. The container's entrypoint clones the repo, runs orc, and exits.

Run data is stored locally:

    ~/.horde/horde.db                        SQLite run history
    ~/.horde/workspaces/<run-id>/            Persistent workspace
    ~/.horde/workspaces/<run-id>-sessions/   Persistent agent session state
    ~/.horde/results/<run-id>/               Artifacts, audit logs, saved logs

Completion is detected lazily — the next 'horde status', 'horde results',
or 'horde list' call checks the container state. On detecting completion,
horde copies artifacts from the container and updates the database. The
container is preserved for 'horde retry' and 'horde shell'.

Timeout enforcement is also lazy: each status/results/list call checks
whether the run has exceeded its timeout_at time. If so, horde stops
the container (preserving it for retry).

Logs are available via 'docker logs' while the container exists. Saved
container logs are also available in the results directory.

AWS ECS Provider (v0.2)
-----------------------

The ECS provider runs horde-worker as an ECS Fargate task. It is
selected automatically when an AWS stack is deployed (horde discovers it
via SSM), or explicitly with --provider aws-ecs.

    horde launch --workflow implement-ticket PROJ-123                  # auto-detected via SSM
    horde launch --workflow implement-ticket PROJ-123 --provider aws-ecs

What it uses:
    - ECS RunTask for launching (no Lambda indirection)
    - DynamoDB for shared team run history
    - SSM Parameter Store for infrastructure config discovery
    - CloudWatch for log streaming
    - S3 for artifact storage and recoverable-run persistence
    - EventBridge + status Lambda for status sync (accurate even if the
      CLI disconnects); it also records the ECS stop_code / stop_reason
      into the run's metadata
    - Secrets Manager for token injection

Stand up the stack with 'horde bootstrap' (CloudFormation) or the
@horde.io/cdk construct — see 'horde docs bootstrap' and 'horde docs cdk'.

Recoverable runs
----------------

Both providers support 'horde retry' on runs that ended in a recoverable
state (failed, killed, timed_out, rate_limited): orc resumes from where
it left off. Docker reuses the on-host workspace in place; ECS restores
the agent session and working tree from S3, since Fargate has no
persistent filesystem. See 'horde docs retry' for the full lifecycle.
`

const topicRetry = `Retrying and Inspecting Runs
=============================

A run that ends in a recoverable state can be relaunched with the same
run ID, and orc picks up where it left off. On Docker the preserved
on-host workspace is reused in place; on ECS the worker restores the
agent session and working tree it synced to S3 before the task was
reaped (see "Recoverable Runs", below).

Recoverable statuses
--------------------

orc's exit code maps to a terminal status:

    exit 0    success         done; not retryable
    exit 2    timed_out       hit an orc phase timeout
    exit 4    rate_limited    hit the Anthropic cost/rate limit
    other     failed          any other non-zero exit

Two more terminal statuses come from horde itself rather than orc:

    killed                    stopped via 'horde kill' or an ECS StopTask
                              / spot interruption

'horde retry' accepts failed, killed, timed_out, and rate_limited.
Successful runs cannot be retried. timed_out and rate_limited are
distinct from failed precisely so a caller (or a future auto-resume
loop) can tell a recoverable interruption apart from a genuine failure;
all four are treated identically by retry, list filters, and
IsTerminal().

Retry
-----

    horde retry <run-id> [-- <orc-args>...]

Relaunches against the same run ID. If the old worker is still alive, it
is stopped first. orc sees its audit state and picks up from the
interrupted phase automatically. The same run ID is reused and the
timeout is reset.

By default --resume is passed to orc so it reattaches to the in-flight
agent session. Pass explicit orc args after -- to override:

    horde retry abc123                  # implicit --resume
    horde retry abc123 -- --resume      # explicit, same effect
    horde retry abc123 -- --retry implement

Extra orc flags after -- are passed through to orc unchanged. horde does
not validate them.

How the session and working tree survive depends on the provider:

  - Docker: the workspace and sessions dir live on the host
    (~/.horde/workspaces/<run-id>/ and -sessions/) and are remounted
    into the fresh container in place.
  - ECS: Fargate has no persistent filesystem, so the worker syncs both
    ~/.claude (the agent session) and the full /workspace (committed +
    uncommitted changes + .git) to S3 when the task terminates, and
    restores both on the next launch with the same run ID — using the
    task role's existing S3 access, no git push or repo write. See
    "Recoverable Runs (ECS)" below.

Shell
-----

    horde shell <run-id>

Opens an interactive bash shell. If the container is alive, exec's into
it. If the container is gone but the workspace exists, launches an
ephemeral container with the workspace mounted:

    horde shell abc123
    # Inside the container:
    cd /workspace
    orc run horde-k43 --resume          # resume interrupted agent session
    orc run horde-k43 --retry implement # retry from a specific phase
    git push origin HEAD:refs/heads/horde/horde-k43  # push work manually

Changes made in the shell affect the workspace directly.

Clean
-----

    horde clean              # remove all terminal containers
    horde clean <run-id>     # remove a specific container
    horde clean --purge      # also remove workspace directories

Containers are removed but workspaces are preserved by default for
retry and shell access. Use --purge to free disk space when you no
longer need the workspace. Running and pending runs cannot be cleaned.
--purge also removes the matching sessions dir.

Workspace Persistence (Docker)
------------------------------

Each run's workspace lives at ~/.horde/workspaces/<run-id>/ on the host,
mounted into the container at /workspace. Agent session state lives
alongside it at ~/.horde/workspaces/<run-id>-sessions/, mounted at
/home/horde/.claude. This means:

    - Container crashes: workspace survives, retry launches fresh compute
    - Docker restarts: same — workspace is on the host filesystem
    - horde kill: container stopped, workspace preserved
    - horde clean: container removed, workspace preserved (use --purge)

You can also access the workspace directly from the host:

    ls ~/.horde/workspaces/<run-id>/
    cd ~/.horde/workspaces/<run-id>/ && git log

Recoverable Runs (ECS)
----------------------

A Fargate task has no host filesystem to mount, so the ECS worker
reproduces the Docker persistence guarantee through S3. It is the analog
of the Docker provider's on-host workspace, not a separate feature.

On terminate (orc exit, 'horde kill', ECS StopTask, or a spot
interruption), the worker:

    1. Catches SIGTERM. orc runs backgrounded so the signal reaches it;
       orc saves its interrupted agent session before anything uploads.
       orc's true exit code is preserved across the signal.
    2. Syncs ~/.claude (the agent session) and the full /workspace
       (committed + uncommitted changes + .git) to S3 under the run's
       prefix. Uses the task role's S3 access — no git push, no repo
       write perms.

On the next 'horde retry' for that run ID, a fresh task restores both
before re-entering orc, so the agent resumes the same conversation
against the same working tree. This replaced an earlier git-ref snapshot
approach, which needed repo push perms and captured only committed work.

The status Lambda also records the ECS stop_code / stop_reason into the
run's metadata map. A spot interruption surfaces as
stop_code == "TerminationNotice" — the signal a future auto-resume
follow-up keys off.

Container Lifecycle
-------------------

    horde launch   →  workspace created on host, container mounts it
    orc finishes   →  container stays alive (sleep infinity)
    horde status   →  detects completion via marker file, copies artifacts
    horde retry    →  stop old container (if any), launch new against workspace
    horde shell    →  exec in live container, or ephemeral container
    horde kill     →  container stopped, workspace preserved
    horde clean    →  container removed, workspace preserved
`

const topicEnv = `Environment Setup
=================

horde requires a .env file in your project root with credentials for
the worker container.

Required Keys
-------------

CLAUDE_CODE_OAUTH_TOKEN

    A long-lived OAuth token for the Claude CLI. Generate it with:

        claude setup-token

    This is how orc (running inside the container) authenticates with
    Claude. The token is passed to the container via --env-file.

GIT_TOKEN

    A GitHub fine-grained personal access token. This is used for two
    purposes:

    1. Git authentication: The container uses GIT_ASKPASS with a
       credential helper (git-askpass.sh) that returns the token.
       This avoids putting credentials in process arguments or
       .git/config.

    2. GitHub CLI: The entrypoint sets GH_TOKEN to the GIT_TOKEN
       value, so 'gh' commands (like 'gh pr create') authenticate
       with the same token.

    Recommended GitHub PAT permissions:

        Contents         Read/Write    Clone repos, push to branches
        Metadata         Read          Required for all fine-grained PATs
        Pull requests    Read/Write    Open PRs via 'gh pr create'
        Issues           Read          Read ticket/issue context
        Workflows        Read/Write    Push changes to .github/workflows/

.env File Format
----------------

    # Comments start with #
    CLAUDE_CODE_OAUTH_TOKEN=your-token-here
    GIT_TOKEN=github_pat_xxxxx

    Blank lines and comments are ignored. Each key=value pair is on
    its own line. No quotes needed around values.

Security
--------

    - The .env file must be gitignored. Never commit tokens to git.
    - Tokens reach the container via 'docker run --env-file', not as
      command-line arguments (which would be visible in process lists).
    - GIT_ASKPASS is set as a Dockerfile ENV directive, making it
      available to all processes in the container (not just the
      entrypoint shell).

Validation
----------

horde validates the .env file before every launch. The required keys
are driven by the merged secret spec (see 'horde docs config'):

  - The two canonical secrets, CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN,
    are always required unless overridden in .horde/config.yaml's
    secrets: block.
  - Any additional secret declared with an env: source contributes its
    referenced host env-var name to the required set.

If a key is missing, the launch fails with a single error message
naming every missing key. Values are not verified — invalid tokens
surface later at clone time or during orc execution.
`

const topicHydrate = `Hydrating Run Results
=====================

` + "`horde hydrate`" + ` copies the .orc/audit/ and .orc/artifacts/ trees from
one or more completed runs into a local directory so you can run
` + "`orc improve`" + `, ` + "`orc doctor`" + `, or any other orc tool that operates on
a local .orc/ folder.

Synopsis

    horde hydrate <run-id> [<run-id>...] --into <dir>

Layout

Hydrated data is placed under a per-run leaf directory so multiple runs
never collide:

    <dir>/.orc/audit/<ticket>-<run-id>/...
    <dir>/.orc/artifacts/<ticket>-<run-id>/...

For runs that used a named workflow, the workflow name is inserted before
the leaf:

    <dir>/.orc/audit/<workflow>/<ticket>-<run-id>/...

Examples

Single run:

    horde hydrate abc123def456 --into /tmp/inspect
    cd /tmp/inspect
    orc improve

Weekly batch (e.g. a cron job):

    horde list --all --json \\
      | jq -r '.runs[].id' \\
      | xargs horde hydrate --into /tmp/weekly

Semantics

- Each run-id is processed independently. A failure on one does not abort
  the others.
- Runs whose destination subdirectory already exists are skipped. To
  re-hydrate, delete the subdirectory.
- Runs that are not in a terminal state (pending/running) are reported as
  failures and skipped.
- Exit 0 if all run-ids were hydrated or skipped. Exit non-zero if any
  run-id failed.

Providers

- Docker provider: copies from ~/.horde/results/<run-id>/.
- ECS provider: downloads from s3://<artifacts-bucket>/horde-runs/<run-id>/.
`

const topicJSON = `Machine-Readable Output (--json)
================================

The global --json flag switches a command's stdout to a single
machine-readable JSON object. It applies to:

    launch  retry  status  results  list  kill  clean  hydrate  push

Under --json, stdout carries exactly one JSON object and nothing else.
Human-oriented chatter (image-build progress, warnings, the "Retrying..."
line) always goes to stderr, so a consumer parsing stdout is never
corrupted.

Errors
------

On a real failure, any command emits an error envelope to stdout and
exits non-zero, while the human "error: <message>" line still goes to
stderr:

    {
      "status": "error",
      "reason": "validating .env file: missing required key(s): GIT_TOKEN"
    }

Launch status enum
------------------

'horde launch --json' emits a stable status so programmatic callers never
have to grep stderr wording:

    {
      "status": "launched",
      "run_id": "a1b2c3d4e5f6",
      "ticket": "PROJ-123",
      "workflow": "implement-ticket",
      "branch": "develop",
      "existing_run_id": null
    }

    status            meaning                                        exit
    --------------    -------------------------------------------    ----
    launched          run started; run_id set                       0
    duplicate         an active run already exists for the ticket;   0
                      existing_run_id set
    capped            at the concurrency limit; retry later;         0
                      reason set
    error             a real failure; reason set                     1

run_id is null unless status is "launched"; existing_run_id is null
unless status is "duplicate".

Exit codes
----------

Under --json, exit codes are binary: 0 for any protocol-level outcome
(launched / duplicate / capped) and 1 only for true errors. Branch on the
"status" field to tell them apart — a capped launch is a signal to retry,
not a failure.

WITHOUT --json, 'launch' keeps its human behavior: a duplicate or capped
launch prints a message to stderr and exits 1.

Scripting examples
------------------

Launch and react to the outcome:

    out=$(horde launch --provider docker --workflow implement-ticket PROJ-123 --json)
    case "$(echo "$out" | jq -r .status)" in
      launched)  echo "started $(echo "$out" | jq -r .run_id)" ;;
      duplicate) echo "already running: $(echo "$out" | jq -r .existing_run_id)" ;;
      capped)    echo "at capacity, requeue later" ;;          # retry the message
      error)     echo "failed: $(echo "$out" | jq -r .reason)" >&2; exit 1 ;;
    esac

List every run's workflow without N+1 status calls (issue #25):

    horde list --all --json | jq -r '.runs[] | "\(.id) \(.workflow) \(.status)"'

Other commands report a status object too: retry -> "retrying",
kill -> "killed", clean -> "cleaned" (with removed_run_ids), push ->
"pushed" (with image + digest), hydrate -> aggregate counts plus a
per-run "runs" array.
`

const topicInstall = `# Installing and Updating horde

## Install (no Go toolchain required)

Download the latest release binary:

    curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh | sh

Pin a version or change the install directory:

    HORDE_VERSION=v0.1.0 HORDE_BINDIR="$HOME/.local/bin" \
      curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh | sh

The script detects your OS/arch, downloads the matching release archive,
verifies its SHA-256 checksum, and installs the binary.

## Install via Homebrew

    brew tap jorge-barreto/tap
    brew install horde

## Install from source (requires Go 1.24+)

    make install

## Updating

If you installed via the script or a downloaded binary:

    horde update          # install the latest release if newer
    horde update --check  # only report whether an update is available

If you installed via Homebrew:

    brew upgrade horde

'horde version' prints the running version and notes when a newer release is
available. Set HORDE_NO_UPDATE_CHECK=1 to disable that check.
`

const topicBootstrap = `AWS Bootstrap
=============

For projects that don't already have CDK or other IaC, horde ships a
self-contained CloudFormation path that provisions every AWS resource
needed to run workflows on ECS Fargate: VPC, ECS cluster, DynamoDB table,
S3 artifacts bucket, ECR repository, Secrets Manager secrets, IAM roles,
a CloudWatch log group, an SSM config parameter, and an EventBridge rule
plus inline Lambda that keeps run status in sync.

Workflow

  horde bootstrap init       # generates .horde/cloudformation.yaml
  horde bootstrap deploy     # creates or updates the CloudFormation stack
  horde push                 # tags and pushes horde-worker:latest to ECR
  horde launch --provider aws-ecs --workflow implement-ticket TICKET-123
  horde bootstrap destroy    # tears everything down

Step 1 — horde bootstrap init

Derives a project slug from the current git remote (e.g.
github.com/jorge-barreto/horde → jorge-barreto-horde), renders the
embedded CloudFormation template with that slug, and writes it to
.horde/cloudformation.yaml. Prints a summary of resources. Refuses to
overwrite an existing file unless --regenerate is passed.

The generated template is inspectable and hand-editable. Commit it to
version control — secrets are passed as NoEcho CloudFormation parameters
at deploy time, never baked into the file on disk.

Step 2 — horde bootstrap deploy

Applies .horde/cloudformation.yaml to AWS under the stack name
horde-<slug>. If the stack does not exist, it is created; otherwise it
is updated in place. horde polls CloudFormation every 5 seconds and
streams each new stack event as
  <timestamp> <LogicalResourceId> <ResourceStatus> <ResourceStatusReason>
until the stack reaches CREATE_COMPLETE or UPDATE_COMPLETE. Rollback
and *_FAILED terminal statuses produce an error. If UpdateStack reports
"No updates are to be performed" (template + parameters match the live
stack), horde treats it as success.

Deploy needs two secrets, passed as NoEcho CloudFormation parameters
(ClaudeCodeOauthToken, GitToken). It never logs or echoes them:
  - Interactive: when stdin is a TTY, horde prompts with hidden input.
  - Headless / CI: when stdin is not a TTY, horde reads CLAUDE_CODE_OAUTH_TOKEN
    and GIT_TOKEN from the environment (or from .env in the project root).
    Missing either is a hard error.

The stack creates IAM roles with fixed names, so deploy passes
CAPABILITY_NAMED_IAM automatically and tags the stack with horde-slug.

First-time deploys take roughly 15 minutes (the NAT gateway and ECS
cluster dominate); subsequent updates that only touch in-place resources
typically finish in 1–3 minutes.

When deploy completes it prints the SSM config parameter path,
/horde/<slug>/config, which holds the stack's runtime outputs consumed
by the ECS provider.

Step 3 — horde push

Tags the local horde-worker:latest image with the ECR repository URI
discovered from the SSM config parameter and pushes it to ECR. horde
push calls ecr:GetAuthorizationToken via the AWS SDK and pipes the
decoded password into 'docker login --password-stdin' — there is no
dependency on the AWS CLI. Requires that horde-worker:latest is built
locally first (a 'horde launch' under the docker provider, or 'make
docker-build', produces it); push errors out with guidance if it is
missing. The image is pushed as <ecr-repo-uri>:latest, and the sha256
digest parsed from the push output is echoed back for verification.

Step 4 — horde bootstrap destroy

Deletes the horde-<slug> CloudFormation stack and waits for deletion to
complete. Refuses if pending or running runs exist in DynamoDB (horde
checks the store first; kill the active runs before destroying). Prompts
for confirmation — the user must type the full stack name. Pass --force
to skip the confirmation in scripts.

If the DynamoDB store is unreachable (credentials stale, table deleted
from a prior partial destroy), destroy warns and proceeds rather than
blocking on an unreachable dependency.

Naming and cost notes

- All resources are named horde-<slug>-* and will coexist with other
  CloudFormation stacks from different projects.
- The stack includes one NAT gateway (~$32/month standalone, plus data
  transfer). The rest is pay-per-request or per-invocation — near-zero
  when idle.
- Private-subnet topology is the default (assign_public_ip=DISABLED).
  Tasks reach GitHub, Anthropic, and other public APIs through the NAT.
`

const topicECSIntegration = `ECS Integration Tests
=====================

The ECS integration suite (test/integration/ecs_*_test.go) runs every
horde feature end-to-end against a real CloudFormation stack: launch,
status, logs, kill, list, hydrate, retry with session + workspace
restore from S3, ECS stop-reason capture, concurrent runs, and
timeout/finalize reconciliation. Each test drives a full Fargate task
lifecycle and verifies the status Lambda updates DynamoDB.

Prerequisites

1. AWS credentials available (SSO via 'aws sso login --profile <name>',
   or static credentials — profile of your choice).
2. A '.env' at the repo root containing at minimum:

       AWS_PROFILE=<your-profile>
       AWS_REGION=us-east-1
       HORDE_E2E_ECS=1
       HORDE_E2E_ECS_KEEP=1
       CLAUDE_CODE_OAUTH_TOKEN=<from 'claude setup-token'>
       GIT_TOKEN=<a GitHub PAT>

3. Docker running locally (needed so 'horde push' can tag and push the
   worker image to ECR — push shells out to 'docker push').

Gate flags

HORDE_E2E_ECS=1
    Required. Without it the ECS tests skip and the test suite runs
    only docker-backed integration tests.

HORDE_E2E_ECS_KEEP=1
    Keep the bootstrap stack alive after the test suite exits. Default
    behavior (unset) destroys the stack at TestMain exit; keeping it
    cuts the next run's turnaround from ~3 minutes to under 10 seconds.
    When you're actively iterating, set this.

Running the suite

Parallel (recommended; ~2 minutes wall-clock for 17 tests):
    go test -count=1 -timeout 30m -run 'TestECS' ./test/integration/ -v

Individual test:
    go test -count=1 -timeout 10m -run TestECSSmoke ./test/integration/ -v

Stack lifecycle

On each 'go test' invocation with HORDE_E2E_ECS=1 the suite's TestMain:
    1. Regenerates .horde/cloudformation.yaml via 'horde bootstrap init --regenerate'.
    2. Runs 'horde bootstrap deploy' (no-op when stack exists and is current).
    3. Runs 'horde push' (no-op when ECR digest matches local image).
    4. Runs the tests.
    5. If HORDE_E2E_ECS_KEEP is unset, runs 'horde bootstrap destroy --force'.

Manually teardown when you're done iterating:
    horde bootstrap destroy

Cost

The stack's idle cost is dominated by one NAT gateway (~$0.045/hour +
data-transfer-per-GB). Fargate tasks cost ~$0.05/hour for 1 vCPU / 4 GB
while running; each test's task runs for ~90 seconds. A typical full
run with the stack kept alive for an hour and 20 task launches totals
well under $0.10.

Destroying the stack between iterations avoids NAT idle cost at the
expense of a ~3-minute redeploy on the next run.

Concurrency

The stack's SSM config sets max_concurrent=20, allowing up to 20
Fargate tasks at once. All ECS tests call t.Parallel(), so the 17-test
suite finishes in ~2 minutes wall-clock. Lowering max_concurrent also
requires rebuilding the template and redeploying.

Recoverable-run coverage

'horde retry' on ECS and agent-session persistence across retries are
covered end-to-end (this is what #35 added):

- TestECSResumeRestoresSession launches a run, kills it mid-agent-phase,
  asserts the session (~/.claude) and full /workspace land in S3, runs
  'horde retry', and confirms orc reattaches the restored session and
  the run reaches success. The resume-marker.yaml workflow is
  self-verifying — success on resume is only reachable if the session
  survived the kill.
- TestECSStopReasonRecorded asserts the status Lambda records the ECS
  stop_code / stop_reason into the run's DynamoDB metadata.

What's NOT tested end-to-end

- 'horde logs --follow' in the integration suite. The CLI implementation
  exists and works manually; an automated streaming-mode test is future
  work.
`

const topicCDK = `CDK Construct (@horde.io/cdk)
==========================

Teams that already use AWS CDK can import the @horde.io/cdk npm package and
provision every AWS resource horde needs from inside their own CDK app.
This is the alternative to 'horde bootstrap' (which uses CloudFormation
directly).

Install
-------

    npm install @horde.io/cdk aws-cdk-lib constructs

Both aws-cdk-lib (^2) and constructs (^10) are peer dependencies. The
status-sync Lambda ships pre-bundled, so consumer synth does not require
Docker or a local esbuild install.

Usage
-----

    import { App, Stack } from "aws-cdk-lib";
    import * as ecr from "aws-cdk-lib/aws-ecr";
    import * as ecs from "aws-cdk-lib/aws-ecs";
    import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
    import { HordeWorker } from "@horde.io/cdk";

    const app = new App();
    const stack = new Stack(app, "HordeStack");

    const repo = new ecr.Repository(stack, "WorkerImage", {
      repositoryName: "horde-my-org-my-repo",
    });

    new HordeWorker(stack, "Horde", {
      projectSlug: "my-org-my-repo",
      workerImage: ecs.ContainerImage.fromEcrRepository(repo, "latest"),
      ecrRepository: repo,
      secrets: {
        CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(
          stack, "ClaudeToken", "horde/claude-code-oauth-token"),
        GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(
          stack, "GitToken", "horde/git-token"),
      },
    });

What it provisions
------------------

Same surface as 'horde bootstrap' (CloudFormation flavor):

  - VPC with public + private subnets and one NAT gateway (or BYO via
    the 'vpc' prop)
  - ECS Fargate cluster + Fargate task definition (1 vCPU / 4 GB by
    default; tunable via 'cpu' / 'memoryMiB' props)
  - DynamoDB horde-runs-<slug> table with 4 GSIs (by-repo, by-ticket,
    by-status, by-instance)
  - S3 artifacts bucket with public-access blocked, SSE-S3, and a
    deny-non-TLS bucket policy (or BYO via 'artifactsBucket' prop)
  - Egress-only worker security group (443 outbound)
  - SSM /horde/<slug>/config parameter consumed by the CLI
  - EventBridge rule + status-sync Lambda (Node 20) that updates run
    rows in DynamoDB on STOPPED ECS task events
  - Scoped IAM task role, execution role, status Lambda role
  - Managed policy for the horde CLI, exposed as CfnOutput
    CliUserManagedPolicyArn — attach to the IAM principals that
    will run the CLI

Sidecar containers
------------------

The 'sidecars' prop adds extra containers to the worker task definition —
a test database, a headless browser, a mock API. They share the task's
network namespace (reachable on localhost) and its lifecycle.

    sidecars: [
      {
        containerName: "postgres",
        image: ecs.ContainerImage.fromRegistry("postgres:16"),
        environment: { POSTGRES_PASSWORD: "dev" },
      },
    ]

Each entry is a CDK ContainerDefinitionOptions. The construct defaults
'essential' to false (a crashing sidecar won't stop the run; set true for a
hard dependency) and routes logging to the worker log group. Run status is
always the worker container's exit code, never a sidecar's. 'memoryMiB' is
the task ceiling shared across containers; set per-container 'memoryLimitMiB'
to cap a sidecar. CDK-only — 'horde bootstrap' and '--provider docker' do
not provision sidecars.

Config defaults
---------------

  cpu                       1024 (1 vCPU)
  memoryMiB                 4096 (4 GB)
  maxConcurrent             5
  defaultTimeoutMinutes     1440 (24 h)
  logRetentionDays          30
  ssmParameterPath          /horde/<projectSlug>/config

CDK vs. CloudFormation
----------------------

Use 'horde bootstrap' if you don't already use CDK and want a single
'horde bootstrap deploy' command. Use @horde.io/cdk if you have an existing
CDK app and want the construct in your own pipeline. Both produce the
same SSM JSON shape (internal/config/ssm.go::HordeConfig), so the CLI
can't tell them apart.

End-to-end verification
-----------------------

The construct ships with a real-AWS smoke test gated by HORDE_E2E_CDK=1.
Three independent test functions, run as separate 'go test -run'
invocations so the stack lifetime is decoupled from the test runs:

    HORDE_E2E_CDK=1 go test -v -timeout 20m -run TestECSCDK_Bringup ./test/integration/
    HORDE_E2E_CDK=1 go test -v -timeout 15m -run TestECSCDK_Smoke    ./test/integration/
    HORDE_E2E_CDK=1 go test -v -timeout 15m -run TestECSCDK_Teardown ./test/integration/

Bring-up runs 'cdk deploy', builds the worker image via 'make docker-build',
and pushes it to the freshly-created ECR repo. State is written to
/tmp/horde-cdk-e2e-state.json so subsequent test runs can find the stack.

Smoke launches one quick-success workflow against the deployed stack and
asserts the status-sync Lambda updates the DynamoDB row to status=success
within 8 minutes. Re-run as often as you like — each invocation costs a
few cents of Fargate time.

Teardown empties the ECR repo + S3 artifacts bucket, then 'cdk destroy'.
Idempotent: works even if the state file is missing.

Full-suite verification (recommended before shipping a release)
---------------------------------------------------------------

Smoke alone exercises one happy-path workflow. To run every ECS test
('TestECSLaunch*', 'TestECSStatus*', 'TestECSLogs*', 'TestECSKill*',
'TestECSList*', 'TestECSLifecycle*', 'TestECSHydrate*') against the CDK
stack, set HORDE_E2E_ECS_BACKEND=cdk in addition to HORDE_E2E_ECS=1:

    HORDE_E2E_CDK=1 go test -v -timeout 20m -run TestECSCDK_Bringup    ./test/integration/
    HORDE_E2E_ECS=1 HORDE_E2E_ECS_BACKEND=cdk go test -v -timeout 30m \
        -run TestECS -skip TestECSSmoke ./test/integration/
    HORDE_E2E_CDK=1 go test -v -timeout 15m -run TestECSCDK_Teardown   ./test/integration/

This runs every CLI surface (launch/status/logs/kill/list/hydrate/
lifecycle) through the CDK-deployed stack, giving symmetric coverage
with the CF bootstrap path. 'TestECSSmoke' is skipped because it
hardcodes the CF slug internally; all other TestECS_* tests honor the
backend switch.

Backend selection via HORDE_E2E_ECS_BACKEND:

    (unset) or cf    Default. CloudFormation bootstrap stack.
    cdk              CDK-deployed stack. Requires TestECSCDK_Bringup
                     to have populated /tmp/horde-cdk-e2e-state.json.

Cost: this stack runs its own NAT Gateway (~$32/mo idle) since it can't
share infrastructure with the bootstrap CF stack. Always tear down when
you're done.
`

const topicLabels = `Run Labels and List Filtering
=============================

Labels are key=value tags you attach to a run at launch. They describe the
*dispatch* — which epic a run belongs to, which prompt variant it used, who
fired it — things the run's work fields (ticket, workflow, branch) don't
capture. They are separate from horde's internal provider metadata, so your
keys never collide with reserved ones.

Setting labels
--------------

    horde launch --workflow implement-ticket PROJ-123 \
      --label epic=KS-100 --label variant=v3

  - --label is repeatable; pass it once per tag.
  - Split on the first '=', so values may contain '=' (e.g. note=a=b).
  - Keys: letters, digits, and . _ - only, up to 64 chars.
  - Values: any text, up to 256 chars.
  - Labels are set once at launch. 'horde retry' keeps the original run's
    labels (it reuses the same run record).

Filtering the list
------------------

'horde list' is scoped to the current repo and accepts these filters, all
AND-combined (a run must match every one you pass):

    --label key=value   only runs carrying this label (repeatable)
    --status <status>    pending | running | success | failed | killed |
                         timed_out | rate_limited (repeatable). Passing
                         --status also includes terminal runs, like --all.
    --workflow <name>    only runs of this workflow
    --ticket <id>        only runs for this ticket
    --since <when>       only runs started at/after <when>
    --until <when>       only runs started at/before <when>

<when> is an RFC3339 timestamp (2026-04-01 or 2026-04-01T12:00:00Z) or a
duration-ago: 30m, 1h, 7d.

Examples:

    horde list --label epic=KS-100 --all
    horde list --status running --status pending
    horde list --workflow qa-pr --since 24h
    horde list --label epic=KS-100 --label variant=v3 --all

Reading labels back
-------------------

'horde status <run-id>' shows a Labels: line when a run has labels.
'horde list --json' and 'horde status --json' include a "labels" object on
each run (omitted when empty).

Cohort cost
-----------

'horde list --json' includes a "summary" object aggregating the filtered
result set:

    {
      "runs": [ ... ],
      "summary": { "count": 12, "total_cost_usd": 4.82 }
    }

So a label cohort's spend is one command away — no client-side summing:

    horde list --all --label epic=KS-100 --json | jq .summary

The human table prints the same as a trailing "12 runs, $4.82 total" line.
`
