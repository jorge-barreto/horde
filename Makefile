.PHONY: build install test vet worker

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
