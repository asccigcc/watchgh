# wgh build targets. The CLI (wgh) is pure Go and stays cgo-free.

BINDIR ?= $(HOME)/go/bin
DIST   ?= dist

.PHONY: build install test release

# Build the pure-Go binary in the working directory.
build:
	CGO_ENABLED=0 go build -o wgh ./cmd/wgh

# Cross-compile the release binaries (both macOS arches) plus a checksum file
# into $(DIST). The CLI is cgo-free, so this needs no C toolchain.
# Upload these as GitHub release assets; install.sh downloads them.
release:
	@rm -rf "$(DIST)" && mkdir -p "$(DIST)"
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$(DIST)/wgh-darwin-arm64" ./cmd/wgh
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o "$(DIST)/wgh-darwin-amd64" ./cmd/wgh
	cd "$(DIST)" && shasum -a 256 wgh-darwin-* > checksums.txt
	@echo "built release binaries in $(DIST)/"

# Install to $(BINDIR) (on PATH).
install:
	go install ./cmd/wgh

test:
	go test ./...
