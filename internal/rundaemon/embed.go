// Package rundaemon carries the shellkit-runner binaries inside the shellkit
// daemon and (in later units) bootstraps them onto a remote host (U5) and
// speaks the runner protocol as a client (U6a).
//
// # Embedded runners (U7 scaffold — placeholders)
//
// The daemon self-contains four cross-compiled shellkit-runner binaries
// (decision #13: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64),
// gzip-compressed and embedded via [runnersFS]. The four .gz files committed
// today under runners/ are PLACEHOLDERS — a gzip stream wrapping a sentinel
// text, not a runnable binary — so that the //go:embed directive below
// compiles and every package that imports rundaemon can build before
// cmd/shellkit-runner exists (U3a/U3b build it).
//
// `just build-runners` (see the repo justfile) overwrites these placeholders
// with real cross-compiled binaries once the runner source is complete;
// U7-final wires that recipe's output into this directory for real. Until
// that assembly happens, [RunnerGz] returns placeholder bytes — callers must
// not assume a successful [RunnerGz] call means a bootable binary.
package rundaemon

import (
	"embed"
	"fmt"
)

// runnersFS embeds the gzip-compressed shellkit-runner binaries built by
// `just build-runners`, one per supported GOOS/GOARCH pair. See the package
// doc for the placeholder-vs-final distinction.
//
//go:embed runners/*.gz
var runnersFS embed.FS

// RunnerGz returns the gzip-compressed shellkit-runner binary embedded for
// the given goos/goarch pair, spelled the way Go itself spells them
// (runtime.GOOS / runtime.GOARCH, e.g. "linux"/"amd64"). Daemon bootstrap
// (U5) uses this to pick the blob matching a remote host's uname before
// pushing it over the exec channel.
//
// The four supported pairs are decision #13's cross-compile matrix:
// linux/amd64, linux/arm64, darwin/amd64, darwin/arm64. Any other pair
// returns an error naming it — never a nil slice masquerading as "no
// runner needed".
//
// PLACEHOLDER (U7 scaffold): until `just build-runners` runs against a
// complete cmd/shellkit-runner (U3a/U3b), the bytes returned here decompress
// to a sentinel string, not an executable. Bootstrap code built against this
// accessor is correct; the binaries it pushes are not runnable yet.
func RunnerGz(goos, goarch string) ([]byte, error) {
	name := runnerAssetName(goos, goarch)
	if name == "" {
		return nil, fmt.Errorf("rundaemon: no embedded runner for %s/%s", goos, goarch)
	}
	b, err := runnersFS.ReadFile("runners/" + name)
	if err != nil {
		return nil, fmt.Errorf("rundaemon: embedded runner %s: %w", name, err)
	}
	return b, nil
}

// runnerAssetName returns the embedded filename for goos/goarch, or "" if the
// pair is not one of decision #13's four cross-compile targets.
func runnerAssetName(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
		return fmt.Sprintf("runner-%s-%s.gz", goos, goarch)
	default:
		return ""
	}
}
