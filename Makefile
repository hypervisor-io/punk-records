BIN := bin/punk
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: build test lint run clean version

build:
	go build $(LDFLAGS) -o $(BIN) ./cmd/punk

version:
	@echo $(VERSION)

test:
	go test ./...

test-ui:
	node --test internal/api/ui/brain-core.test.mjs
	node --check internal/api/ui/brain.js

lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

run: build
	$(BIN) serve

clean:
	rm -rf bin

demo:
	./scripts/demo.sh

brain-demo:
	./scripts/brain-demo.sh
