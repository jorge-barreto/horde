.PHONY: build install unit-test integration-test bootstrap-validate-test vet worker docker-build check e2e-up e2e-test e2e-down

# Version metadata embedded via -ldflags. `version` falls back to the short
# git describe (tag + offset + SHA) so dev builds are self-identifying;
# release builds get the clean tag from goreleaser. `commit` and `buildDate`
# are always from git / UTC now.
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

build:
	go build -ldflags '$(LDFLAGS)' ./cmd/horde

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/horde

# Fast tier: no Docker, no AWS. Must stay green on every commit.
# Excludes cdk/ because it's a Node package; its node_modules/ contains
# scaffold files with %name% placeholders that Go's ./... walker cannot
# parse ("invalid input file name"). Listing our own packages explicitly
# keeps CI green regardless of what npm installs underneath cdk/.
unit-test:
	go test -short ./cmd/... ./internal/... ./test/...

# Docker tier: real containers, real horde binary, real orc. ~2 min.
# Preflight docker on PATH so a missing daemon doesn't silently pass
# via testing.Short()-style skips inside the suite.
#
# Unsets HORDE_E2E_AWS / HORDE_E2E_ECS so this target stays Docker-only
# regardless of what .env has. AWS-backed tests live under e2e-* and
# (for CloudFormation template validation) bootstrap-validate-test.
integration-test:
	@command -v docker >/dev/null 2>&1 || { \
	  echo 'integration-test requires docker on PATH; install Docker or use make unit-test' >&2; \
	  exit 1; \
	}
	@HORDE_E2E_AWS=0 HORDE_E2E_ECS=0 HORDE_E2E_CDK=0 \
	  go test -count=1 -timeout 10m ./test/integration/

# Free AWS-backed check: CloudFormation ValidateTemplate on the rendered
# bootstrap template. Creates no resources, no account charges. Requires
# a live SSO token and HORDE_E2E_AWS=1 (set in .env locally; never in CI).
bootstrap-validate-test:
	@$(E2E_ENV) && export HORDE_E2E_AWS=1 && \
	  go test -v -count=1 -timeout 1m -run TestBootstrap_ValidateTemplate ./test/integration/

vet:
	go vet ./cmd/... ./internal/... ./test/...

worker:
	docker build -t horde-worker:latest docker/

# Validate the worker Dockerfile builds cleanly. Distinct from `worker`
# so CI can run it under a throwaway tag without touching a user's
# horde-worker:latest image.
docker-build:
	docker build -t horde-worker:ci-check docker/

# Static checks that don't require Docker or network. Run before pushing.
check: vet
	bash -n docker/entrypoint.sh
	bash -n docker/git-askpass.sh
	bash docker/entrypoint_test.sh

# -------- CDK e2e verification sweep (real AWS; costs money) --------
#
# Run in order: `make e2e-up` -> `make e2e-test` -> `make e2e-down`.
# Reads AWS creds from .env. Skips if HORDE_E2E_CDK != 1, so setting the
# env var is the only gesture of intent.
#
#   e2e-up    Deploy the @horde.io/cdk stack, populate secrets, push the
#             worker image. ~5 min cold, ~2 min warm.
#   e2e-test  Run the full TestECS_* suite plus TestECSCDK_Smoke against
#             the deployed stack. ~15-20 min.
#   e2e-down  Destroy the stack. ~3 min. ALWAYS run this when done.
#
E2E_ENV = set -a && . ./.env && set +a && export HORDE_E2E_CDK=1

e2e-up:
	@$(E2E_ENV) && go test -v -count=1 -timeout 20m -run TestECSCDK_Bringup ./test/integration/

e2e-test:
	@$(E2E_ENV) && export HORDE_E2E_ECS=1 HORDE_E2E_ECS_BACKEND=cdk && \
	    go test -v -count=1 -timeout 30m \
	    -run 'TestECS' \
	    -skip 'TestECSSmoke|TestECSCDK_Bringup|TestECSCDK_Teardown' \
	    ./test/integration/

e2e-down:
	@$(E2E_ENV) && go test -v -count=1 -timeout 15m -run TestECSCDK_Teardown ./test/integration/
