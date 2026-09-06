# watchgit

A GitHub activity timeline for your terminal — the PRs, reviews, and (soon) CI
runs that need you, listed in the order they arrive.

## Status

watchgit polls notifications and tracked-PR CI/merge state into a persistent
timeline, live or on demand, and can run as an always-on background daemon.

- [x] **1. list** — `GET /notifications`, enriched, rendered as timeline rows
- [x] **2. store** — SQLite persistence, dedupe, read/unread, retention pruning
- [x] **3. watch** — poll loop (conditional GET, honors `X-Poll-Interval`) +
      macOS desktop notifications for actionable events
- [x] **4. tracked PRs** — GraphQL diff engine emitting CI-finished and
      blocked/unblocked events on state transitions (open + assigned PRs)
- [x] **5. daemon** — launchd LaunchAgent runs the poll loop continuously;
      notifications fire with no terminal open, and list/watch just read the store
- [x] **6. menu bar** — a macOS status-bar app (`watchgit-menu`) that reads the
      store, shows the timeline with an unread badge, and opens items via the CLI

## Usage

```sh
watchgit list             # or just: watchgit — print the stored timeline
watchgit watch            # stream new events live + desktop notifications
watchgit open 42          # open item 42 in browser, mark read (here + GitHub)
watchgit read 42          # mark item 42 read without opening

watchgit daemon install   # run the poller in the background via launchd (starts now + at login)
watchgit daemon status    # is it running? where are the plist and log?
watchgit daemon uninstall # stop and remove it
```

## Background daemon

`watchgit daemon install` writes a per-user **LaunchAgent** to
`~/Library/LaunchAgents/com.watchgit.plist` pointing at the installed binary and
loads it. A LaunchAgent (not a system LaunchDaemon) runs inside your GUI login
session — that's what lets it post desktop notifications. It starts immediately,
restarts at login, and `KeepAlive` respawns it if it dies. It writes a timestamped
event log to `~/Library/Logs/watchgit.log`.

Because the daemon polls into the same SQLite store, `list` just reads what the
daemon has already collected — no polling on your part. The store runs in WAL
mode so the CLI can read while the daemon writes. You can still run
`watchgit watch` alongside it; whichever process sees an event first records it,
so you won't get duplicate notifications.

## Menu-bar app

`watchgit-menu` is a macOS status-bar viewer built on
[menuet](https://github.com/caseymrm/menuet). It's a **pure viewer**: it reads
the same store the daemon fills (never polls GitHub itself) and delegates
open/mark-read to the `watchgit` CLI, so the GitHub-sync logic lives in one
place. The menu-bar title shows the unread count (`◆ 3`); the dropdown lists the
newest events with their colored badge and a NEW/DUE pill, and clicking a row
opens it (marking it read here and on GitHub).

The dropdown also shows the background daemon's health at the bottom — `Daemon:
running ✓` when the poller is alive, or `Daemon: not running` with a **Start
background poller** action when it isn't (so the menu never quietly shows a
stale timeline because nothing is polling). Stop/restart stay in the CLI; a dead
poller is the only daemon state worth acting on from a viewer.

```sh
make menu            # build the .app bundle and launch it (look for ◆)
make menu-uninstall  # quit and remove the bundle
```

menuet needs cgo and must run from inside a `.app` bundle, so — unlike the
pure-Go, cgo-free CLI/daemon — it's built and installed separately (via the
`Makefile`, into `~/Applications/watchgit-menu.app`). Toggle **Start at Login**
from the app's own menu to keep it running across reboots.

`watch` polls on GitHub's requested interval using conditional requests (a
304 "nothing changed" costs no rate limit), streams only newly-arrived events,
and fires a desktop notification for **actionable** ones only (review requested,
assigned, changes requested, CI failed). Notifications open the PR on click when
[`terminal-notifier`](https://github.com/julienXX/terminal-notifier) is
installed; otherwise it falls back to `osascript`.

CI and blocked events come from a separate GraphQL poll of your open and
assigned PRs, diffing each PR's check-rollup and merge state against the last
value seen (stored in `pr_state`). Events fire only on transitions — a first
sighting seeds state silently, drafts are skipped, and a failed build is
"actionable" only when it's your own PR.

State lives in a SQLite DB under your OS config dir
(`~/Library/Application Support/watchgit/watchgit.db` on macOS). Read/resolved
items are pruned after 7 days; unread items are never auto-removed.

Auth piggybacks on your existing credentials: `GITHUB_TOKEN` if set, otherwise
`gh auth token`.

## Reading a row

```
SEQ GUT TIME  BADGE     REPO#NUM        AUTHOR    DETAIL
  7 DUE   2d  ◆ review  api#412         @kai      review requested
```

- **SEQ** — stable local number for `watchgit open <n>` / `read <n>`.
- **Gutter** — `DUE` needs you and has gone stale (>24h), `NEW` unread, blank read.
- **Badge** — colored by kind; red is reserved for CI failure.
- **REPO#NUM** — Cmd+click opens the PR (OSC 8; falls back to a raw URL).
- **AUTHOR** — the PR author; `—` when it's yours.
```
