# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# norca-loadbalancer
GO      ?= go
BINARY  := lbprovider
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build build-all test test-race lint vuln fmt tidy-check clean help

all: fmt lint test build

## build: Build bin/lbprovider for this machine
build:
	$(GO) build $(LDFLAGS) -o bin/$(BINARY) ./cmd/lbprovider

## build-all: Cross-build for the supported platforms
build-all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-linux-amd64 ./cmd/lbprovider
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-linux-arm64 ./cmd/lbprovider
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-darwin-arm64 ./cmd/lbprovider
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build $(LDFLAGS) -o bin/$(BINARY)-windows-amd64.exe ./cmd/lbprovider

## test: Run the tests
test:
	$(GO) test -count=1 ./...

## test-race: Run the tests with the race detector (needs cgo; CI runs it on Linux)
test-race:
	$(GO) test -race -shuffle=on -count=1 ./...

## lint: Run golangci-lint (the code base is clean under .golangci.yml; CI fails on a finding)
lint:
	golangci-lint run ./...

## vuln: Scan the module and the standard library for known vulnerabilities
vuln:
	govulncheck ./...

## fmt: Format and vet
fmt:
	$(GO) fmt ./...
	$(GO) vet ./...

## tidy-check: Fail when go.mod or go.sum is not tidy
tidy-check:
	$(GO) mod tidy
	git diff --exit-code go.mod go.sum

## clean: Remove build output
clean:
	rm -rf bin/ coverage.out

## help: Show this help
help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':'

