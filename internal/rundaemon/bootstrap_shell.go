package rundaemon

// bootstrap_shell.go — the POSIX-sh command strings the bootstrap issues over
// the ssh exec channel, plus their output parsers. Every command is pure
// `/bin/sh` (no bashisms) and depends only on mkdir/cat/chmod/mv/printf/uname
// plus one of sha256sum|shasum — so a bash-3.2 or sh-only host bootstraps
// identically (requirements: zero dependence on remote shell version).
//
// Injection surface: the only value interpolated into a command that is not
// daemon-constrained is the candidate root expression. Built-in candidates are
// constants; the per-host `runner_tmp` override is operator-supplied inventory
// data (trusted, same trust class as an ssh target). Platform tokens come from
// uname mapped through a strict whitelist (parseUname), and the digest is hex —
// neither can carry shell metacharacters back into a later command.

import (
	"strings"

	"github.com/caoer/shellkit/internal/inventory"
)

// Distinct exit codes let the daemon tell a digest mismatch (re-pushable) from a
// plain filesystem failure. Kept out of the 0–125 range shells use for real
// program exits to avoid collision with a runner's own status.
const exitDigestMismatch = 34

// candidate is one cache_root option: a shell expression (expanded remote-side)
// and an optional guard that skips it when a required variable is unset.
type candidate struct {
	expr  string // shell expression for the root dir, e.g. `$HOME/.cache/shellkit`
	guard string // sh test that must pass, or "" for none
}

// candidateRoots is the ordered cache_root chain (Mitogen pattern, decision #15):
// an optional per-host `runner_tmp` override first, then
// ~/.cache → $XDG_CACHE_HOME → /var/tmp → /dev/shm. The override, when set, is
// highest priority but still falls through to the defaults if it is itself
// unusable — bootstrap never hard-fails on a single candidate.
func candidateRoots(srv *inventory.Server) []candidate {
	var cands []candidate
	if tmp := strings.TrimSpace(srv.RunnerTmp); tmp != "" {
		cands = append(cands, candidate{expr: tmp})
	}
	return append(cands,
		candidate{expr: `$HOME/.cache/shellkit`},
		candidate{expr: `${XDG_CACHE_HOME}/shellkit`, guard: `[ -n "$XDG_CACHE_HOME" ]`},
		candidate{expr: `/var/tmp/shellkit`},
		candidate{expr: `/dev/shm/shellkit`},
	)
}

// smokeTestCmd builds the write+chmod+exec probe for one candidate. It creates
// the dir with a private mode (umask 077 → 0700), writes a trivial script,
// chmod +x it, and EXECUTES it — the exec is what detects a noexec mount (chmod
// succeeds, exec returns 126). On success it prints the host platform
// (uname -s / -m) so the caller maps it to goos/goarch in the same round trip.
// Distinct exit codes tag which step failed.
//
// Security hardening (shared candidate roots /var/tmp, /dev/shm): a pre-existing
// cache dir is only accepted when it is a real directory (not a symlink) owned by
// the ssh user. A symlinked or foreign-owned dir at the predictable path is a
// plant vector — reject it and fall through to the next candidate rather than
// writing (and later execing) inside an attacker-controlled directory. Own dirs
// are (re)tightened to 0700 so a lax pre-existing mode can't leave the runner
// world-writable.
func smokeTestCmd(c candidate) string {
	guard := c.guard
	if guard == "" {
		guard = "true"
	}
	// $$ keeps concurrent probes on the same host from colliding on the tmp name.
	return strings.Join([]string{
		guard + ` || exit 10`,
		`d="` + c.expr + `"`,
		`[ -h "$d" ] && exit 15`, // reject a symlinked cache root (plant vector)
		`(umask 077; mkdir -p "$d") 2>/dev/null || exit 11`,
		`[ -d "$d" ] && [ ! -h "$d" ] || exit 15`, // must be a real dir, not a symlink
		`[ -O "$d" ] || exit 16`,                  // must be owned by the ssh user
		`chmod 700 "$d" 2>/dev/null || exit 17`,   // tighten a lax pre-existing mode
		`p="$d/.shellkit-probe.$$"`,
		`printf '#!/bin/sh\nexit 0\n' > "$p" 2>/dev/null || exit 12`,
		`chmod +x "$p" 2>/dev/null || exit 13`,
		`"$p" || exit 14`,
		`rm -f "$p"`,
		`printf 'SHELLKIT_UNAME=%s %s\n' "$(uname -s)" "$(uname -m)"`,
		`echo SHELLKIT_ROOT_OK`,
	}, "; ")
}

// versionCmd runs `<path> --version`, exiting early if the path is not an
// executable file so a missing binary reads as a clean miss, not a shell error.
func versionCmd(path string) string {
	return `[ -x "` + path + `" ] || exit 40; "` + path + `" --version 2>/dev/null`
}

// cachedDigestCmd computes the remote sha256 of the cached binary at path and
// prints it as a `SHELLKIT_DIGEST=<hex>` line (parsed by parseDigest), so the
// daemon can compare it to its own digest before trusting a warm-hit binary
// (security: a version-string match is forgeable; a byte-exact digest is not).
// A missing/unreadable path, or the absence of both sha256 tools, prints no
// digest (or an empty one) — read by the daemon as a miss, forcing a re-push.
// It must be a regular file, not a symlink: a symlinked cache path could redirect
// the digest read (and later the exec) to attacker-controlled bytes.
func cachedDigestCmd(path string) string {
	return strings.Join([]string{
		`p="` + path + `"`,
		`[ -f "$p" ] || exit 41`,
		`[ -h "$p" ] && exit 42`, // reject a symlinked cache entry
		`got=$(sha256sum "$p" 2>/dev/null | cut -d" " -f1)`,
		`[ -n "$got" ] || got=$(shasum -a 256 "$p" 2>/dev/null | cut -d" " -f1)`,
		`printf 'SHELLKIT_DIGEST=%s\n' "$got"`,
	}, "; ")
}

// pushCmd cats the runner bytes (on stdin) to a hidden dotname tmp under root,
// chmod +x, verifies the remote sha256 equals want BEFORE committing, then
// atomically `mv -f` to final. The tmp is hidden (dot-prefixed, vs tmp-cleanup
// daemons) and `$$`-suffixed (concurrent-push safe); the mv is atomic so a
// concurrent push of identical content is a harmless overwrite. sha256sum
// (coreutils) is tried first, shasum -a 256 (macOS) second.
func pushCmd(rootExpr, final, want string) string {
	return strings.Join([]string{
		`root="` + rootExpr + `"`,
		`final="` + final + `"`,
		`want="` + want + `"`,
		`tmp="$root/.shellkit-runner.$want.$$.tmp"`,
		`mkdir -p "$root" 2>/dev/null || exit 30`,
		`cat > "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 31; }`,
		`chmod +x "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 32; }`,
		`got=$(sha256sum "$tmp" 2>/dev/null | cut -d" " -f1)`,
		`[ -n "$got" ] || got=$(shasum -a 256 "$tmp" 2>/dev/null | cut -d" " -f1)`,
		`printf 'SHELLKIT_DIGEST=%s\n' "$got"`,
		`[ "$got" = "$want" ] || { rm -f "$tmp"; exit 34; }`,
		`mv -f "$tmp" "$final" 2>/dev/null || { rm -f "$tmp"; exit 35; }`,
		`echo SHELLKIT_PUSH_OK`,
	}, "; ")
}

// parseUname reads the raw `<sysname> <machine>` tokens from a
// `SHELLKIT_UNAME=<sysname> <machine>` line. ok is false when the line is absent
// or malformed. Other output lines (MOTD / banner noise) are ignored — the
// tagged line is located, not assumed first. Mapping to a Go platform is a
// separate step (mapUname) so "dir works, arch unmappable" stays distinct from
// "no usable dir".
func parseUname(out string) (sysname, machine string, ok bool) {
	line, found := taggedLine(out, "SHELLKIT_UNAME=")
	if !found {
		return "", "", false
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// mapUname maps raw uname tokens to Go's GOOS/GOARCH spelling. Unrecognized
// systems or machines yield ok=false so the caller falls back rather than
// guessing an arch and pushing the wrong binary.
func mapUname(sysname, machine string) (goos, goarch string, ok bool) {
	switch sysname {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", false
	}
	switch machine {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", false
	}
	return goos, goarch, true
}

// parseDigest reads the remote-computed sha256 from a `SHELLKIT_DIGEST=<hex>`
// line, or "" if absent (e.g. the push aborted before the digest step).
func parseDigest(out string) string {
	line, found := taggedLine(out, "SHELLKIT_DIGEST=")
	if !found {
		return ""
	}
	return strings.TrimSpace(line)
}

// taggedLine returns the text after prefix on the first line carrying it,
// tolerating leading MOTD / banner noise on the exec channel.
func taggedLine(out, prefix string) (string, bool) {
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, prefix); i >= 0 {
			return strings.TrimRight(ln[i+len(prefix):], "\r"), true
		}
	}
	return "", false
}
