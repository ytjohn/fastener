# Contributing

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
messages and PR titles:

```
<type>(<optional scope>): <summary>
```

Common types: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `ci`, `chore`.
Scopes are package or area names, e.g. `fix(parser): …`, `feat(cli): …`.
Pull requests are squash-merged, so the PR title becomes the commit on `main`.

Reference the issue in the PR body (`Fixes #N`).

## Checks

Before opening a PR:

```bash
go vet ./...
go test -race ./...
GOOS=darwin GOARCH=arm64 go vet ./...
gofmt -l .        # must print nothing
```

Tests that call real agent CLIs are opt-in (`FASTENER_SMOKE=1`) because they
spend model calls.
