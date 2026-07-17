// Package rundaemon carries the shellkit-runner binaries inside the shellkit
// daemon and (in later units) bootstraps them onto a remote host (U5) and
// speaks the runner protocol as a client (U6a).
//
// # Embedded runners
//
// The daemon self-contains four cross-compiled shellkit-runner binaries
// (decision #13: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64),
// gzip-compressed and embedded via [runnersFS]. `just build-runners` (see the
// repo justfile) cross-compiles the runner static (CGO_ENABLED=0, -trimpath),
// stamps each with the content-hash version (ad-hoc codesigns darwin/arm64 per
// decision #18), gzips them into runners/, and regenerates version_gen.go so
// rundaemon.RunnerVersion matches the runner's stamped main.version — the
// bootstrap version-sync invariant. The four .gz files (~6MB total) are real,
// bootable binaries; [RunnerGz] returns the blob for a host's GOOS/GOARCH.
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
