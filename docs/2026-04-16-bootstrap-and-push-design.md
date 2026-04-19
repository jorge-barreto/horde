# horde bootstrap + horde push — Design

> **Note (2026-04-19):** This doc is the as-designed snapshot. A few details
> diverged during implementation (horde-jqd):
> - The Claude secret is `CLAUDE_CODE_OAUTH_TOKEN` (Claude CLI OAuth token),
>   not `ANTHROPIC_API_KEY`. The CloudFormation parameter is
>   `ClaudeCodeOauthToken`; the Secrets Manager resource is
>   `horde-<slug>-claude-code-oauth-token`.
> - ECR repo uses `EmptyOnDelete: true` so `horde bootstrap destroy` works
>   after `horde push`.
> - `horde bootstrap destroy` pre-empties the S3 artifacts bucket before
>   calling `DeleteStack` (CloudFormation can't delete non-empty buckets).
> The implementation is the source of truth; see `horde docs bootstrap`.

## Problem

The ECS provider (epic 10) needs backing AWS infrastructure: ECS cluster, DynamoDB table, S3 bucket, IAM roles, etc. The CDK construct (epic 12) serves projects that already have CDK in their stack. Projects that don't — including horde itself — need a self-contained way to provision everything from scratch.

## Solution

A `horde bootstrap` command that generates and deploys a CloudFormation template, plus a `horde push` command that builds and pushes the worker image to ECR. Together they take a project from zero AWS infrastructure to running cloud workers.

The CloudFormation template is written to disk (`.horde/cloudformation.yaml`) so it can be inspected, committed, and hand-edited. The CDK construct becomes the graduation path for teams that want composable IaC.

## Commands

### `horde bootstrap init`

Generates `.horde/cloudformation.yaml` from the current git remote.

- Derives a project slug from the remote (e.g., `github.com/jorge-barreto/horde.git` → `jorge-barreto-horde`)
- All resource names and the SSM path (`/horde/{slug}/config`) use this slug
- Prints a summary of what the template will create
- Idempotent: refuses if the file exists unless `--regenerate` is passed

### `horde bootstrap deploy`

Deploys the template from disk.

- Reads `.horde/cloudformation.yaml`; errors if missing ("run `horde bootstrap init` first")
- Prompts interactively for `ANTHROPIC_API_KEY` and `GIT_TOKEN` via masked stdin input
- Headless fallback: reads `ANTHROPIC_API_KEY` and `GIT_TOKEN` from environment variables
- Passes secrets as CloudFormation parameters (`NoEcho: true`) → Secrets Manager resources
- Creates the stack, or updates if it already exists
- Streams stack events to show progress
- On success, prints the SSM parameter path and "next step: run `horde push`"

### `horde bootstrap destroy`

Tears down the CloudFormation stack.

- Queries DynamoDB for active runs; refuses if any exist
- Confirmation prompt: "This will destroy all horde infrastructure for {slug}. Type the stack name to confirm:"
- Deletes the stack, waits for completion

### `horde push`

Builds and pushes the project worker image to ECR.

- Reads SSM parameter at `/horde/{slug}/config` to discover ECR repo URI
- Pulls the public base image from GHCR (`ghcr.io/jorge-barreto/horde-worker-base:latest`)
- If `worker/Dockerfile` exists, builds the project image (`FROM` the base)
- If no `worker/Dockerfile`, tags the base image directly
- Authenticates to ECR via `aws ecr get-login-password`
- Tags and pushes with `:latest`
- Prints success + image digest

Requires Docker running locally (already a given for Docker-provider users).

## End-to-End User Journey

```
horde bootstrap init          # generates .horde/cloudformation.yaml
horde bootstrap deploy        # prompts for secrets, creates stack
horde push                    # builds + pushes worker image to ECR
horde launch --provider aws-ecs -t TICKET-123
horde logs -f abc123def456
horde status abc123def456
horde results abc123def456
```

## CloudFormation Resources

All resources named/tagged with the project slug. ~30 resources total.

### Networking

- VPC (10.0.0.0/16)
- 2 public subnets + 2 private subnets across 2 AZs
- Internet Gateway + NAT Gateway (one public subnet)
- Route tables: public → IGW, private → NAT

### Compute

- ECS Cluster (Fargate)
- ECS Task Definition: references ECR image with `:latest` tag, CPU 1024 / memory 4096. Injects `ANTHROPIC_API_KEY` and `GIT_TOKEN` via Secrets Manager `valueFrom`. Environment variables `REPO_URL`, `TICKET`, `BRANCH`, `WORKFLOW`, `RUN_ID`, `ARTIFACTS_BUCKET` are set as overrides at launch time, not baked in.
- ECR Repository for the project worker image

### Storage

- DynamoDB table (`horde-runs-{slug}`): pay-per-request, partition key `id` (S), 4 GSIs:
  - `by-repo`: partition `repo` (S), sort `started_at` (S)
  - `by-ticket`: partition `ticket` (S), sort `started_at` (S)
  - `by-status`: partition `status` (S), sort `started_at` (S)
  - `by-instance`: partition `instance_id` (S), no sort key
- S3 bucket (`horde-artifacts-{slug}-{account-id}`): secure-transport policy
- Secrets Manager secrets (2): `ANTHROPIC_API_KEY`, `GIT_TOKEN`, populated via `NoEcho` stack parameters

### Observability

- CloudWatch Log Group (`/ecs/horde-worker-{slug}`)

### Status Sync

- EventBridge Rule: filters ECS task state changes on the horde cluster
- Lambda function (inline Python via `ZipFile`): receives event, queries DynamoDB `by-instance` GSI, updates run record
- Lambda IAM Role: scoped to DynamoDB table, S3 bucket (read), CloudWatch Logs (write)

### Security and IAM

- Security Group: egress-only (all outbound, no inbound)
- IAM Task Role: S3 `PutObject` on artifacts prefix, Secrets Manager `GetSecretValue` on the two secrets
- IAM Execution Role: ECR pull, CloudWatch Logs `CreateLogStream`/`PutLogEvents`
- IAM CLI User Managed Policy: ECS `RunTask`/`DescribeTasks`/`StopTask`, SSM `GetParameter`, DynamoDB CRUD, CloudWatch Logs `GetLogEvents`, S3 `GetObject`. Exported as stack output.

### Config

- SSM Parameter (`/horde/{slug}/config`): JSON blob containing all ARNs/names the CLI needs:

```json
{
  "cluster_arn": "arn:aws:ecs:...:cluster/horde-{slug}",
  "task_definition_arn": "arn:aws:ecs:...:task-definition/horde-{slug}:N",
  "subnets": ["subnet-abc", "subnet-def"],
  "security_group": "sg-123",
  "log_group": "/ecs/horde-worker-{slug}",
  "artifacts_bucket": "horde-artifacts-{slug}-{account-id}",
  "runs_table": "horde-runs-{slug}",
  "ecr_repo_uri": "{account-id}.dkr.ecr.{region}.amazonaws.com/horde-{slug}",
  "max_concurrent": 5,
  "default_timeout_minutes": 1440
}
```

## Status Lambda Logic

Inline Python, ~60 lines, deployed via CloudFormation `ZipFile`:

1. Triggered by EventBridge for ECS task state change (`STOPPED` only)
2. Extract task ARN, exit code, stopped reason from event
3. Query DynamoDB `by-instance` GSI with task ARN → find run ID
4. If no matching run, log and return (non-horde task)
5. Read `run-result.json` from S3 at `horde-runs/{run-id}/run-result.json` for `total_cost_usd`. Missing file is not an error (crashed runs).
6. Update DynamoDB: `status` → success/failed (based on exit code), `exit_code`, `completed_at`, `total_cost_usd`
7. Idempotent: if record is already terminal, skip

The CLI does NOT lazy-check ECS on every `status`/`list` call. DynamoDB is the source of truth. The only exception: if a run is past its `timeout_at` and still marked "running", the CLI calls `DescribeTasks` to reconcile. This keeps CLI performance fast.

## Project Slug Derivation

- Parse git remote URL via existing `config.NormalizeRepoURL`
- Extract `{owner}/{repo}`, strip `.git` suffix
- Sanitize for CloudFormation: lowercase, replace `/` and non-alphanumeric with hyphens, truncate to keep stack name under 128 chars
- Example: `github.com/jorge-barreto/horde.git` → `jorge-barreto-horde`

## Template Generation

- Go `text/template` rendering a YAML CloudFormation template
- Template source embedded in Go binary via `//go:embed` in `internal/bootstrap/`
- Variables substituted: slug, SSM path, resource names
- Secrets are CloudFormation `Parameters` with `NoEcho: true` — never in the rendered YAML on disk

## Code Layout

- `internal/bootstrap/`: template generation, stack deploy/destroy, ECR push logic
- `cmd/horde/`: `bootstrap.go` (init/deploy/destroy subcommands), `push.go`

## Version Control

`.horde/cloudformation.yaml` should be committed. Secrets are stack parameters, not in the file.

## Worker Image Strategy

- **Base image** (`horde-worker-base`): published to GHCR by the horde maintainer. Contains orc, Claude CLI, git, standard tools. Versioned on release.
- **Project image**: built by `horde push`, extends the base via `worker/Dockerfile`. Stored in the project's ECR repository.

## Epic Dependencies

- Depends on epics 7 + 8 (AWS SDK foundation + SSM config discovery)
- ECS provider (epic 10) depends on this indirectly — needs infrastructure to talk to
- CDK construct (epic 12) is independent — the advanced/composable path
- Slots in parallel to epic 10 or between 10 and 11

## Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Networking | Full VPC created | Bootstrap is the "I have nothing" path; CDK is for existing infra |
| Secrets input | Stdin prompts + env var fallback | No secrets in argv (process list visibility); env vars for CI |
| Base image hosting | Public GHCR | Maintained by horde, shared by all users; no per-project rebuild |
| Template storage | `.horde/cloudformation.yaml` on disk | Inspectable, editable, committable |
| init vs deploy | Separate subcommands | Supports the edit-then-deploy workflow |
| Stack naming | Derived from git remote | Automatic, deterministic, multi-project safe |
| Status sync | Inline Lambda via CloudFormation ZipFile | Avoids packaged Lambda complexity; keeps template hand-editable |
| CLI performance | Trust Lambda, DynamoDB is source of truth | No lazy ECS polling on status/list; only reconcile past timeout |
| Queuing | Not in scope | Launch rejects at max_concurrent; user retries manually |
