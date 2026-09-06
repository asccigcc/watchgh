# watchgit build targets.
#
# The core CLI/daemon (watchgit) is pure Go and stays cgo-free. The menu-bar
# viewer (watchgit-menu) uses menuet, which needs cgo AND must run from inside
# a .app bundle — so it is built and installed separately via menu-app / menu.

BINDIR ?= $(HOME)/go/bin
APPDIR ?= $(HOME)/Applications
APP    := $(APPDIR)/watchgit-menu.app

.PHONY: build install test menu-app menu menu-uninstall

# Build the pure-Go CLI/daemon binary in the working directory.
build:
	CGO_ENABLED=0 go build -o watchgit ./cmd/watchgit

# Install the CLI/daemon to $(BINDIR) (on PATH). The daemon runs this.
install:
	go install ./cmd/watchgit

test:
	go test ./...

# Assemble the menu-bar .app bundle. menuet crashes as a bare binary
# (bundleProxyForCurrentProcess is nil), so it must live in a bundle.
menu-app: install
	@rm -rf "$(APP)"
	@mkdir -p "$(APP)/Contents/MacOS"
	go build -o "$(APP)/Contents/MacOS/watchgit-menu" ./cmd/watchgit-menu
	@cp cmd/watchgit-menu/Info.plist "$(APP)/Contents/Info.plist"
	@# GUI apps launched via `open` don't inherit the shell PATH, so give the
	@# bundle a direct link to the CLI it delegates open/read actions to.
	@ln -sf "$(BINDIR)/watchgit" "$(APP)/Contents/MacOS/watchgit"
	@echo "built $(APP)"

# Build the bundle and launch it — look for the ◆ item in the menu bar.
menu: menu-app
	@open "$(APP)"
	@echo "watchgit-menu launched; look for ◆ in the menu bar."
	@echo "Use its 'Start at Login' item to keep it running across reboots."

menu-uninstall:
	@pkill -f "watchgit-menu" 2>/dev/null || true
	@rm -rf "$(APP)"
	@echo "removed $(APP)"
