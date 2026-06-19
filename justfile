# shellkit — task runner
# Run `just` (or `just --list`) to see available recipes.

# Show available recipes.
default:
    @just --list

# Format Go (gofmt), shell (shfmt), and nix (flake formatter) sources.
fmt:
    #!/usr/bin/env bash
    set -euo pipefail
    gofiles=$(git ls-files --cached --others --exclude-standard '*.go')
    if [ -n "$gofiles" ]; then
        printf '%s\n' "$gofiles" | xargs gofmt -w
    else
        echo "gofmt: no Go files to format"
    fi
    files=$(git ls-files --cached --others --exclude-standard -z | xargs -0 shfmt -f 2>/dev/null || true)
    if [ -n "$files" ]; then
        printf '%s\n' "$files" | xargs shfmt -w
    else
        echo "shfmt: no shell files to format"
    fi
    nix fmt

# Lint Go (go vet + golangci-lint), shell (shellcheck), and validate the flake.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    go vet ./...
    if command -v golangci-lint >/dev/null 2>&1; then
        golangci-lint run
    else
        echo "golangci-lint: not installed, skipping (CI runs it)"
    fi
    files=$(git ls-files --cached --others --exclude-standard -z | xargs -0 shfmt -f 2>/dev/null || true)
    if [ -n "$files" ]; then
        printf '%s\n' "$files" | xargs shellcheck
    else
        echo "shellcheck: no shell files to lint"
    fi
    nix flake check

# Validate the flake.
check:
    nix flake check

# Build the shellkit binary.
build:
    go build -o shellkit ./cmd/shellkit

# Run the Go test suite. Pass extra args, e.g. `just test -run TestFoo`.
test *args:
    go test {{args}} ./...

# Vet the Go sources.
vet:
    go vet ./...

# Install the lefthook git hooks (the dev shell does this automatically on entry).
hooks:
    lefthook install
