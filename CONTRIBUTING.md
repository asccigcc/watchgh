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

## Signed commits

`main` requires signed commits, so your PR's commits must be signed or it can't
be merged. If you haven't set this up, GitHub has a short guide, and the gist is:

```sh
git config --global commit.gpgsign true   # sign every commit
# then add a GPG or SSH signing key to your account
```

An unsigned commit shows "Unverified" on GitHub; a signed one shows "Verified".

## The checks

CI runs the same gauntlet I run by hand, and it must pass. Run it locally
before pushing with:

```sh
make ci
```

That's exactly what CI runs (gofmt, cgo-free build, vet, test, and a race
pass), so a green `make ci` means a green CI — the most common reason a PR
stalls is skipping it. watchgh is macOS-only, so `make ci` on a Mac is the
local equivalent of the CI run; there's no Linux/`act` shortcut, because the
code won't build off Darwin.

## Style

Match the surrounding code: comment the *why*, not the *what*; prefer small,
composable functions; add a test for behavior you change. When in doubt, read a
neighboring file and follow its lead.
