# watchgh

A GitHub activity timeline for your terminal: the PRs, reviews, and CI runs that
need you, in the order they arrive. `wgh` shows the timeline in your terminal
and installs a launchd background poller that keeps GitHub in sync and posts
desktop notifications for actionable events, even when no window is open. Auth
piggybacks on your credentials (`GITHUB_TOKEN` if set, otherwise
`gh auth token`).

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
wgh stop     # stop the background poller (restarts next time you open wgh)
```

## Interactive timeline

Bare `wgh` in a terminal opens a full-screen timeline; piped or redirected, it
prints plain text so scripts keep working. It re-reads the local store every
couple of seconds so events the background poller writes appear live, and
refreshes on launch and on `R`. A `⟳ syncing` marker in the title bar shows a
sync in flight; the footer shows whether the background poller is on. (If the
poller isn't running, the open window polls GitHub itself.) Four tabs, with live
counts:

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

## Background poller

The first time you open `wgh` it installs a launchd agent
(`com.watchgh.poller`) that runs `wgh` headless in the background. launchd keeps
it alive across logins and reboots, so it polls GitHub on the poll-floor cadence
and posts desktop notifications for actionable events (review requested,
assigned, changes requested, CI failed) even when no window is open. Its first
pass is silent so a cold start doesn't alert on the whole backlog; after that
only genuinely new events notify. CI and blocked events come from a GraphQL poll
of your open and assigned PRs, firing only on state transitions.

```sh
wgh stop   # unload the poller and remove its agent
wgh        # opening wgh again re-installs and starts it
```

Notification delivery uses `terminal-notifier` if installed (click to open the
item), otherwise the built-in `osascript`. Because the poller runs under
launchd — which starts with a minimal environment — it resolves your token via
`gh auth token`; make sure the GitHub CLI is authenticated (`gh auth login`).
Its output goes to `~/Library/Application Support/watchgh/poller.log`.

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
