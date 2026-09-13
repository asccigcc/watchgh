# Contributing

Thanks for your interest in watchgh! A quick note on how this project works so
we don't waste each other's time.

**This is a solo pet project.** I maintain it as time allows, so I review issues
and pull requests when I can — replies may be slow, and not everything will be
merged. That's not a reflection on your work; it's just the pace of a
one-person project. It's MIT-licensed, so you're always free to fork and take it
in your own direction.

## Before you open a PR

- **Open an issue first for anything non-trivial.** A quick "here's the bug /
  here's what I'd add — worth a PR?" saves you from building something I can't
  merge. Small, obvious fixes (typos, clear bugs) can skip this.
- **Keep it focused.** One change per PR. A tight, single-purpose diff gets
  reviewed and merged far faster than a sprawling one.
- **Explain the why.** What problem does this solve, and how did you verify it?

## The checks

CI runs the same gauntlet I run by hand, and it must pass:

```sh
gofmt -l .                                    # must print nothing
CGO_ENABLED=0 go build ./cmd/wgh/... ./internal/...
go vet ./...
go test ./...
go test -race ./...
```

Run it locally before pushing — a red CI is the most common reason a PR stalls.

## Style

Match the surrounding code: comment the *why*, not the *what*; prefer small,
composable functions; add a test for behavior you change. When in doubt, read a
neighboring file and follow its lead.
