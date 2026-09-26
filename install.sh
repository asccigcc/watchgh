#!/usr/bin/env bash
#
# watchgh installer. Downloads the prebuilt wgh binary from the latest GitHub
# release and installs it on your PATH. No Go toolchain required.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/install.sh | bash
#
# Override the install location or release tag with env vars:
#   BINDIR=/opt/homebrew/bin WGH_TAG=v0.7.0 bash install.sh
#
# The curl one-liner works anonymously against the public releases. If the GitHub
# CLI (gh) is installed it's used instead, which also works behind auth and rate
# limits. To upgrade an existing install later, use update.sh.
set -euo pipefail

REPO="${WGH_REPO:-asccigcc/watchgh}"
TAG="${WGH_TAG:-latest}"
BINDIR="${BINDIR:-/usr/local/bin}"

bold() { printf '\033[1m%s\033[0m\n' "$1"; }
info() { printf '  %s\n' "$1"; }
warn() { printf '\033[33m  ⚠ %s\033[0m\n' "$1"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- preflight ------------------------------------------------------------
[ "$(uname -s)" = "Darwin" ] || die "watchgh is macOS-only."
case "$(uname -m)" in
  arm64)  ASSET="wgh-darwin-arm64" ;;
  x86_64) ASSET="wgh-darwin-amd64" ;;
  *)      die "unsupported architecture: $(uname -m)" ;;
esac

# --- download -------------------------------------------------------------
TMP="$(mktemp -d "${TMPDIR:-/tmp}/watchgh.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

bold "Downloading $ASSET ($TAG)…"
if have gh; then
  # gh works with private repos (uses your auth). Empty tag = latest release.
  gh release download "${TAG#latest}" --repo "$REPO" --pattern "$ASSET" --dir "$TMP" \
    || die "download failed — is there a release with asset $ASSET on $REPO?"
else
  # Anonymous fallback: only works once the repo (and release) is public.
  url="https://github.com/$REPO/releases/latest/download/$ASSET"
  [ "$TAG" != "latest" ] && url="https://github.com/$REPO/releases/download/$TAG/$ASSET"
  curl -fSL --progress-bar "$url" -o "$TMP/$ASSET" \
    || die "download failed — the repo may be private (install gh) or the tag may not exist."
fi

# --- install --------------------------------------------------------------
# Elevate only when $BINDIR isn't writable by us. Testing -w (not the exit of
# `mkdir -p`) is the crux: mkdir -p on an existing-but-unwritable dir like
# /usr/local/bin succeeds, so gating on it would skip sudo and then `install`
# would die with Permission denied.
bold "Installing → $BINDIR/wgh"
if [ ! -d "$BINDIR" ]; then
  mkdir -p "$BINDIR" 2>/dev/null || sudo mkdir -p "$BINDIR" || die "could not create $BINDIR"
fi
if [ -w "$BINDIR" ]; then
  install -m 0755 "$TMP/$ASSET" "$BINDIR/wgh"
else
  info "$BINDIR needs elevated permissions; using sudo"
  sudo install -m 0755 "$TMP/$ASSET" "$BINDIR/wgh"
fi
info "installed $BINDIR/wgh"

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
info "wgh        # open the timeline; first run installs the background poller"
info "wgh stop   # stop the background poller"
