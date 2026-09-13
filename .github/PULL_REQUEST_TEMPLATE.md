<!-- Thanks for contributing! Please keep PRs small and focused — one change per PR. -->

## What & why

<!-- What does this change, and what problem does it solve? Link the issue if there is one (e.g. Fixes #12). -->

## How I tested it

<!-- Commands you ran, steps you followed, what you saw. -->

## Checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `CGO_ENABLED=0 go build ./cmd/wgh/... ./internal/...` passes
- [ ] `go vet ./...` passes
- [ ] `go test ./...` and `go test -race ./...` pass
- [ ] This PR does one thing
