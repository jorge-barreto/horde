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
		Summary: "Docker (v0.1) and planned AWS ECS (v0.2)",
		Content: topicProviders,
	},
	{
		Name:    "retry",
		Title:   "Retrying and Inspecting Runs",
		Summary: "Retry failed runs, shell access, container cleanup",
		Content: topicRetry,
	},
	{
		Name:    "hydrate",
		Title:   "Hydrating Run Results",
		Summary: "Copy run artifacts locally for orc improve / orc doctor",
		Content: topicHydrate,
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

    horde launch PROJ-123

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
    horde list --all      # include completed, failed, killed

Other useful commands:

    horde kill <run-id>      # stop a running run
    horde retry <run-id>     # restart — orc picks up where it left off
    horde retry <run-id> -- --resume  # pass extra flags through to orc
    horde shell <run-id>     # interactive shell into the container
    horde clean [run-id]     # remove stopped containers
    horde docs <topic>       # read detailed documentation
`

const topicConfig = `Project Configuration
=====================

horde reads optional project-level settings from .horde/config.yaml in
your project root. If the file is missing, horde uses empty defaults.

Schema
------

    mounts:
      - <host-path>:<container-path>

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

    horde launch PROJ-123                   # uses docker by default
    horde launch PROJ-123 --provider docker # explicit

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

AWS ECS Provider (planned — v0.2)
---------------------------------

The ECS provider will run horde-worker as an ECS Fargate task.

    horde launch PROJ-123 --provider aws-ecs

Planned features:
    - ECS RunTask for launching (no Lambda indirection)
    - DynamoDB for shared team run history
    - SSM Parameter Store for infrastructure config discovery
    - CloudWatch for log streaming
    - S3 for artifact storage
    - EventBridge + Lambda for status sync (accurate even if CLI disconnects)
    - Secrets Manager for token injection
`

const topicRetry = `Retrying and Inspecting Runs
=============================

Every run's workspace is mounted to the host at ~/.horde/workspaces/<run-id>/.
If a container vanishes (crash, reboot, OOM), the workspace persists and
retry or shell access still works by launching a new container.

Retry
-----

    horde retry <run-id> [-- <orc-args>...]

Launches a new container against the preserved workspace and sessions
dir. If the old container is still alive, it is stopped first. orc sees
its audit state and picks up from the failed phase automatically; the
agent's Claude session is restored from ~/.horde/workspaces/<run-id>-sessions/
so orc --resume can reattach to the in-flight conversation.

The same run ID is reused. The timeout is reset.

    horde retry abc123
    horde retry abc123 -- --resume
    horde retry abc123 -- --retry implement

Extra orc flags after -- are passed through to orc unchanged. horde does
not validate them.

The run must be in failed or killed status. Successful runs cannot be
retried.

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

Workspace Persistence
---------------------

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

horde validates the .env file before every launch. It checks that both
CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN keys are present (but does not
verify that the values are valid tokens — that fails later at clone
or orc execution time).
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
      | jq -r '.[].id' \\
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
  horde launch --provider aws-ecs -t TICKET-123
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
(AnthropicApiKey, GitToken). It never logs or echoes them:
  - Interactive: when stdin is a TTY, horde prompts with hidden input.
  - Headless / CI: when stdin is not a TTY, horde reads ANTHROPIC_API_KEY
    and GIT_TOKEN from the environment. Missing either is a hard error.

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
