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
		Name:    "env",
		Title:   "Environment Setup",
		Summary: "Required secrets, .env file, token permissions",
		Content: topicEnv,
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

Base image rebuilds when:
    - The image does not exist
    - Any file in ~/.horde/workerfiles/ is newer than the image

Project image rebuilds when:
    - The image does not exist
    - The base image was rebuilt (base is newer than project image)
    - Any file in worker/ is newer than the project image

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

    1. Runs the container in detached mode with environment variables
       for repo URL, ticket, branch, workflow, and run ID.
    2. Secrets from .env are passed via --env-file.
    3. Volume mounts from .horde/config.yaml are applied with -v flags.
    4. The container's entrypoint clones the repo, runs orc, and exits.

Run data is stored locally:

    ~/.horde/horde.db               SQLite run history
    ~/.horde/results/<run-id>/      Artifacts, audit logs, saved logs

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

When a run fails or is killed, the container is preserved with its full
filesystem intact — all code changes, orc state, and audit data.

Retry
-----

    horde retry <run-id>

Restarts the stopped container via 'docker start'. The entrypoint detects
that the workspace already exists (skips clone) and re-runs orc. Orc sees
its own audit state and picks up from the failed phase automatically.

The same run ID is reused — no new run or container is created. The
timeout is reset.

    horde retry abc123
    # Output: Retrying horde-k43 (run abc123)

The run must be in failed or killed status. Successful runs cannot be
retried.

Shell
-----

    horde shell <run-id>

Opens an interactive bash shell into the container's filesystem. Use this
for manual inspection or to run orc commands directly:

    horde shell abc123
    # Inside the container:
    cd /workspace
    orc run horde-k43 --resume          # resume interrupted agent session
    orc run horde-k43 --retry implement # retry from a specific phase

The shell runs in a snapshot of the container (via 'docker commit'), so
changes made in the shell do not affect the original container. The
snapshot is cleaned up on exit.

Clean
-----

    horde clean              # remove all stopped containers
    horde clean <run-id>     # remove a specific container

Containers for completed runs are preserved by default for retry and
shell access. Use 'horde clean' to free up disk space when you no longer
need them. Running and pending runs cannot be cleaned.

Container Lifecycle
-------------------

    horde launch   →  container created and running
    orc completes  →  container stopped (preserved)
    horde status   →  copies artifacts, updates DB (container untouched)
    horde retry    →  container restarted, orc resumes
    horde kill     →  container stopped (preserved)
    horde shell    →  interactive access via snapshot
    horde clean    →  container removed
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
