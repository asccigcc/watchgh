# watchgh

A GitHub activity timeline for your terminal: the PRs, reviews, and CI runs that
need you, in the order they arrive. `wgh` shows the timeline in your terminal,
polls GitHub in the background while it's open, and posts desktop notifications
for actionable events. Auth piggybacks on your credentials (`GITHUB_TOKEN` if
set, otherwise `gh auth token`).

## Install

macOS only. The installer downloads the prebuilt `wgh` binary from the latest
GitHub release and drops it on your PATH. No Go toolchain required.

```sh
curl -fsSL https://raw.githubusercontent.com/asccigcc/watchgh/main/install.sh | bash
```

It installs to `/usr/local/bin/wgh` by default; set `BINDIR` to override, or
`WGH_TAG` to pin a release:

```sh
BINDIR=/opt/homebrew/bin WGH_TAG=v0.1.0 bash install.sh
```

## Commands

```sh
wgh          # interactive timeline (prints plain text when piped)
wgh open 42  # open item 42 in the browser, mark read (here + GitHub)
wgh read 42  # mark item 42 read without opening
```

## Interactive timeline

Bare `wgh` in a terminal opens a full-screen timeline; piped or redirected, it
prints plain text so scripts keep working. It polls GitHub in the background on
the poll-floor cadence (and on launch, and on `R`), writing to a local store it
re-reads every couple of seconds so new events appear live. A `⟳ syncing` marker
in the title bar shows when a poll is in flight. Four tabs, with live counts:

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

## Notifications

While `wgh` is open it posts a desktop notification for each freshly-arrived
actionable event (review requested, assigned, changes requested, CI failed). The
launch sync is silent so you aren't alerted about the existing backlog, and a
manual `R` is silent too since you're already looking. CI and blocked events come
from a GraphQL poll of your open and assigned PRs, firing only on state
transitions.

## Configuration

Optional `config.toml` next to the store
(`~/Library/Application Support/watchgh/config.toml`). Flat `key = value`; every
key is optional, and a malformed file falls back to defaults. See
[`config.example.toml`](config.example.toml) for the full list.

The poll floor sets the background poll cadence; it defaults to `5m` (minimum
`60s`). Desktop notifications pull you in and `R` forces an on-demand refresh, so
tighter polling rarely pays for the extra API traffic. State lives in a SQLite DB
under
`~/Library/Application Support/watchgh/watchgh.db`; read items are pruned after 7
days, unread items never.

## License

MIT. See [LICENSE](LICENSE).
