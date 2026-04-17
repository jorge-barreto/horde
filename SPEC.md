# horde — Cloud Launcher for orc Workflows

## Context

orc is a deterministic agent orchestrator that runs workflows locally. horde is the deployment layer that runs orc on ephemeral cloud instances. orc handles the "what" (workflow phases), horde handles the "where" (infrastructure).

horde is project-agnostic: any repo with an `.orc/` directory is deployable. The workflow config IS the deployment spec.

Git is a hard requirement — horde clones a repo to run orc against it. Running horde outside a git repository is an error. All commands are scoped to the current repo (inferred from `git remote get-url origin`).

## Design Principles

1. **orc knows nothing about horde** — orc runs the same way locally or in the cloud
2. **horde knows nothing about orc internals** — it launches a command, monitors it, collects the result
3. **Ephemeral instances** — spin up, clone, run, collect, tear down
4. **Infra is pre-provisioned** — horde submits jobs, doesn't create clusters
5. **AWS-first** — designed for AWS with interfaces that allow other providers later
6. **Team-first, solo-friendly** — shared visibility and run history are the default; local dev mode exists for testing the pipeline, not as the product

## Users

Teams of full-stack developers who manage their own DevOps. The primary use case is a team lead swarming 3-5 tickets at end of day so review work is queued for the team next morning. Any team member can see all runs, check status, read logs, and retrieve results.

The solo developer on a laptop is a secondary use case — useful for testing workflows end-to-end before deploying to ECS, but not the product.

## Architecture

### v0.1 — Local (Docker)

```
Developer laptop
┌──────────────────────────────┐
│  horde CLI                   │
│   └─► docker run             │
│        ┌──────────────────┐  │
│        │ horde-worker     │  │
│        │  ├── orc         │  │
│        │  ├── claude CLI  │  │
│        │  ├── bd          │  │
│        │  └── git         │  │
│        └──────────────────┘  │
│                              │
│  SQLite (~/.horde/horde.db)  │
└──────────────────────────────┘
```

The docker provider runs the worker image locally. Run history is stored in a local SQLite database. Repo URL is inferred from the local git remote. Secrets are loaded from a `.env` file. This mode is for testing the horde pipeline — not for production use.

### v0.2 — AWS (ECS)

```
Developer laptop                         AWS
┌──────────────┐                ┌──────────────────────────────┐
│  horde CLI   │──RunTask──────►│  ECS Fargate                 │
│              │                │  ┌────────────────┐          │
│              │◄─CloudWatch────│  │ horde-worker   │          │
│              │   logs         │  │  ├── orc       │          │
│              │                │  │  ├── claude CLI│          │
│              │◄─S3────────────│  │  ├── bd        │          │
│              │   artifacts    │  │  └── git       │          │
│              │                │  └────────────────┘          │
│              │◄─DynamoDB──────│                               │
│              │   run history  │  DynamoDB (shared runs)       │
└──────────────┘                │  SSM (config)                 │
                                │  Secrets Manager              │
                                │  S3 (artifacts)               │
                                │  CloudWatch (logs)            │
                                │                               │
                                │  EventBridge ──► Status Lambda│
                                │  (ECS task state → DynamoDB)  │
                                └──────────────────────────────┘
```

The horde CLI reads infrastructure config from SSM Parameter Store, then calls ECS `RunTask` directly. Run history is stored in DynamoDB — shared across the entire team. Any developer with AWS credentials can see all runs, check logs, and retrieve results.

**EventBridge status sync:** An EventBridge rule captures ECS task state changes and triggers a small Lambda that updates the DynamoDB run record. This ensures run status is accurate even if the CLI user closes their terminal, loses network, or crashes. Without this, DynamoDB records would go stale and `horde list`, duplicate ticket checks, and concurrency enforcement would be unreliable.

The status Lambda:
- Receives ECS task state change events filtered to the horde cluster
- Extracts the task ARN and maps it to a run ID via DynamoDB query
- Updates `status`, `exit_code`, `completed_at`, and `total_cost_usd` (reads `run-result.json` from S3 if present) on terminal states (STOPPED)
- Is idempotent — CLI-driven updates and Lambda-driven updates converge to the same state

## CLI Commands

```
horde launch [--branch=<branch>] [--workflow=<name>] [--timeout=<duration>] [--force] <ticket>
horde status <run-id>
horde logs <run-id> [--follow]
horde kill <run-id>
horde results <run-id>
horde list [--all]                              # active runs for current repo; --all includes completed/failed
horde health                                    # v0.3
horde sweep                                     # v0.3
horde retry <run-id>                            # v0.4
horde init                                      # v0.4
horde artifacts <run-id> [path]                 # v0.4
horde stats [--range=<duration>]                # v0.4
horde swarm <ticket1> [ticket2] ...             # v0.5
```

Global flags:
- `--provider` — Override provider selection (`docker` or `aws-ecs`). Default is the newest available provider.
- `--profile` — AWS named profile (passed through to AWS SDK) (v0.2)
- `--json` — Machine-readable JSON output (v0.2)

`--json` applies to: `status`, `results`, `list`, `health`. Output schemas are stable per major version.

horde doesn't understand tickets, waves, or beads. `horde launch` runs `orc run <ticket> --auto --no-color` on an ephemeral instance. orc's workflow decides whether the ticket is an epic, whether to loop, etc.

## Run Lifecycle

### 1. Launch

```
horde launch PROJ-123
```

horde:
- Resolves repo URL from the local git remote (`git remote get-url origin`)
- Generates a run ID
- **Concurrency check (v0.2):** Queries the store for active runs. If the count equals or exceeds `maxConcurrent` (from SSM config), launch is rejected with an error showing current active runs. No queuing — the user decides what to do.
- **Duplicate ticket check:** Queries the store for active runs with the same ticket. If found, warns and requires `--force` to proceed.
- Records the run in the store as `pending` (SQLite for docker, DynamoDB for ECS)
- Starts the worker container (docker provider) or calls ECS RunTask (ECS provider)
- Updates the run to `running` with the instance ID (container ID or task ARN)
- Prints the run ID
- The container entrypoint: clone repo → `orc run <ticket> --auto --no-color`

### 2. Monitor

orc writes to stdout (captured by Docker logs or CloudWatch).

`horde logs <run-id>` streams logs from the container (docker: `docker logs`, ECS: CloudWatch). For docker, logs are only available while the container exists — once a run completes and the container is removed, logs are gone. (For ECS, logs persist in CloudWatch.)
`horde logs <run-id> --follow` tails in real time until the run completes.
`horde status <run-id>` shows detail for a single run: status, exit code, duration, cost, and who launched it. Use `horde list` to see multiple runs.

### 3. Completion

When orc exits:
- The entrypoint script exits with orc's exit code
- For ECS: entrypoint uploads artifacts to S3 before exiting; EventBridge Lambda updates DynamoDB
- For docker: completion is detected lazily — the next `horde status`, `horde results`, or `horde list` call checks container state via `docker inspect`. On detecting completion, horde:
  1. Copies `.orc/audit/` and `.orc/artifacts/` from the container to `~/.horde/results/<run-id>/`
  2. Updates the store with exit code, status (mapped from exit code), completion time, and `total_cost_usd` (from `run-result.json` if present)
  3. Removes the container
- Instance is torn down (docker container removed, ECS Fargate task is destroyed)

### 4. Results

```
horde results <run-id>
```

The CLI first checks the run's status in the store:
- **Still running**: reports that the run is in progress — does not call `ReadFile`.
- **Completed**: reads `run-result.json` via the provider's `ReadFile` and displays a formatted summary: status, total cost, total duration, and per-phase breakdown (phase name, status, cost, duration).
- **Missing `run-result.json`** (e.g., orc crashed before writing it): reports what is known from the run record (exit code, status, cost if stored) and notes that detailed results are unavailable.

For docker, `ReadFile` reads from the local results store (`~/.horde/results/<run-id>/`), where `.orc/audit/` and `.orc/artifacts/` were copied at completion. For ECS, `ReadFile` reads from S3.

### 5. Failure

horde reports and stops. No retries. No auto-remediation.
Exit code from orc tells horde what happened:
- 0: workflow completed successfully
- 1: phase failure (agent fail, script fail, gate denied, loop exhaustion, missing outputs)
- 2: phase timed out
- 3: configuration or setup error
- 4: cost limit exceeded (per-phase or per-run)
- 5: signal interrupt (SIGINT/SIGTERM/SIGHUP)
- 6: resume failure (cannot recover interrupted session)

horde maps these to run statuses: 0 → `success`, 1/2/3/4/6 → `failed`, 5 → `killed`.

`horde status` shows the failure. `horde logs` shows what happened. Human decides next step.

### 6. Kill

`horde kill <run-id>` stops a running instance:
- **Docker provider:** calls `docker stop`, attempts a best-effort copy of `.orc/audit/` and `.orc/artifacts/` (may be incomplete or absent if orc hadn't written results yet), extracts `total_cost_usd` and `exit_code` from `run-result.json` if present, updates the store to `killed`, removes the container.
- **ECS provider:** calls `ecs:StopTask`. The EventBridge Lambda updates DynamoDB when the task reaches STOPPED state.

### 7. Timeout

Runs have a maximum duration. Default: 24 hours. Override with `--timeout` on `horde launch`.

- **Docker provider:** timeout is enforced lazily. Each `status`, `results`, or `list` call checks `timeout_at` against the current time. If a run has exceeded its timeout, horde calls `Kill()` and marks the run as `killed`.
- **ECS provider:** The CDK construct sets `stopTimeout` on the Fargate task definition. Additionally, horde records the timeout in DynamoDB. The EventBridge Lambda checks the timeout and stops overdue tasks.

The timeout covers the entire run including git clone, orc execution, and artifact upload.

## Instance Setup

### Base Docker Image

Published as `ghcr.io/<org>/horde-worker:latest`:

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y git jq bash curl unzip \
    && rm -rf /var/lib/apt/lists/*
# AWS CLI v2 (needed for ECS artifact upload; present but unused in docker provider)
RUN curl "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m).zip" -o awscliv2.zip \
    && unzip awscliv2.zip && ./aws/install && rm -rf aws awscliv2.zip
# Install orc
COPY --from=orc-builder /orc /usr/local/bin/orc
# Install claude CLI
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/root/.claude/bin:${PATH}"
# Install bd
COPY --from=bd-builder /bd /usr/local/bin/bd
COPY entrypoint.sh /entrypoint.sh
COPY git-askpass.sh /usr/local/bin/git-askpass.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/git-askpass.sh
ENTRYPOINT ["/entrypoint.sh"]
```

### Entrypoint Script

```bash
#!/bin/bash
set -uo pipefail

# Clone repo using credential helper (avoids token in process args and .git/config)
export GIT_ASKPASS="/usr/local/bin/git-askpass.sh"
if ! git clone "https://${REPO_URL}" /workspace; then
    echo "ERROR: git clone failed" >&2
    exit 3  # configuration/setup error
fi
cd /workspace || { echo "ERROR: cd /workspace failed" >&2; exit 3; }
if [ -n "${BRANCH:-}" ]; then
    if ! git checkout "$BRANCH"; then
        echo "ERROR: git checkout failed for branch ${BRANCH}" >&2
        exit 3
    fi
fi

# Run orc
if [ -n "${WORKFLOW:-}" ]; then
    orc run -w "$WORKFLOW" "$TICKET" --auto --no-color
else
    orc run "$TICKET" --auto --no-color
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
```

`git-askpass.sh` is a helper script baked into the Docker image:

```bash
#!/bin/bash
echo "x-access-token:${GIT_TOKEN}"
```

This keeps the token out of process arguments, `.git/config`, and error messages from failed clones.

### Entrypoint Environment Variables

The provider maps `LaunchOpts` to container environment variables:

| Env Var | Source | Notes |
|---------|--------|-------|
| `REPO_URL` | `LaunchOpts.Repo` | Normalized, no scheme (e.g., `github.com/org/repo.git`) |
| `TICKET` | `LaunchOpts.Ticket` | Passed as argument to `orc run` |
| `BRANCH` | `LaunchOpts.Branch` | Empty if not specified (entrypoint skips checkout) |
| `WORKFLOW` | `LaunchOpts.Workflow` | Empty if default workflow |
| `RUN_ID` | `LaunchOpts.RunID` | Used by S3 upload path (ECS only) |
| `ARTIFACTS_BUCKET` | ECS config (SSM) | Absent in docker mode — triggers S3 upload when present |
| `ANTHROPIC_API_KEY` | `.env` (docker) / Secrets Manager (ECS) | Required by orc |
| `GIT_TOKEN` | `.env` (docker) / Secrets Manager (ECS) | Used by `GIT_ASKPASS` helper |

### Team Customization

Teams extend the base image with project-specific tools:

```dockerfile
FROM ghcr.io/org/horde-worker:latest
RUN apt-get install -y nodejs npm
RUN npm install -g yarn
```

## Secrets

### Docker Provider (v0.1)

The docker provider reads secrets from a `.env` file in the project directory (must be gitignored):

```
# .env (gitignored)
ANTHROPIC_API_KEY=sk-ant-...
GIT_TOKEN=ghp_...
```

horde passes this file to the container via `docker run --env-file .env`. Before launching, horde validates:
- The `.env` file exists (error if missing)
- The `.env` file contains `ANTHROPIC_API_KEY` (error if missing)
- The `.env` file contains `GIT_TOKEN` (error if missing)

Validation checks that the keys are defined, not that the values are valid — bad credentials are caught at runtime by the container.

### ECS Provider (v0.2)

Secrets are injected via Secrets Manager `valueFrom` in the ECS task definition — never as plain environment variables. The CDK construct wires this up. Developers never handle secrets directly in horde config.

- **API key limits**: Anthropic API key with usage limits set at the key level.
- **Scoped git token**: Fine-grained GitHub PAT — read + push to `horde/*` branches only. Cannot push to main, cannot delete branches.

## Configuration

### v0.1 — Zero Config

No configuration file. Everything is inferred:
- **Repo URL**: from `git remote get-url origin` in the working directory. horde normalizes the URL for HTTPS cloning: strips the `https://` scheme if present, and converts SSH format (`git@github.com:org/repo.git`) to HTTPS format (`github.com/org/repo.git`). The entrypoint prepends `https://` when cloning.
- **Provider**: `docker` (only provider in v0.1)
- **Docker image**: hardcoded to `horde-worker:latest` (no override flag)
- **Secrets**: from `.env` file in the project directory

### v0.2 — SSM Discovery for ECS

For the `aws-ecs` provider, all config is discovered from a single SSM parameter. The CDK construct writes a JSON parameter to `/horde/config`:

```json
{
  "cluster_arn": "arn:aws:ecs:...",
  "task_definition_arn": "arn:aws:ecs:...",
  "subnets": ["subnet-abc", "subnet-def"],
  "security_group": "sg-123",
  "log_group": "/ecs/horde-worker",
  "log_stream_prefix": "ecs",
  "artifacts_bucket": "my-horde-artifacts",
  "runs_table": "horde-runs",
  "max_concurrent": 5,
  "default_timeout_minutes": 1440
}
```

The CLI reads this using AWS credentials (via `--profile` or default credential chain) and calls `RunTask` directly. No ARNs or networking details in local config.

**Provider selection:** The newest available provider is always the default — in v0.2+, that is `aws-ecs`. There is no silent fallback. If the ECS provider fails to initialize (missing credentials, SSM parameter unreadable), horde errors with a diagnostic message. To use the docker provider explicitly, pass `--provider docker`.

**Note:** Provider determines store. `--provider docker` always uses SQLite — runs launched this way are local-only and invisible to the team's shared DynamoDB history. This is intentional: docker mode is for local testing, not a degraded production path.

## Data Model

### Store Interface

The `Store` interface has two implementations — SQLite for local testing, DynamoDB for production. The CLI selects the implementation based on the provider (docker → SQLite, aws-ecs → DynamoDB).

```go
type Store interface {
    CreateRun(ctx context.Context, run *Run) error
    GetRun(ctx context.Context, id string) (*Run, error)
    UpdateRun(ctx context.Context, id string, update *RunUpdate) error
    ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error)
    FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error)
    CountActive(ctx context.Context) (int, error)    // v0.2 — cluster-wide concurrency check
    ListActive(ctx context.Context) ([]*Run, error)  // v0.2 — list active runs for concurrency error detail
}
```

```go
type RunUpdate struct {
    Status       *Status
    InstanceID   *string
    Metadata     map[string]string // nil = don't update
    ExitCode     *int
    CompletedAt  *time.Time
    TotalCostUSD *float64
}
```

### SQLite (v0.1 — docker provider)

Database: `~/.horde/horde.db`

Lean schema for local testing. Not shared, not the production path.

| Column | Type | Notes |
|--------|------|-------|
| id | TEXT | Run ID (primary key) — see Run ID Generation below |
| repo | TEXT | Repository URL |
| ticket | TEXT | ID passed to orc run |
| branch | TEXT | Git branch (empty = repo default) |
| workflow | TEXT | orc workflow name (empty = default) |
| provider | TEXT | docker |
| instance_id | TEXT | Container ID |
| metadata | TEXT | JSON-encoded map[string]string — provider-specific data (NULL if none) |
| status | TEXT | pending, running, success, failed, killed |
| exit_code | INTEGER | orc exit code (NULL while running) |
| launched_by | TEXT | Local git user name (from `git config user.name`) |
| started_at | TEXT | RFC3339 |
| completed_at | TEXT | RFC3339 (NULL while running) |
| timeout_at | TEXT | RFC3339 — when this run should be killed |
| total_cost_usd | REAL | Total cost from run-result.json (NULL if unavailable) |

### DynamoDB (v0.2 — ECS provider)

Table: `horde-runs` (created by CDK construct)

This is the production store — shared across the team. Every developer with AWS credentials sees the same run history.

- Partition key: `id` (String) — short random run ID
- GSI `by-repo`: partition key `repo`, sort key `started_at` — for repo-scoped listing (`horde list`)
- GSI `by-ticket`: partition key `ticket`, sort key `started_at` — for duplicate ticket check and ticket history
- GSI `by-status`: partition key `status`, sort key `started_at` — for listing active runs and concurrency check
- GSI `by-instance`: partition key `instance_id`, projection ALL — for status Lambda task ARN lookup

| Attribute | Type | Notes |
|-----------|------|-------|
| id | S | Run ID (partition key) — see Run ID Generation below |
| repo | S | Repository URL |
| ticket | S | ID passed to orc run |
| branch | S | Git branch (empty = repo default) |
| workflow | S | orc workflow name (empty = default) |
| provider | S | aws-ecs |
| instance_id | S | ECS task ARN |
| status | S | pending, running, success, failed, killed |
| exit_code | N | orc exit code (null while running) |
| launched_by | S | IAM identity (from `sts:GetCallerIdentity`) |
| started_at | S | RFC3339 |
| completed_at | S | RFC3339 (null while running) |
| timeout_at | S | RFC3339 — when this run should be killed |
| total_cost_usd | N | Total cost from run-result.json (null if unavailable) |
| cluster_arn | S | ECS cluster ARN |
| log_group | S | CloudWatch log group |
| artifacts_bucket | S | S3 bucket name |
| artifacts_uri | S | S3 URI prefix for this run's artifacts |
| ttl | N | Unix epoch for DynamoDB TTL (v0.3) |

Pay-per-request billing — essentially free at low-to-moderate volume.

### Additional audit fields (v0.3)

| Attribute | Type | Notes |
|-----------|------|-------|
| commit_sha | S | HEAD commit after checkout |
| image_digest | S | Container image SHA256 |
| cli_version | S | horde CLI version |
| killed_by | S | IAM identity of who killed the run |

## Run ID Generation

Run IDs are 12-character lowercase alphanumeric strings generated from `crypto/rand`. Character set: `abcdefghijklmnopqrstuvwxyz0123456789` (36 chars). This gives ~62 bits of entropy — collision probability stays below 1 in a billion through ~50,000 runs.

Example: `k7m2xp4qr9n3`

The format is chosen to be:
- Short enough to type and remember
- Safe in URLs, filenames, shell arguments, and DynamoDB keys
- Collision-resistant for any realistic usage volume

## Security

### v0.2 — Baseline

- **Scoped IAM for tasks**: Task role can only write to the artifacts S3 bucket and read specific secrets. No EC2, no other AWS services.
- **Scoped IAM for CLI users**: The CDK construct outputs a managed policy ARN. Developers need: `ecs:RunTask`, `ecs:DescribeTasks`, `ecs:StopTask` on the cluster; `ecs:TagResource` on task resources under the cluster (e.g., `arn:aws:ecs:*:*:task/<cluster>/*`); `iam:PassRole` on the horde task role and execution role (e.g., `arn:aws:iam::*:role/horde-*`); `ssm:GetParameter` on `/horde/config`; `dynamodb:PutItem`, `dynamodb:GetItem`, `dynamodb:UpdateItem`, `dynamodb:Query` on `horde-runs` table; `logs:GetLogEvents` on the log group; `s3:GetObject` on the artifacts bucket.
- **Scoped git token**: Fine-grained GitHub PAT — read + push to `horde/*` branches only. Cannot push to main, cannot delete branches.
- **API key limits**: Anthropic API key with usage limits set at the key level.
- **Ephemeral**: Instance is destroyed after the run. No persistent state, no attack surface.
- **Secrets via valueFrom**: API keys and tokens injected via Secrets Manager ARN references, not plain environment variables.
- **GIT_TOKEN protected**: Credential helper (`GIT_ASKPASS`) keeps the token out of process args, `.git/config`, and error messages.

### v0.3 — Hardening

- **Tightened IAM**: Task role S3 policy scoped to `horde-runs/${RUN_ID}/*` prefix. CLI DynamoDB policy restricted to specific actions (no `DeleteTable`, `DeleteItem`, or unconditional `UpdateItem`). DynamoDB condition expressions prevent modifying completed run records.
- **Container hardening**: Run as non-root user (`USER horde`). Read-only root filesystem with tmpfs at `/workspace` and `/tmp`. Fargate platform version pinned.
- **Supply chain**: Base image pinned by SHA256 digest. AWS CLI install verified by checksum. orc and bd binaries verified by checksum or signature. Claude CLI installed from a versioned, pinned artifact.
- **Encryption at rest**: KMS CMK for S3 bucket, DynamoDB table, and CloudWatch log group. S3 bucket policy enforcing `aws:SecureTransport`.
- **Network egress**: VPC endpoints for S3, Secrets Manager, CloudWatch Logs, ECR, SSM, STS. NAT gateway for GitHub and Anthropic API traffic. Security group egress restricted to VPC endpoints + NAT gateway.
- **CloudWatch Logs data protection**: Policy to detect and mask patterns matching API keys and tokens.
- **Image scanning**: ECR scan-on-push enabled. CDK construct configures it by default.

### v0.4 — Audit and compliance

- **Immutable audit log**: DynamoDB Streams piped to S3 (via Kinesis Data Firehose) for append-only run history. Run records cannot be retroactively modified once archived.
- **CloudTrail coverage**: Data events enabled for S3, DynamoDB, and Secrets Manager.
- **S3 access logging**: Server access logging on the artifacts bucket.
- **Branch trust (optional)**: Configurable allowlist of branches/refs that horde will execute. Rejects launches against unreviewed branches. Off by default (teams opt in).

## Provider Interface

```go
type Provider interface {
    Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
    Status(ctx context.Context, instanceID string) (*InstanceStatus, error)
    Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error)
    Kill(ctx context.Context, opts KillOpts) error
    ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error)
    Finalize(ctx context.Context, run *store.Run, homeDir string) error
}

type LaunchOpts struct {
    Repo     string
    Ticket   string
    Branch   string
    Workflow string
    RunID    string
    EnvFile  string            // path to .env file (docker provider)
}

type LaunchResult struct {
    InstanceID string            // container ID or ECS task ARN
    Metadata   map[string]string // provider-specific data (ECS: cluster_arn, log_group, etc.)
}

type InstanceStatus struct {
    State      string // pending, running, stopping, stopped, unknown
    ExitCode   *int   // nil while running
    StartedAt  time.Time
    FinishedAt *time.Time // nil while running
}

type KillOpts struct {
    InstanceID string // container ID or ECS task ARN
    ResultsDir string // per-run results directory for artifact copy (docker); empty to skip copy
}

type ReadFileOpts struct {
    InstanceID string            // container ID or ECS task ARN
    Path       string            // logical path relative to project root (e.g., ".orc/audit/<ticket>/run-result.json")
    RunID      string            // run ID (used by ECS provider to resolve S3 prefix)
    Metadata   map[string]string // provider-specific metadata from LaunchResult (ECS: artifacts_bucket, etc.)
}
```

`KillOpts.ResultsDir` is used by the docker provider to copy `.orc/audit/` and `.orc/artifacts/` from the container before removal. The ECS provider ignores it — ECS artifacts are uploaded to S3 by the entrypoint before the task stops.

`ReadFile` is used by `horde results` to read `run-result.json`. The caller checks the run's status in the store first — if the run is still in progress, it reports that without calling `ReadFile`. The caller passes a logical path relative to the project root (e.g., `.orc/audit/<ticket>/run-result.json`) along with the run ID and provider metadata. Each provider resolves the path internally:
- **docker**: reads from the local results store (`~/.horde/results/<run-id>/`) where `.orc/audit/` and `.orc/artifacts/` were copied at completion
- **ECS**: maps the logical path to an S3 key using the run ID and artifacts bucket from metadata

Shipped providers:
- `docker` — Runs in a local Docker container using the worker image. For local testing.
- `aws-ecs` — Calls ECS RunTask directly with SSM-discovered config. Production provider.

## Infrastructure: CDK Construct (v0.2)

Teams import a CDK construct into their existing infra stack. It creates everything horde needs.

```typescript
import { HordeWorker } from '@horde/cdk';

new HordeWorker(this, 'Horde', {
  vpc: existingVpc,                             // optional — creates one if not provided
  workerImage: ecs.ContainerImage.fromAsset('./horde-worker'),
  artifactsBucket: existingBucket,              // optional — creates one if not provided
  secrets: {
    ANTHROPIC_API_KEY: secretsmanager.Secret.fromSecretNameV2(...),
    GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(...),
  },
  // Optional overrides
  cpu: 1024,                     // 1 vCPU (default)
  memoryMiB: 4096,               // 4 GB (default)
  maxConcurrent: 5,              // Max simultaneous tasks (enforced by CLI, default 5)
  defaultTimeoutMinutes: 1440,   // Default run timeout (default 1440 = 24h)
  logRetentionDays: 30,          // CloudWatch log retention
  ssmParameterPath: '/horde/config',
});
```

The construct creates:
- ECS cluster (or uses existing one if provided)
- Fargate task definition with the worker image
- Secrets injected via `valueFrom` (Secrets Manager ARN injection)
- IAM task role (scoped: S3 write to artifacts bucket, Secrets Manager read for secrets)
- IAM execution role (ECR pull, CloudWatch Logs write)
- CloudWatch log group
- S3 bucket for artifacts (if not provided)
- DynamoDB table (`horde-runs`) with GSIs for repo, ticket, and status queries
- Security group (egress-only — needs internet for git clone + API calls)
- **EventBridge rule** — captures ECS task state changes for the horde cluster
- **Status sync Lambda** — triggered by EventBridge, updates DynamoDB run records on task completion/failure
- SSM Parameter at `/horde/config` — JSON blob with all config the CLI needs
- IAM managed policy for CLI users (optional output — teams attach to developer IAM roles/groups)

## Project Structure

```
~/work/horde/
  cmd/horde/main.go              # CLI entrypoint (urfave/cli/v3)
  internal/provider/provider.go  # Provider interface
  internal/provider/docker.go    # Docker container provider
  internal/store/store.go        # Run history store interface
  internal/store/sqlite.go       # SQLite implementation
  internal/config/config.go      # Config resolution (git remote, .env)
  internal/runid/runid.go        # Run ID generation
  docker/
    Dockerfile                   # Base worker image
    entrypoint.sh
    git-askpass.sh               # Credential helper for GIT_TOKEN
  go.mod
  SPEC.md
  ORC_CONTRACT_EXPECTATIONS.md
  README.md
  LICENSE                        # MIT
```

v0.2 additions:
```
  internal/provider/ecs.go       # ECS provider implementation
  internal/store/dynamo.go       # DynamoDB implementation
  internal/config/ssm.go         # SSM parameter discovery
  cdk/
    lib/horde-worker.ts          # CDK construct
    lib/status-lambda/index.ts   # EventBridge → DynamoDB status sync
    package.json
    tsconfig.json
```

## Dependencies on orc

| Feature | Why |
|---------|-----|
| `--auto` flag | Unattended mode — no gates, no interactive prompts, no stdin reads |
| `--no-color` flag | Clean output in CloudWatch (no ANSI escape codes) |
| `-w <name>` flag | Select a named workflow from `.orc/workflows/` |
| Exit codes 0–6 | Structured exit codes for status mapping (see ORC_CONTRACT_EXPECTATIONS.md) |
| `run-result.json` | Machine-readable outcome with cost, duration, per-phase breakdown |
| Headless mode | No TTY required when `--auto` is set |

See `ORC_CONTRACT_EXPECTATIONS.md` for the full interface contract.

## What horde does NOT do

- Run orc phases (that's orc's job)
- Manage beads/work items (that's bd's job)
- Handle git merges or conflict resolution (PRs are orc's workflow, merges are human)
- Retry failed runs automatically (human decides; `horde retry` is a convenience, not automation)
- Create infrastructure (teams deploy the CDK construct)
- Understand tickets, waves, epics, or beads (it just runs `orc run <thing>`)
- Manage permissions per-user (any team member with AWS credentials can launch/kill/view any run)
- Queue excess launches (rejects when at capacity; queuing is a future consideration)

## Milestones

### v0.1 — Docker testing

Local-only. Validates the horde pipeline end-to-end without AWS.

- Provider interface defined
- Store interface defined
- `docker` provider: runs worker image locally
- SQLite store for local run history
- All CLI commands: `launch`, `status`, `logs`, `kill`, `results`, `list`
- Cost display: `status` and `list` show cost from `run-result.json` when available
- `.env` validation: checks for required keys (`ANTHROPIC_API_KEY`, `GIT_TOKEN`) before launch
- Zero-config: repo from git remote, secrets from `.env`, image hardcoded to `horde-worker:latest`
- Base worker Docker image + entrypoint (with `git-askpass.sh` credential helper)
- Run ID generation (12-char lowercase alphanumeric, `crypto/rand`)
- Duplicate ticket warning (`--force` to override)
- Run timeout (`--timeout` flag, default 24h, enforced lazily on next status/results/list call)
- `--provider` flag for explicit provider selection

### v0.2 — AWS MVP

The minimum viable team product. A team can deploy this and start swarming tickets.

- `aws-ecs` provider: direct ECS RunTask with SSM-discovered config
- DynamoDB store for shared team run history
- SSM parameter discovery for infrastructure config
- CloudWatch log streaming via `horde logs`
- S3 artifact upload (entrypoint) and retrieval (`horde results`)
- CDK construct — creates: ECS cluster, task def, DynamoDB table, S3 bucket, SSM parameter, IAM roles, security groups, EventBridge rule, status sync Lambda
- EventBridge → Lambda status sync (ECS task state → DynamoDB)
- `maxConcurrent` enforcement (error on breach, configurable via CDK prop, default 5)
- `--profile` flag for AWS credential selection
- `--json` flag on `status`, `results`, `list`
- `launched_by` field (IAM identity via `sts:GetCallerIdentity`)
- `timeout_at` field, enforced by Fargate `stopTimeout` and status Lambda
- IAM managed policy for CLI users (CDK output)
- Scoped IAM for task role and CLI users (specific DynamoDB actions, not `dynamodb:*`)
- GIT_TOKEN protected via `GIT_ASKPASS` credential helper
- S3 upload failure logged (not silently swallowed)

### v0.3 — Security hardening

Enterprise trust. What a security team needs to approve production use.

- **Container hardening**: non-root user, read-only rootfs, Fargate platform version pinned
- **Supply chain**: base image pinned by digest, downloads checksum-verified, image scanning (ECR scan-on-push)
- **Encryption at rest**: KMS CMK for S3, DynamoDB, CloudWatch Logs. S3 bucket policy enforcing TLS.
- **Network egress restrictions**: VPC endpoints for AWS services, NAT gateway for external traffic, security group egress tightened
- **Tightened IAM**: task role S3 scoped to run prefix, DynamoDB condition expressions preventing tampering with completed records
- **Audit metadata in run records**: `commit_sha`, `image_digest`, `cli_version`, `killed_by`
- **CloudWatch Logs data protection**: masking of API keys and tokens in log output
- **Resource lifecycle**: DynamoDB TTL on run records (configurable, default 90 days), S3 lifecycle rules on artifacts bucket, CloudWatch log archival to S3
- **`horde health` command**: verifies ECS cluster reachable, SSM parameter readable, DynamoDB table accessible, Secrets Manager secrets resolvable, S3 bucket writable, CloudWatch log group exists
- **Stale run detection**: `horde sweep` command marks runs stuck in `pending`/`running` beyond timeout as `failed`, stops underlying ECS tasks. Status Lambda also detects and handles this.
- **ECS task tagging**: tasks tagged with `horde-run-id`, `horde-ticket`, `horde-launched-by` for cost allocation and reconciliation

### v0.4 — Team experience

What makes teams love the tool.

- **`horde retry <run-id>`**: re-launches with the same parameters (ticket, branch, workflow). Does not resume — starts fresh.
- **Onboarding**: `horde init` validates setup (Docker daemon running, image available, `.env` present for docker; AWS credentials valid, SSM parameter readable for ECS). Generates `.env.example` template.
- **Notification webhook**: configurable URL in SSM config, JSON POST on run completion/failure. Payload includes: run_id, ticket, status, exit_code, cost, duration, launched_by. Enables Slack/PagerDuty/custom integrations.
- **Artifact browsing**: `horde artifacts <run-id>` lists artifact files. `horde artifacts <run-id> <path>` downloads a specific file. For ECS, reads from S3 listing.
- **CloudWatch custom metrics**: CDK construct publishes `horde/ActiveRuns`, `horde/RunsLaunched`, `horde/RunsFailed`, `horde/RunDuration` as CloudWatch custom metrics. Enables native AWS dashboards and alarms.
- **CloudWatch alarms**: CDK construct creates default alarms for: failure rate > threshold, runs stuck in pending, active runs approaching maxConcurrent.
- **Structured logging from horde CLI**: JSON log lines with `run_id`, `ticket`, `event`, `timestamp` for aggregation and cross-run search.
- **Graceful shutdown on kill**: `horde kill` sends SIGTERM with configurable grace period (default 30s) before SIGKILL. Gives orc time to write `run-result.json`. Entrypoint traps SIGTERM for cleanup.
- **Run tagging**: `horde launch --label key=value` for custom metadata. Stored in DynamoDB. Filterable in `horde list --label key=value`.
- **Reporting**: `horde stats` shows aggregate metrics: total runs, success rate, total cost, average duration. Filterable by date range, ticket, launched_by.

### v0.5 — Scale and integration

- **Multi-repo support**: SSM path parameterized per repo (e.g., `/horde/config/<repo-name>`). CDK construct supports multiple task definitions with different images. `horde list --repo` filter.
- **CI/CD integration**: GitHub Action that runs `horde launch` on ticket label events. Example workflow provided in docs.
- **Batch launch**: `horde swarm <ticket1> <ticket2> ...` launches multiple tickets. Returns a swarm ID grouping the runs. `horde kill --swarm <swarm-id>` kills all runs in a group.
- **Concurrency queuing**: when at capacity, `horde launch --wait` queues the run and launches when a slot opens. SQS-backed queue, processed by the status Lambda on task completion.
- **REST API**: Optional API Gateway + Lambda exposing `list`, `status`, `results` endpoints for dashboards and bots. Read-only.
- **Budget controls**: configurable weekly/monthly spend threshold. horde estimates Fargate cost (duration × CPU/memory price) + orc API cost (from run-result.json). Warns or blocks launches when approaching threshold.
- **Branch trust verification**: configurable allowlist of branches/refs. Rejects launches against unreviewed branches. Off by default.
- **Immutable audit log**: DynamoDB Streams → Kinesis Data Firehose → S3 for append-only run history. CloudTrail data events for S3 and DynamoDB.
