VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/llm-shape-proxy ./cmd/llm-shape-proxy

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

check: lint test
