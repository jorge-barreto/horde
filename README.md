# horde

Cloud launcher for [orc](https://github.com/jorge-barreto/orc) workflows. Runs orc on ephemeral cloud instances — spin up, clone, run, collect, tear down.

See [SPEC.md](SPEC.md) for the full design and [ROADMAP.md](ROADMAP.md) for the implementation plan.

## Setup

### Install

```bash
make install
```

### Set up secrets

```bash
cp .env.example .env
# Edit .env — fill in ANTHROPIC_API_KEY and GIT_TOKEN
```

`GIT_TOKEN` needs read access to the repo you want to run orc against.

### Launch

```bash
horde launch <ticket>       # auto-builds worker image on first run
horde logs <run-id> --follow
horde status <run-id>
horde results <run-id>
horde list --all
horde kill <run-id>
```

The first `horde launch` builds the `horde-worker:latest` Docker image automatically. Subsequent launches rebuild only when the embedded Dockerfile changes (after `make install`). To build the image manually: `make worker`.

### Prerequisites

- Go (1.22+)
- Docker

## License

MIT
