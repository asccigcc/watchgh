# wgh build targets.
#
# The core CLI/daemon (wgh) is pure Go and stays cgo-free. The menu-bar
# viewer (wgh-menu) uses menuet, which needs cgo AND must run from inside
# a .app bundle — so it is built and installed separately via menu-app / menu.

BINDIR ?= $(HOME)/go/bin
APPDIR ?= $(HOME)/Applications
APP    := $(APPDIR)/wgh-menu.app

.PHONY: build install test icon menu-app menu menu-uninstall

# Build the pure-Go CLI/daemon binary in the working directory.
build:
	CGO_ENABLED=0 go build -o wgh ./cmd/wgh

# Install the CLI/daemon to $(BINDIR) (on PATH). The daemon runs this.
install:
	go install ./cmd/wgh

test:
	go test ./...

# Regenerate the menu-bar icon PNGs from an SF Symbol (needs the Swift toolchain).
# The generated PNGs are committed, so this is only needed when changing the icon.
icon:
	swift tools/genicon/genicon.swift eye cmd/wgh-menu/Resources

# Assemble the menu-bar .app bundle. menuet crashes as a bare binary
# (bundleProxyForCurrentProcess is nil), so it must live in a bundle.
menu-app: install
	@rm -rf "$(APP)"
	@mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	go build -o "$(APP)/Contents/MacOS/wgh-menu" ./cmd/wgh-menu
	@cp cmd/wgh-menu/Info.plist "$(APP)/Contents/Info.plist"
	@cp cmd/wgh-menu/Resources/*.png "$(APP)/Contents/Resources/"
	@# GUI apps launched via `open` don't inherit the shell PATH, so give the
	@# bundle a direct link to the CLI it delegates open/read actions to.
	@ln -sf "$(BINDIR)/wgh" "$(APP)/Contents/MacOS/wgh"
	@echo "built $(APP)"

# Build the bundle and launch it — look for the ◆ item in the menu bar.
menu: menu-app
	@open "$(APP)"
	@echo "wgh-menu launched; look for ◆ in the menu bar."
	@echo "Use its 'Start at Login' item to keep it running across reboots."

menu-uninstall:
	@pkill -f "wgh-menu" 2>/dev/null || true
	@rm -rf "$(APP)"
	@echo "removed $(APP)"
