.PHONY: test build

test:
	go test -race ./...
	go vet ./...

build:
	go build -o bin/matching-service ./cmd/server
