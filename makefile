.PHONY: vendor mocks test build run

vendor:
	go mod tidy && go mod vendor

mocks:
	mockery --all

test:
	go test ./...

build:
	go build -o bin/laker ./cmd/laker
	go build -o bin/laker-agent ./cmd/laker-agent

run: build
	./bin/laker serve
