# horde

Cloud launcher for [orc](https://github.com/jorge-barreto/orc) workflows. Runs orc on ephemeral cloud instances — spin up, clone, run, collect, tear down.

See [SPEC.md](SPEC.md) for the full design and [ROADMAP.md](ROADMAP.md) for the implementation plan.

## Setup

### Prerequisites

- Go (1.24+)
- Docker

### Install

```bash
make install
```

### Set up secrets

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

### Usage

```bash
horde launch <ticket>          # auto-builds worker image on first run
horde logs <run-id> --follow
horde status <run-id>
horde results <run-id>
horde list --all
horde kill <run-id>
horde resume <run-id>          # retry from failed phase
```

The first `horde launch` builds the `horde-worker:latest` Docker image automatically. Subsequent launches rebuild only when the embedded Dockerfile changes (after `make install`).

## License

MIT
