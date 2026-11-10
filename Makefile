.PHONY: build test lint vet clean compose-up compose-down

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE ?= $(shell command -v docker-compose 2>/dev/null || echo "docker compose")

build:
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/agent ./cmd/agent

test:
	go test ./... -v -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -rf bin/

compose-up:
	$(COMPOSE) up -d --build

compose-down:
	$(COMPOSE) down