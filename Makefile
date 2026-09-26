# wgh build targets. The CLI (wgh) is pure Go and stays cgo-free.

BINDIR ?= $(HOME)/go/bin
DIST   ?= dist

# VERSION stamps the binary (wgh version). It comes from the current git tag, so a
# build at v0.7.0 reports "v0.7.0" and an untagged working build reports something
# like "v0.7.0-3-g1a2b3c4"; a tree with no tags/git falls back to "dev". Override
# on the command line for a release build: make release VERSION=v0.7.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test release ci

# Build the pure-Go binary in the working directory.
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o wgh ./cmd/wgh

# Cross-compile the release binaries (both macOS arches) plus a checksum file
# into $(DIST). The CLI is cgo-free, so this needs no C toolchain.
# Upload these as GitHub release assets; install.sh downloads them.
release:
	@rm -rf "$(DIST)" && mkdir -p "$(DIST)"
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o "$(DIST)/wgh-darwin-arm64" ./cmd/wgh
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o "$(DIST)/wgh-darwin-amd64" ./cmd/wgh
	cd "$(DIST)" && shasum -a 256 wgh-darwin-* > checksums.txt
	@echo "built release binaries ($(VERSION)) in $(DIST)/"

# Install to $(BINDIR) (on PATH).
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/wgh

test:
	go test ./...

# The full pre-commit / CI gauntlet. Run this locally before pushing; CI runs
# the exact same target, so a green `make ci` here means a green CI. macOS only
# (the CLI is macOS-only), so there's no cross-platform runner to reproduce —
# this is the local equivalent of the GitHub Actions run.
ci:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	CGO_ENABLED=0 go build ./cmd/wgh/... ./internal/...
	go vet ./...
	go test ./...
	go test -race ./...
