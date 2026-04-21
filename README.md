# horde

Cloud launcher for [orc](https://github.com/jorge-barreto/orc) workflows. Runs orc on ephemeral Docker containers — spin up, clone, run, collect, tear down.

orc handles the "what" (workflow phases), horde handles the "where" (infrastructure). Any repo with an `.orc/` directory is deployable. Git is a hard requirement — horde infers the repo URL from the local git remote.

See [SPEC.md](SPEC.md) for the full design.

## Prerequisites

- Go (1.24+)
- Docker — required for local mode and for `horde push` (which builds the worker image)
- AWS account with credentials configured (only for the v0.2 AWS path) — either [Path A](#path-a--cloudformation-bootstrap-no-cdk-required) or [Path B](#path-b--cdk-construct-teams-with-an-existing-cdk-app) below
- Node.js 18+ (only for Path B, which uses CDK)

## Install

```bash
make install
```

## Setup

```bash
cp .env.example .env
```

Edit `.env` and fill in:

- **`CLAUDE_CODE_OAUTH_TOKEN`** — run `claude setup-token` to generate a long-lived OAuth token for the Claude CLI.
- **`GIT_TOKEN`** — a GitHub fine-grained personal access token. Recommended permissions:

  | Permission       | Access     | Used for                              |
  |------------------|------------|---------------------------------------|
  | Contents         | Read/Write | Clone repos, push to `horde/` branches |
  | Metadata         | Read       | Required for all fine-grained PATs    |
  | Pull requests    | Read/Write | Open PRs via `gh pr create`           |
  | Issues           | Read       | Read ticket/issue context             |
  | Workflows        | Read/Write | Push changes to `.github/workflows/`  |

Run `horde docs env` for more detail on token setup and security.

## Quickstart: AWS (v0.2)

Stand up the cloud backend once, then every `horde launch` from the project runs on ECS Fargate. Two provisioning paths — pick one.

### Path A — CloudFormation bootstrap (no CDK required)

```bash
horde bootstrap init                  # writes .horde/cloudformation.yaml
horde bootstrap deploy                # prompts for CLAUDE_CODE_OAUTH_TOKEN + GIT_TOKEN, ~5 min cold
horde push                            # build + upload worker image to ECR
```

In CI/headless contexts, set `CLAUDE_CODE_OAUTH_TOKEN` and `GIT_TOKEN` as environment variables instead of answering prompts. Attach the managed policy the stack exports (`horde-<slug>-cli-policy-arn`) to the IAM user or role that will run `horde launch`. Full flow: `horde docs bootstrap`.

### Path B — CDK construct (teams with an existing CDK app)

Use [`examples/cdk-consumer/`](examples/cdk-consumer/) as a starting point: copy the directory into a new CDK app (or merge `app.ts` into your existing stack), edit the `SLUG` constant to match your git remote (`owner-repo`, lowercased, non-alphanumerics → `-`), then:

```bash
npm install                           # pulls @horde.io/cdk from npmjs
npm run deploy                        # ~5 min cold

# Populate the empty secrets the stack created:
aws secretsmanager put-secret-value --secret-id horde-<slug>-claude-code-oauth-token --secret-string "$CLAUDE_CODE_OAUTH_TOKEN"
aws secretsmanager put-secret-value --secret-id horde-<slug>-git-token              --secret-string "$GIT_TOKEN"

# From your project directory:
horde push                            # build + upload worker image
```

Attach the `CliUserManagedPolicyArn` output to the IAM user or role that will run `horde launch`. Full flow: `horde docs cdk`.

### Launch

Once the stack is up and the image is pushed, from any repo whose git remote matches the slug:

```bash
horde launch PROJ-123                 # provider auto-detects from SSM
horde status PROJ-123
horde status PROJ-123 --json
```

Example `horde status --json` output:

```json
{
  "id": "a1b2c3d4e5f6",
  "ticket": "PROJ-123",
  "workflow": "default",
  "branch": "horde/PROJ-123",
  "status": "running",
  "instance_id": "arn:aws:ecs:us-east-1:123456789012:task/horde-acme-widgets/abc123",
  "duration_seconds": 42.7,
  "launched_by": "you@example.com",
  "started_at": "2026-04-20T14:02:17Z"
}
```

### Teardown

```bash
# Path A:
horde bootstrap destroy

# Path B (from the CDK app directory):
npm run destroy
```

## Usage

All commands require a provider. For local Docker mode, pass `--provider docker`. Omit the flag when an AWS stack (provisioned via `horde bootstrap` or the [`@horde.io/cdk`](cdk/) construct) is deployed — horde auto-detects via SSM.

```bash
# Launch a run
horde launch --provider docker PROJ-123
horde launch --provider docker PROJ-123 --branch feature/xyz
horde launch --provider docker PROJ-123 --workflow bugfix --timeout 30m

# Monitor
horde status <run-id>
horde logs <run-id> --follow
horde list                        # active runs
horde list --all                  # include completed/failed/killed
horde status <run-id> --json      # machine-readable JSON output
horde list --all --json           # JSON output for scripting

# Results
horde results <run-id>

# Stop, retry, inspect
horde kill <run-id>
horde retry <run-id>              # restart container — orc picks up where it left off
horde shell <run-id>              # interactive shell into the container
horde clean [run-id]              # remove stopped containers

# Documentation
horde docs                        # list topics
horde docs <topic>                # read a topic
```

The first `horde launch` builds the worker Docker image automatically. Subsequent launches reuse the cached image unless the Dockerfile changes.

## Worker Image

horde uses a two-layer Docker image system:

- **`horde-worker-base:latest`** — built from files embedded in the horde binary (orc, claude CLI, bd, git, gh, AWS CLI). Rebuilds when horde is upgraded.
- **`horde-worker:latest`** — built from `worker/Dockerfile` if present (extends the base with project-specific tools), or tagged from base otherwise.

Both are built automatically on launch. Run `horde docs worker-image` for details.

## Project Config

Optional project-level settings in `.horde/config.yaml`:

```yaml
mounts:
  - .beads:/workspace/.beads
```

Mounts use Docker's `host:container` format. Host paths are relative to the project root. Run `horde docs config` for the full schema.

## Testing

```bash
make unit-test         # fast Go tests, no Docker, no AWS. What CI runs.
make integration-test  # real Docker, real horde, real orc (~2 min). Also CI.
make e2e-up            # developer-local only: deploy AWS stack (not CI)
make e2e-test          # run full TestECS_* suite against deployed stack
make e2e-down          # always run when done with e2e
```

Unit tests use fake Docker shell scripts — no real containers. Docker integration tests launch real Docker containers running real orc with script-only workflows against a real SQLite store. They exercise the full status detection chain: launch, timeout, kill, and external stop scenarios, and take ~1-2 minutes.

ECS integration tests drive a real CloudFormation-deployed AWS stack end-to-end: launch, status, logs (CloudWatch), kill, list, hydrate (S3), concurrent runs, and timeout reconciliation. Gated by `HORDE_E2E_ECS=1` in `.env`; set `HORDE_E2E_ECS_KEEP=1` to reuse the bootstrap stack between runs. Full suite runs in ~2 minutes parallel. See `horde docs ecs-integration` for setup and cost notes.

## Run Data

All local data lives under `~/.horde/`:

```
~/.horde/
  horde.db                  # SQLite run history
  results/<run-id>/         # artifacts, audit logs, saved container logs
  workerfiles/              # synced Docker build context
```

## License

MIT
