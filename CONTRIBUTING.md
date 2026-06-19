# Contributing to shellkit

Thanks for your interest in improving shellkit.

## Development

shellkit is a single Go module. You need Go (see the version in `go.mod`) and,
at runtime, `nix` on `PATH` (inventory is loaded via `nix eval`).

```sh
go build -o shellkit ./cmd/shellkit   # build
just test                # run the test suite (go test ./...)
just vet                 # go vet ./...
just lint                # shellcheck + golangci-lint + nix flake check
just fmt                 # format Go, shell, and nix sources
```

A Nix flake provides a reproducible dev shell: `nix develop` (or `direnv allow`).
Entering the dev shell installs a fast pre-commit hook (`lefthook`) that runs
`gofmt` and `go vet` on staged changes — it finishes in seconds. Install it
manually with `just hooks`, or skip a one-off commit with `LEFTHOOK=0 git commit`.

## Pull requests

- Keep changes focused; one logical change per PR.
- Run `just test`, `just vet`, and `just lint` before opening a PR — CI runs the
  same checks.
- Add or update tests for behavior changes. The TUI log-dashboard has golden
  tests under `testdata/golden/`; regenerate expected output with
  `GOLDEN_UPDATE=1 go test -run TestGoldenUnifiedView .` when you intend to
  change rendering.
- Run `just fmt` to format (Go, shell, nix). The pre-commit hook and CI both
  enforce `gofmt` — no unformatted code.

## Inventory and test data

Never commit real infrastructure details (hostnames, IPs, keys, tokens). Test
fixtures use the IETF documentation ranges (RFC 5737/3849), RFC 1918 private
space, and synthetic names. See `examples/inventory.sample.nix`.

## Reporting bugs

Open an issue with the shellkit version (`shellkit version`), your OS, and steps
to reproduce. For security issues, see [SECURITY.md](SECURITY.md).
