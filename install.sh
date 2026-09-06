#!/usr/bin/env bash
#
# watchgh installer — builds and installs both pieces from source:
#   1. the wgh CLI/daemon    -> $BINDIR (default ~/go/bin)
#   2. the wgh-menu.app      -> $APPDIR (default ~/Applications), then launches it
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/install.sh | bash
#
# The menu bar app uses cgo, so a Go toolchain and the Xcode command line tools
# are required. Override install locations or the source ref with env vars:
#   BINDIR=/usr/local/bin APPDIR=/Applications WGH_REF=v1.2.3 bash install.sh
#   WGH_NO_MENU=1   bash install.sh   # CLI/daemon only, skip the menu app
set -euo pipefail

REPO="${WGH_REPO:-https://github.com/asccigcc/watchgh.git}"
REF="${WGH_REF:-main}"
BINDIR="${BINDIR:-$HOME/go/bin}"
APPDIR="${APPDIR:-$HOME/Applications}"
MIN_GO="1.27"

bold() { printf '\033[1m%s\033[0m\n' "$1"; }
info() { printf '  %s\n' "$1"; }
warn() { printf '\033[33m  ⚠ %s\033[0m\n' "$1"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- preflight ------------------------------------------------------------
[ "$(uname -s)" = "Darwin" ] || die "watchgh is macOS-only (needs launchd + the menu bar)."
have git || die "git is required. Install the Xcode command line tools: xcode-select --install"
have go  || die "Go is required. Install it from https://go.dev/dl or: brew install go"

# Go must be new enough to build the module (go.mod pins $MIN_GO).
gover="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [ -n "$gover" ] && [ "$(printf '%s\n%s\n' "$MIN_GO" "$gover" | sort -V | head -1)" != "$MIN_GO" ]; then
  die "Go $gover is too old; watchgh needs $MIN_GO or newer."
fi

# The menu app needs a C compiler (cgo). CLI/daemon is cgo-free, so this is only
# fatal when we're building the menu.
if [ -z "${WGH_NO_MENU:-}" ] && ! have cc && ! have clang; then
  die "The menu app needs a C compiler. Run: xcode-select --install (or set WGH_NO_MENU=1)"
fi

# --- fetch source ---------------------------------------------------------
# Build in place when run from a checkout; otherwise clone shallowly to a temp
# dir we clean up on exit.
SRC=""
if [ -f "./go.mod" ] && head -1 ./go.mod | grep -q '^module watchgh'; then
  SRC="$(pwd)"
else
  SRC="$(mktemp -d "${TMPDIR:-/tmp}/watchgh.XXXXXX")"
  trap 'rm -rf "$SRC"' EXIT
  bold "Fetching watchgh ($REF)…"
  git clone --quiet --depth 1 --branch "$REF" "$REPO" "$SRC" \
    || die "clone failed — is $REF a valid branch/tag on $REPO?"
fi

# --- build & install ------------------------------------------------------
bold "Installing the CLI/daemon → $BINDIR"
make -C "$SRC" install BINDIR="$BINDIR" >/dev/null
info "installed $BINDIR/wgh"

if [ -z "${WGH_NO_MENU:-}" ]; then
  bold "Building the menu bar app → $APPDIR"
  make -C "$SRC" menu-app BINDIR="$BINDIR" APPDIR="$APPDIR" >/dev/null
  open "$APPDIR/wgh-menu.app"
  info "launched wgh-menu — look for ◆ in the menu bar"
  info "use its 'Start at Login' item to keep it running across reboots"
fi

# --- guidance -------------------------------------------------------------
echo
bold "✓ watchgh installed"

case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) warn "$BINDIR is not on your PATH — add it, e.g.: echo 'export PATH=\"$BINDIR:\$PATH\"' >> ~/.zshrc" ;;
esac

if have gh; then
  gh auth status >/dev/null 2>&1 || warn "GitHub CLI isn't authenticated — run: gh auth login"
else
  warn "No GitHub token source found — install gh (brew install gh) or export GITHUB_TOKEN."
fi

echo
info "wgh                  # open the interactive timeline"
info "wgh daemon install   # run the background poller (desktop notifications)"
