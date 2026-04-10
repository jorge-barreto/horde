.PHONY: build test vet

build:
	go build ./cmd/horde

test:
	go test ./...

vet:
	go vet ./...
