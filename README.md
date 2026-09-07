# watchgh

A GitHub activity timeline for your terminal: the PRs, reviews, and CI runs that
need you, in the order they arrive. A background daemon polls GitHub and posts
desktop notifications; `wgh` shows the timeline in your terminal or the macOS
menu bar. Auth piggybacks on your credentials — `GITHUB_TOKEN` if set, otherwise
`gh auth token`.

## Install

macOS only. Builds from source (needs a Go toolchain and the Xcode command line
tools) and installs both the `wgh` CLI/daemon and the `wgh-menu` menu bar app:

```sh
curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/install.sh | bash
```

The CLI/daemon lands in `~/go/bin`, the app in `~/Applications`. Override the
paths, or skip the app on a headless box:

```sh
BINDIR=/usr/local/bin APPDIR=/Applications \
  curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/install.sh | bash
WGH_NO_MENU=1 bash install.sh   # CLI/daemon only
```

## Commands

```sh
wgh                  # interactive timeline (prints plain text when piped)
wgh open 42          # open item 42 in the browser, mark read (here + GitHub)
wgh read 42          # mark item 42 read without opening

wgh daemon install   # start the background poller via launchd (now + at login)
wgh daemon status    # is it running? where are the plist and log?
wgh daemon uninstall # stop and remove it

make menu            # build and launch the menu bar app
make menu-uninstall  # quit and remove it
```

## Interactive timeline

Bare `wgh` in a terminal opens a full-screen timeline; piped or redirected, it
prints plain text so scripts keep working. It's a pure viewer over the store
that the daemon fills, re-read every couple of seconds so new events appear live.
Four tabs, with live counts:

| Tab | Shows |
| --- | --- |
| 1 Inbox | unread items others put on you: review requests, assignments |
| 2 My PRs | every open PR you own, with its latest activity or CI/merge state |
| 3 Read | items you've already handled |
| 4 CI | build pass/fail and blocked/clean on your tracked PRs |

| Key | Action |
| --- | --- |
| 1-4, Tab | switch tab (Tab cycles) |
| ↑/↓, k/j | move the selection |
| g / G | jump to newest / oldest |
| PgUp/PgDn | page by a screenful |
| ⏎ | open the selected item in the browser + mark read |
| r | mark read without opening |
| R | poll GitHub now |
| q / Esc / Ctrl-C | quit |

## Background daemon

`wgh daemon install` writes a per-user LaunchAgent to
`~/Library/LaunchAgents/com.watchgh.plist` and loads it. Running inside your GUI
login session is what lets it post desktop notifications. It starts immediately,
restarts at login, respawns if it dies, and logs events to
`~/Library/Logs/watchgh.log`. It polls into the same SQLite store `wgh` reads, so
the timeline shows what the daemon has already collected without polling itself.

Notifications use conditional requests (a 304 costs no rate limit) and fire for
actionable events by default (review requested, assigned, changes requested, CI
failed). CI and blocked events come from a GraphQL poll of your open and assigned
PRs, firing only on state transitions.

## Menu bar

`wgh-menu` reads the same store and shows unread items with a count badge
(`◆ 3`); clicking a row opens it and marks it read. It never polls GitHub itself,
and delegates open/mark-read to the `wgh` CLI, so the GitHub-sync logic lives in
one place. It needs cgo and runs from a `.app` bundle, so it builds separately
via the `Makefile` into `~/Applications/wgh-menu.app`.

## Configuration

Optional `config.toml` next to the store
(`~/Library/Application Support/watchgh/config.toml`). Flat `key = value`; every
key is optional, and a malformed file falls back to defaults. See
[`config.example.toml`](config.example.toml) for the full list.

The poll floor defaults to `5m` (minimum `60s`): desktop notifications pull you
in and `R` forces an on-demand refresh, so tighter polling rarely pays for the
extra API traffic. State lives in a SQLite DB under
`~/Library/Application Support/watchgh/watchgh.db`; read items are pruned after 7
days, unread items never.

## License

MIT — see [LICENSE](LICENSE).
