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
watchgit                  # interactive full-screen timeline (falls back to list when piped)
watchgit list             # print the stored timeline and exit
watchgit watch            # stream new events live + desktop notifications
watchgit open 42          # open item 42 in browser, mark read (here + GitHub)
watchgit read 42          # mark item 42 read without opening

watchgit daemon install   # run the poller in the background via launchd (starts now + at login)
watchgit daemon status    # is it running? where are the plist and log?
watchgit daemon uninstall # stop and remove it
```

## Interactive timeline

Running `watchgit` with no arguments (attached to a terminal) opens a full-screen
timeline on the alternate screen — the terminal counterpart to the menu-bar app:

It's organized into four tabs (the counts update live):

| Tab | Shows |
| --- | ----- |
| **1 · Inbox** (default) | unread items others put on you — review requests + assignments |
| **2 · Read** | items you've already handled — excludes your own PRs |
| **3 · Mine** | every open PR you own — one row each, even silent ones — showing its latest activity or CI/merge state |
| **4 · CI** | build pass/fail + blocked/clean on your tracked PRs |

| Key            | Action                                        |
| -------------- | --------------------------------------------- |
| `1`–`4`, `Tab` | switch tab (`Tab` cycles)                     |
| `↑`/`↓`, `k`/`j` | move the selection                          |
| `g` / `G`      | jump to the newest / oldest row               |
| `PgUp`/`PgDn`  | page by a screenful                           |
| `⏎`            | open the selected item in the browser + mark read |
| `r`            | mark the selected item read without opening   |
| `R`            | poll GitHub now (a manual refresh)            |
| `q` / `Esc` / `Ctrl-C` | quit                                  |

It's a pure viewer over the store — the daemon (or a one-shot sync at launch)
fills it — and it re-reads every couple of seconds, so events the daemon collects
appear live. Piped or redirected (`watchgit | less`), it prints like `list` so
scripts keep working.

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
**unread** events (newest first) with their colored badge and a NEW/DUE pill —
an inbox of what still needs you, not a history — and clicking a row opens it
(marking it read here and on GitHub, which drops it from the dropdown). The full
timeline, read items included, stays in `watchgit list`.

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

## Configuration

Thresholds are tunable via an optional `config.toml` in the same directory as
the store (`~/Library/Application Support/watchgit/config.toml` on macOS). It's
a flat `key = value` file; every key is optional and falls back to a default,
and a malformed file just logs a warning and uses defaults. See
[`config.example.toml`](config.example.toml) for the full list — the DUE
staleness window, retention period, poll floor, menu row cap, and whether to
notify on actionable events only. Durations take Go units plus a day unit
(`"7d"`).

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
