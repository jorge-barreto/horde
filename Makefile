.PHONY: build install test vet

build:
	go build ./cmd/horde

install:
	go install ./cmd/horde

test:
	go test ./...

vet:
	go vet ./...
