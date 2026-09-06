# watchgit

A GitHub activity timeline for your terminal — the PRs, reviews, and (soon) CI
runs that need you, listed in the order they arrive.

## Status

All four build steps done. watchgit polls notifications and tracked-PR CI/merge
state into a persistent timeline, live or on demand.

- [x] **1. list** — `GET /notifications`, enriched, rendered as timeline rows
- [x] **2. store** — SQLite persistence, dedupe, read/unread, retention pruning
- [x] **3. watch** — poll loop (conditional GET, honors `X-Poll-Interval`) +
      macOS desktop notifications for actionable events
- [x] **4. tracked PRs** — GraphQL diff engine emitting CI-finished and
      blocked/unblocked events on state transitions (open + assigned PRs)

## Usage

```sh
go run ./cmd/watchgit list     # or just: watchgit — print the stored timeline
go run ./cmd/watchgit watch    # stream new events live + desktop notifications
go run ./cmd/watchgit open 42  # open item 42 in browser, mark read (here + GitHub)
go run ./cmd/watchgit read 42  # mark item 42 read without opening
```

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
GUTTER TIME  BADGE     REPO#NUM        AUTHOR    DETAIL
!      2d    ◆ review  api#412         @kai      review requested
```

- **Gutter** — `!` needs you and has gone stale (>24h), `▍` unread, blank read.
- **Badge** — colored by kind; red is reserved for CI failure.
- **REPO#NUM** — Cmd+click opens the PR (OSC 8; falls back to a raw URL).
- **AUTHOR** — the PR author; `—` when it's yours.
```
