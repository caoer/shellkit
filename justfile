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

# Cross-compile shellkit-runner for the 4 embedded targets (decision #13:
# linux/amd64, linux/arm64, darwin/amd64, darwin/arm64), gzip each into
# internal/rundaemon/runners/, replacing the placeholder .gz files committed
# there by the U7 scaffold. Static build (CGO_ENABLED=0) — required on NixOS
# targets, which have no /lib64/ld-linux for a dynamically linked binary.
# Version is a content hash of the runner source tree, so a source change
# always produces a new version and bootstrap knows to re-push.
#
# Needs cmd/shellkit-runner (U3a/U3b) to exist and build; until that lands
# this recipe is correct but `go build` below fails — that's expected for
# the U7 scaffold, not a bug in the recipe. U7-final is the first run that
# produces real, bootable binaries.
build-runners:
    #!/usr/bin/env bash
    set -euo pipefail
    runner_src=(cmd/shellkit-runner internal/runnerproto internal/interp)
    version=$(git ls-files -- "${runner_src[@]}" | LC_ALL=C sort | xargs -r sha256sum | sha256sum | cut -c1-12)
    out_dir=internal/rundaemon/runners
    mkdir -p "$out_dir"
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
        goos=${target%/*}
        goarch=${target#*/}
        work=$(mktemp -d)
        trap 'rm -rf "$work"' EXIT
        bin="$work/shellkit-runner"
        echo "build-runners: $target (version $version)"
        CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
            -trimpath \
            -ldflags "-s -w -X main.version=$version" \
            -o "$bin" \
            ./cmd/shellkit-runner
        if [ "$goos/$goarch" = "darwin/arm64" ]; then
            # Ad-hoc sign: unsigned cross-compiled arm64 Mach-O binaries are
            # SIGKILLed by AMFI on first exec on real Apple Silicon hardware
            # (decision #18). Ad-hoc signing needs no Apple account/identity.
            codesign -s - "$bin"
        fi
        gzip -n -9 -c "$bin" >"$out_dir/runner-${goos}-${goarch}.gz"
        rm -rf "$work"
        trap - EXIT
    done
    echo "build-runners: wrote $out_dir/runner-{linux,darwin}-{amd64,arm64}.gz (version $version)"

# Run the Go test suite. Pass extra args, e.g. `just test -run TestFoo`.
test *args:
    go test {{args}} ./...

# Vet the Go sources.
vet:
    go vet ./...

# Install the lefthook git hooks (the dev shell does this automatically on entry).
hooks:
    lefthook install
