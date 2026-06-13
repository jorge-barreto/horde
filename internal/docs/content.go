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
	{
		Name:    "queue",
		Title:   "Server-Side Queue and Spend Cap",
		Summary: "Enqueue launches (--enqueue), priority, drain semantics, spend-rate cap (aws-ecs)",
		Content: topicQueue,
	},
	{
		Name:    "events",
		Title:   "Run-Lifecycle Events",
		Summary: "run.started / run.terminal / run.requeued / run.cost-threshold-exceeded on the EventBridge bus",
		Content: topicEvents,
	},
	{
		Name:    "spot",
		Title:   "Fargate Spot and Auto-Resume",
		Summary: "Spot-by-default, --capacity, and automatic resume on interruption",
		Content: topicSpot,
	},
}

const topicQuickstart = `Quick Start
===========

1. Install horde (no Go toolchain required):

    curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh | sh

   Or 'brew install jorge-barreto/tap/horde', or 'make install' from source.
   See 'horde docs install' for all methods and how to update.

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

   Results include status, total cost, duration, token usage, and a per-phase breakdown.

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
                             deploying the @horde.io/cdk stack.

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
    definition by the @horde.io/cdk stack; redeploy the stack after
    editing this list. Fargate caps secret references per task definition
    at roughly 16 — practical projects will not hit this, but mention it
    for transparency.

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

Discovery overrides (running without a checkout)
------------------------------------------------

    By default horde discovers identity and config from the working
    directory: the repo URL from 'git remote get-url origin', and config
    from .horde/config.yaml. Programmatic callers without a checkout
    (a Lambda, a CI runner outside the repo, a webhook handler) can
    override each step. Precedence is flag > env var > discovery > error:

        --repo / HORDE_REPO_URL        Canonical repo identifier. Skips git
                                       discovery entirely. Accepts a full URL
                                       (https://github.com/org/repo.git,
                                       git@github.com:org/repo) or a bare
                                       host/path (github.com/org/repo). It is
                                       canonicalized the same way as a git
                                       remote, so a trailing .git, the scheme,
                                       and case do not change the bucket key.

        --config / HORDE_CONFIG_PATH   Path to the project config — either a
                                       .horde/config.yaml file directly, or a
                                       directory containing .horde/config.yaml.
                                       The .env file (docker secret source) and
                                       relative mount host-paths resolve against
                                       this location's directory (the file's
                                       parent, or the directory itself).

        --ssm-path / HORDE_SSM_PATH    SSM parameter path for the aws-ecs
                                       config. Overrides the slug-derived
                                       default (/horde/<slug>/config).

    With --repo (or HORDE_REPO_URL) set, 'horde launch' never touches git or
    the filesystem for identity and will not error "not a git repository"
    from an empty directory. On aws-ecs the canonical repo still comes from
    the deployment's SSM config (the 'repo' field) regardless of --repo, so
    all run history for a deployment shares one bucket.

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

Stand up the stack with the @horde.io/cdk construct — see 'horde docs cdk'.

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
IsTerminal(). On Fargate Spot, an interrupted run is re-queued and
resumed automatically — see 'horde docs spot'.

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
    queued            parked in the server-side queue (--enqueue);   0
                      run_id + priority set (aws-ecs only)
    duplicate         an active run already exists for the ticket;   0
                      existing_run_id set
    capped            at the concurrency limit; retry later;         0
                      reason set
    error             a real failure; reason set                     1

run_id is set for "launched" and "queued"; existing_run_id is null unless
status is "duplicate". For "queued", a "priority" field carries the level
(lowest|low|med|high|highest). See 'horde docs queue'.

The 'horde queue' subcommands also emit JSON under --json: 'queue list' returns
{"status":"ok","queued":[...]} in drain order; 'queue prioritize' returns
{"status":"reprioritized",...}; 'queue cancel' returns {"status":"cancelled",...}.

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

Token telemetry
---------------

'horde status --json', 'horde list --json', and 'horde results --json'
include a "tokens" object on each run, reporting the per-run token totals
orc consumed:

    "tokens": {
      "input": 54791,
      "output": 87915,
      "cache_creation": 529692,
      "cache_read": 8934181,
      "turns": 12
    }

The object is omitted entirely when horde has no token data for the run.
Docker reports tokens live (read from the running container on each
status/list call); ECS reports them at finalize, after the run stops.

'horde list --json' also sums tokens across the filtered set into its
"summary" object (alongside total_cost_usd), so the orchestrator can derive
a burn rate — tokens over a time window — directly from one query, using the
per-run started_at/completed_at to bound the window:

    horde list --status running --json | jq '.summary.tokens'
`

const topicInstall = `Installing and Updating horde
=============================

Install (no Go toolchain required)
----------------------------------

Download the latest release binary:

    curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh | sh

Pin a version or change the install directory. Note the env vars go on the
'sh' side of the pipe, not before 'curl' — 'VAR=x curl ... | sh' would set the
var for curl, not the script:

    curl -fsSL https://raw.githubusercontent.com/jorge-barreto/horde/main/scripts/install.sh \
      | HORDE_VERSION=v0.4.0 HORDE_BINDIR="$HOME/.local/bin" sh

The script detects your OS/arch, downloads the matching release archive,
verifies its SHA-256 checksum, and installs the binary.

Install via Homebrew
--------------------

    brew tap jorge-barreto/tap
    brew install horde

Install from source (requires Go 1.24+)
---------------------------------------

    make install

Updating
--------

If you installed via the script or a downloaded binary:

    horde update          # install the latest release if newer
    horde update --check  # only report whether an update is available

If you installed via Homebrew:

    brew upgrade horde

'horde version' prints the running version and notes when a newer release is
available. Set HORDE_NO_UPDATE_CHECK=1 to disable that check.
`

const topicECSIntegration = `ECS Integration Tests
=====================

The ECS integration suite (test/integration/ecs_*_test.go) runs every
horde feature end-to-end against a real @horde.io/cdk stack: launch,
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
       HORDE_E2E_CDK=1
       CLAUDE_CODE_OAUTH_TOKEN=<from 'claude setup-token'>
       GIT_TOKEN=<a GitHub PAT>

3. Docker running locally (needed so 'horde push' can tag and push the
   worker image to ECR — push shells out to 'docker push').

Gate flags

HORDE_E2E_CDK=1
    Required to deploy and tear down the CDK stack (make e2e-up / e2e-down).
    Set in .env locally; never in CI.

HORDE_E2E_ECS=1
    Required to run the ECS tests themselves. Without it the ECS tests
    skip and the suite runs only docker-backed integration tests. 'make
    e2e-test' sets it (plus HORDE_E2E_ECS_BACKEND=cdk) for you.

Stack lifecycle

The CDK stack is managed explicitly via Make targets, not by TestMain.
Run them in order (each reads AWS creds + HORDE_E2E_CDK=1 from .env):

    make e2e-up      # deploy the @horde.io/cdk stack, populate Secrets
                     # Manager, push the worker image (~5 min cold, ~2 warm)
    make e2e-test    # run the full TestECS_* suite against the deployed stack
    make e2e-down    # destroy the stack — ALWAYS run this when done

The stack persists between e2e-test runs, so iterating is fast: deploy
once with e2e-up, run e2e-test as many times as you like, then e2e-down.
Under the hood e2e-up/down run TestECSCDK_Bringup / TestECSCDK_Teardown,
which write the stack's SSM path to a state file the ECS tests read.

Cost

The stack is pay-per-request / per-invocation with no NAT gateway
(public-subnet topology), so idle cost is near-zero. Fargate tasks cost
~$0.05/hour for 1 vCPU / 4 GB while running; each test's task runs for
~90 seconds. A typical full run with the stack kept alive for an hour
and 20 task launches totals well under $0.10. Always 'make e2e-down'
when finished so nothing lingers.

Concurrency

The stack's SSM config sets max_concurrent=20, allowing up to 20
Fargate tasks at once. All ECS tests call t.Parallel(), so the suite
finishes in a few minutes wall-clock. Lowering max_concurrent requires
redeploying the CDK stack.

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

The @horde.io/cdk npm package provisions every AWS resource horde needs
to run workflows on ECS Fargate. Import it into your own CDK app (or a
standalone one) — it is the supported way to stand up the horde stack.

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

  - VPC sized by 'networkMode' (default 'public'): public subnets and
    NO NAT gateway, with tasks on a public IP reaching the internet via
    the internet gateway (~$32/mo saved). 'networkMode: "private"' adds
    PRIVATE_WITH_EGRESS subnets and one NAT gateway instead. BYO via the
    'vpc' prop (networkMode then selects which of its subnets to use).
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
  - Custom EventBridge bus (horde-<slug>) carrying run-lifecycle events,
    plus a queue-drain Lambda subscribed to run.terminal (see below and
    'horde docs queue' / 'horde docs events')
  - Scoped IAM task role, execution role, status Lambda role, drain Lambda role
  - Managed policy for the horde CLI, exposed as CfnOutput
    CliUserManagedPolicyArn — attach to the IAM principals that
    will run the CLI

Queue, events, and spend cap
----------------------------

The construct provisions the run-lifecycle event backbone: a horde-<slug>
EventBridge bus, the status Lambda emitting run.terminal after each terminal
write, and a drain Lambda that starts the next queued run when a slot frees.
This enables 'horde launch --enqueue' and the 'horde queue' commands (aws-ecs
only). Subscribe your own rules to the bus to react to runs — see
'horde docs events'.

Optional realized spend cap:

    maxSpendPerWindow: 200,                 // USD; omit to disable
    spendWindow: cdk.Duration.hours(24),    // defaults to 24h when cap set

When the realized cost of runs completed in the trailing window meets the cap,
the drain holds queued runs and emits run.cost-threshold-exceeded. Realized-only
(in-flight runs are uncosted until they finish); the maxConcurrent limit is the
blast-radius backstop. See 'horde docs queue'.

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
to cap a sidecar. The '--provider docker' path does not provision sidecars.

Config defaults
---------------

  cpu                       1024 (1 vCPU)
  memoryMiB                 4096 (4 GB)
  maxConcurrent             5
  defaultTimeoutMinutes     1440 (24 h)
  logRetentionDays          30
  ssmParameterPath          /horde/<projectSlug>/config
  networkMode               public (public subnets, no NAT, public IP;
                            'private' for private subnets behind a NAT)
  dataRemovalPolicy         RETAIN (runs table + artifacts bucket survive
                            'cdk destroy'; set DESTROY for ephemeral stacks)
  pointInTimeRecovery       true (DynamoDB continuous backup on the runs table)

The worker security group is egress-only either way (single 443 outbound,
no ingress), so the public-mode public IP enables outbound only — nothing
on the internet can open a connection to a task. Choose 'private' to keep
tasks off public IPs (e.g. to reach private VPC resources, or to limit the
blast radius of a future ingress rule).

Data durability & teardown
--------------------------

The two data-bearing resources — the DynamoDB runs table (run history +
GSIs) and the S3 artifacts bucket (per-run plans, test/review output, logs)
— default to RemovalPolicy.RETAIN, so 'cdk destroy' tears down compute,
networking, and log groups but leaves your run history and artifacts
behind. The runs table also has point-in-time recovery on by default
(35-day continuous backup). For an ephemeral/dev stack you want to fully
clean up, set 'dataRemovalPolicy: RemovalPolicy.DESTROY' (which also enables
the bucket's auto-delete so a non-empty bucket is emptied before removal)
and optionally 'pointInTimeRecovery: false'. Only RETAIN and DESTROY are
accepted — SNAPSHOT is rejected (S3 has no snapshot policy). A
caller-provided 'artifactsBucket' is never re-policied by the construct.

Runtime config contract
-----------------------

The construct writes an SSM parameter at /horde/<projectSlug>/config
whose JSON shape (internal/config/ssm.go::HordeConfig) is what the CLI
reads to discover the cluster, runs table, log group, and ECR repo. The
CLI only ever sees this SSM contract — it has no knowledge of how the
stack was deployed.

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
stack, the simplest route is the Make targets (see 'horde docs
ecs-integration'):

    make e2e-up      # deploy the stack + push the worker image
    make e2e-test    # run the full TestECS_* suite against it
    make e2e-down    # destroy the stack

Equivalently, by hand: bring the stack up, run the suite with
HORDE_E2E_ECS=1 and HORDE_E2E_ECS_BACKEND=cdk (the gate the ECS tests
read to find the deployed stack's SSM path), then tear it down:

    HORDE_E2E_CDK=1 go test -v -timeout 20m -run TestECSCDK_Bringup    ./test/integration/
    HORDE_E2E_ECS=1 HORDE_E2E_ECS_BACKEND=cdk go test -v -timeout 30m \
        -run TestECS -skip TestECSSmoke ./test/integration/
    HORDE_E2E_CDK=1 go test -v -timeout 15m -run TestECSCDK_Teardown   ./test/integration/

Cost: in the default 'public' networkMode this stack has no NAT Gateway,
so its idle cost is negligible; with 'networkMode: "private"' it runs its
own NAT Gateway (~$32/mo idle). DynamoDB point-in-time recovery (on by
default) adds a small continuous-backup charge scaled to the runs table
size; set 'pointInTimeRecovery: false' on a throwaway stack to avoid it.
Note that with the default 'dataRemovalPolicy: RETAIN', tearing down the
stack leaves the runs table + artifacts bucket (and their cost) behind —
pass DESTROY for a fully self-cleaning e2e/dev stack. Always tear down
when you're done.
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
    --status <status>    pending | running | queued | success | failed |
                         killed | timed_out | rate_limited | cancelled
                         (repeatable). Passing --status also includes terminal
                         runs, like --all. ('queued' is the server-side backlog;
                         'cancelled' is a queued run cancelled before it ran —
                         see 'horde docs queue'.)
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
      "summary": {
        "count": 12,
        "total_cost_usd": 4.82,
        "tokens": { "input": 5, "output": 9, "cache_creation": 1, "cache_read": 3, "turns": 40 }
      }
    }

So a label cohort's spend — and token total — is one command away, no
client-side summing (the "tokens" object is omitted when no run in the set
has token data):

    horde list --all --label epic=KS-100 --json | jq .summary

The human table prints the same as a trailing "12 runs, $4.82 total" line.
`

const topicQueue = `Server-Side Queue and Spend Cap

The queue lets you submit more launches than the cluster can run at once and
have horde start them mechanically as slots free. It is aws-ecs only — the
docker provider stays local/pull-based with no background process, so --enqueue
on docker is an error.

ENQUEUE

    horde launch --enqueue --workflow implement-ticket PROJ-1 --priority high

--enqueue writes the launch to the backlog (a run with status "queued") and
exits immediately instead of starting a task. Under --json the status is
"queued" with exit 0, alongside run_id and priority. A queued run does not
consume a concurrency slot — it is waiting for one.

PRIORITY

Five levels: lowest, low, med (default), high, highest. The drain starts the
highest-priority queued run first, then the oldest within a level (by enqueue
time). Priority is the steering lever — horde never infers urgency from ticket
contents. Adjust a waiting run with:

    horde queue prioritize <run-id> --priority highest

INSPECT AND CANCEL

    horde queue list                 # backlog in drain order (what runs next)
    horde queue cancel <run-id>      # cancel a queued run before it ever runs

A cancelled queued run gets the terminal status "cancelled" (distinct from
"killed", which means a running task was stopped — a cancelled run never ran and
never cost anything).

HOW THE DRAIN WORKS

A run reaching a terminal state frees a slot and fires the drain: the drain
checks capacity and the spend cap, then claims and starts the next queued run.
A backstop drain also runs opportunistically on 'horde launch'/'horde list', so
a missed event self-heals the next time you touch the project. There is no
background daemon. If a claimed run fails to start, it is marked "failed" (not
re-queued) and shows up in 'horde list' for you to retry.

The prioritization "brain" lives above horde: a human or an agent watching the
project curates the backlog (bump priority, or 'horde launch --force' to bypass
the queue for a true emergency). horde just launches what's next.

SPEND-RATE CAP

Configure a realized spend cap in the deployment (CDK HordeWorker props
maxSpendPerWindow / spendWindow, written to SSM as max_spend_per_window /
spend_window). When the realized cost of runs completed in the trailing window
meets the cap, the drain is held — queued runs wait — and a
run.cost-threshold-exceeded event is emitted (see 'horde docs events'). The
backlog drains again as the window slides or in-flight runs finish.

Realized-only caveat: cost is known only when a run finishes, so in-flight runs
count as $0 until then and a burst can overshoot the cap before any report cost.
The concurrency limit (max_concurrent) is the real blast-radius backstop. Live
enforcement is a future enhancement.

Spot-interrupted runs are re-queued at top priority and drained the same way —
see 'horde docs spot'.
`

const topicEvents = `Run-Lifecycle Events

On aws-ecs, horde publishes run-lifecycle events to a dedicated EventBridge bus
(named horde-<project-slug>, created by the CDK construct). Subscribe your own
rules to react to runs without polling horde — notifications, metrics, chained
launches, budget alarms.

EVENT TYPES (EventBridge DetailType, Source = "horde")

    run.started                    a run began executing
    run.terminal                   a run reached a terminal state
    run.requeued                   a Spot-interrupted run was re-queued for
                                   auto-resume (not terminal — a continuation)
    run.cost-threshold-exceeded    a drain was held by the spend cap

DETAIL SHAPE (stable, versioned JSON)

    {
      "version": 1,
      "run_id":  "ab12cd34ef56",
      "repo":    "github.com/org/repo",
      "ticket":  "PROJ-1",
      "workflow":"implement-ticket",
      "branch":  "main",
      "status":  "success",          // horde vocabulary, never raw ECS:
                                      //   success|failed|killed|timed_out|
                                      //   rate_limited|cancelled|running
      "exit_code": 0,
      "total_cost_usd": 1.25,
      "labels": { "epic": "KS-100" },
      "started_at":   "2026-06-08T10:00:00Z",
      "completed_at": "2026-06-08T10:42:00Z",
      "stop_code":    "",            // ECS stop diagnostics when present
      "stop_reason":  ""
    }

The "version" field is the contract version; new fields are additive. Events
carry horde's run identity and mapped status directly, so subscribers never
re-derive status from raw ECS task events.

SUBSCRIBING

Add an EventBridge rule on the horde-<slug> bus filtering by detail-type:

    {
      "source": ["horde"],
      "detail-type": ["run.terminal"]
    }

Target a Lambda, SNS topic, Step Function, or anything EventBridge supports. The
queue drain itself is just one such subscriber (an internal rule on run.terminal).

EMISSION SEMANTICS

Events are best-effort and emitted AFTER the authoritative DynamoDB write — the
store is the source of truth; an event is a notification of truth. A failed
publish is logged and never reverses a run's recorded state.
`

const topicSpot = `horde and Fargate Spot

ECS runs launch on Fargate Spot by default for the cost saving. Spot capacity
can be reclaimed by AWS at any time; horde makes runs survive that.

Per-launch capacity
  horde launch <ticket> --capacity spot        # default
  horde launch <ticket> --capacity on-demand   # opt out of reclaim

Capacity is stored on the run and reused on retry and auto-resume, so a run
stays on the kind of capacity it started on. Both providers are always
registered on the cluster; you only pay for the tasks that run.

Auto-resume (aws-ecs, CDK deployments)
When Spot reclaims a task, ECS stops it with stopCode "TerminationNotice".
The worker has already synced its workspace, agent session, and artifacts to
S3 (the same snapshot 'horde retry' uses). The status updater then RE-QUEUES
the run at top priority instead of failing it, and emits a run.requeued event;
the queue drain re-launches it with the same run ID under the normal
concurrency and spend caps, and orc resumes where it left off.

Loop guard
Each automatic resume increments the run's resume_count. After MAX_RESUMES
(default 5, configurable via the CDK construct's maxSpotResumes prop) the run
is left terminal ("killed") instead of resumed; recover it manually with
'horde retry'. A 'horde kill' (stopCode "UserInitiated") is never auto-resumed.
`
