BIN     := bin/dwt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFILES := cmd internal

.PHONY: build check test install install-agent

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/dwt

check:
	@out="$$(gofmt -l $(GOFILES))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go test -race ./...

test:
	go test -race ./...

install:
	@echo "install: implemented in Task 13"; exit 1

install-agent:
	@echo "install-agent: implemented in Task 13"; exit 1
