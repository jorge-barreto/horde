.PHONY: build install test vet worker docker-build check

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
