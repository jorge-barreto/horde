.PHONY: build install test vet worker docker-build check e2e-up e2e-test e2e-down

build:
	go build ./cmd/horde

install:
	go install ./cmd/horde

test:
	go test ./...

vet:
	go vet ./...

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
#   e2e-up    Deploy the @horde/cdk stack, populate secrets, push the
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
