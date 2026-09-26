#!/usr/bin/env bash
#
# watchgh updater. Upgrades an existing wgh install in place to the latest GitHub
# release (or a pinned tag), then restarts the background poller so it re-execs
# onto the new binary. No Go toolchain required.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/update.sh | bash
#
# Pin a tag or force a reinstall even when already current:
#   WGH_TAG=v0.7.0 bash update.sh
#   WGH_FORCE=1 bash update.sh
#
# For a first-time install (no wgh on PATH yet), use install.sh instead.
set -euo pipefail

REPO="${WGH_REPO:-asccigcc/watchgh}"
TAG="${WGH_TAG:-latest}"

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

# Locate the existing install so we replace it in place (BINDIR overrides the
# detected directory). An updater with nothing to update is a mistake worth
# naming, not a silent fresh install.
WGH="$(command -v wgh || true)"
[ -n "$WGH" ] || die "wgh isn't on your PATH — use install.sh for a first install."
BINDIR="${BINDIR:-$(cd "$(dirname "$WGH")" && pwd)}"

# --- resolve versions -----------------------------------------------------
# `wgh version` lands in v0.7.0; an older binary without it reports "unknown", so
# we just proceed with the update rather than trying to compare.
current="$(wgh version 2>/dev/null || echo unknown)"

resolve_latest() {
  if have gh; then
    gh release view --repo "$REPO" --json tagName -q .tagName 2>/dev/null && return
  fi
  # Anonymous: the /releases/latest redirect lands on /releases/tag/<tag>.
  local url
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null || true)"
  [ -n "$url" ] && printf '%s\n' "${url##*/}"
}

if [ "$TAG" = "latest" ]; then
  target="$(resolve_latest)"
  [ -n "$target" ] || die "couldn't resolve the latest release tag for $REPO."
else
  target="$TAG"
fi

if [ "${WGH_FORCE:-0}" != "1" ] && [ "$current" = "$target" ]; then
  bold "✓ wgh is already up to date ($current)"
  exit 0
fi
info "current: $current  →  target: $target"

# --- download -------------------------------------------------------------
TMP="$(mktemp -d "${TMPDIR:-/tmp}/watchgh.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

bold "Downloading $ASSET ($target)…"
if have gh; then
  gh release download "$target" --repo "$REPO" --pattern "$ASSET" --dir "$TMP" \
    || die "download failed — is there a release $target with asset $ASSET on $REPO?"
else
  curl -fSL --progress-bar "https://github.com/$REPO/releases/download/$target/$ASSET" -o "$TMP/$ASSET" \
    || die "download failed — does release $target exist on $REPO?"
fi

# --- install --------------------------------------------------------------
bold "Installing → $BINDIR/wgh"
if [ -w "$BINDIR" ]; then
  install -m 0755 "$TMP/$ASSET" "$BINDIR/wgh"
else
  info "$BINDIR needs elevated permissions; using sudo"
  sudo install -m 0755 "$TMP/$ASSET" "$BINDIR/wgh"
fi

# --- restart the poller ---------------------------------------------------
# Kickstart re-execs the launchd poller onto the freshly installed binary so the
# running background job picks up the new version without waiting for a reboot.
# Best-effort: if the poller isn't loaded, the next `wgh` launch installs it.
LABEL="com.watchgh.poller"
if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
  if launchctl kickstart -k "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
    info "restarted the background poller"
  else
    warn "couldn't restart the poller; it'll pick up the new binary on next launch"
  fi
fi

echo
bold "✓ wgh updated to $(wgh version 2>/dev/null || echo "$target")"
