.PHONY: build build-linux test run-server run-agent fmt

build:
	mkdir -p bin
	go build -o bin/signcloud-server ./cmd/server
	go build -o bin/signcloud-agent ./cmd/agent

build-linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$${GOARCH:-arm64} go build -trimpath -ldflags="-s -w" -o bin/signcloud-server-linux ./cmd/server

test:
	go test ./...

fmt:
	gofmt -w cmd internal

run-server:
	go run ./cmd/server

run-agent:
	go run ./cmd/agent
