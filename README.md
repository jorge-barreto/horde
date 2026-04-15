# horde

Cloud launcher for [orc](https://github.com/jorge-barreto/orc) workflows. Runs orc on ephemeral Docker containers — spin up, clone, run, collect, tear down.

orc handles the "what" (workflow phases), horde handles the "where" (infrastructure). Any repo with an `.orc/` directory is deployable. Git is a hard requirement — horde infers the repo URL from the local git remote.

See [SPEC.md](SPEC.md) for the full design.

## Prerequisites

- Go (1.24+)
- Docker

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

## Usage

All commands require a provider. For local Docker mode, pass `--provider docker`. Omit the flag when the `@horde/cdk` construct is deployed (auto-detects via SSM).

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
go test ./...                                      # unit tests (fast, no Docker)
go test -v -count=1 -timeout 10m ./test/integration/  # integration tests (requires Docker)
```

Unit tests use fake Docker shell scripts — no real containers. Integration tests launch real Docker containers running real orc with script-only workflows against a real SQLite store. They exercise the full status detection chain: launch, timeout, kill, and external stop scenarios.

Integration tests take ~1-2 minutes. First run is slower (~3-4 minutes) because it builds the worker Docker image.

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
