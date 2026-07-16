package rundaemon

// bootstrap.go — daemon-side runner bootstrap + push (plan U5, §4).
//
// Bootstrap decides, per host, whether a usable shellkit-runner binary is
// already present and, if not, pushes the embedded binary over the SAME ssh
// exec channel the executor uses (riding sshconn.ResolveInvocation — never
// building ssh args, never re-resolving an address: the PortFor regression
// guard, plan decision #2). It is failure-soft by contract: every failure mode
// (noexec/read-only home, unknown arch, transport error, digest/version
// mismatch) yields a fallback verdict so the executor drops to the legacy path.
// It NEVER step-fails and NEVER leaves a silent partial.
//
// Flow (decisions #3, #15; security #1):
//
//	resolveRoot   candidate cache_root chain (Mitogen pattern): for each candidate
//	              mkdir + write + chmod +x + exec a tiny probe BEFORE committing —
//	              this is how noexec / read-only home are detected. The winning
//	              expression is cached per host; uname rides the same command.
//	probe         <root>/runner-<ver>-<os>-<arch> --version — a version match is a
//	              warm HIT, return the path with no push.
//	push          on miss: decompress the embedded gz daemon-side and cat the raw
//	              bytes in-band to a hidden dotname tmp, chmod +x, verify the
//	              daemon-computed sha256 against the remote file, then atomic
//	              `mv -f` to the hash-named immutable final path.
//	exec-test     <final> --version confirms the pushed binary runs and matches.
//
// A digest or version mismatch is a DISTINCT logged diagnostic (security #5, not
// a cosmetic trace note) → re-push ONCE → still bad ⇒ fallback.
//
// NOTE: RunnerGz now returns real cross-compiled binaries (U7-final) that answer
// `--version` with RunnerVersion, so probe/exec-test match against real bytes.
// The full push/exec-test round-trip over a live ssh channel is still validated
// end-to-end in U8/U9.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/sshconn"
)

// RunnerVersion is the content-hash version of the embedded runner binaries —
// the same hash the runner reports from `--version` (its main.version). It names
// the immutable cache path (runner-<ver>-<os>-<arch>) and gates the probe
// HIT/miss decision. The two MUST agree or bootstrap always sees a miss/mismatch
// and never uses the pushed runner; that lockstep is the version-sync invariant.
//
// `just build-runners` writes both in one pass: it stamps the runner via
// `-ldflags "-X main.version=<hash>"` and regenerates the committed
// version_gen.go, whose init() overrides this default with the same <hash>. So a
// plain `go build ./cmd/shellkit` picks up the matching version with no special
// ldflags. Stays "dev" only if build-runners has never run (version_gen.go absent).
var RunnerVersion = "dev"

// Result is the bootstrap verdict handed back to the executor (U6b). Exactly one
// posture: a usable RunnerPath (Fallback false), or Fallback true with a Reason
// naming the cause so U6b can render the per-host `note:` line (contract §5.3).
// Bootstrap NEVER step-fails, so there is no error return — a fallback IS the
// failure signal.
type Result struct {
	// RunnerPath is the absolute remote path to the verified runner binary;
	// empty when Fallback is true.
	RunnerPath string
	// Fallback is true when the executor must drop to the legacy path.
	Fallback bool
	// Reason is the diagnostic cause; empty on success, one explicit line on
	// fallback (rendered as `note:` by U6b).
	Reason string
}

// execResult is the outcome of one remote command over the ssh exec channel.
// A non-nil Err is a transport failure (ssh could not run at all); a non-zero
// ExitCode with a nil Err is the remote command's own exit status.
type execResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Err      error
}

// ok reports a clean remote run (transport up, remote command exit 0).
func (r execResult) ok() bool { return r.Err == nil && r.ExitCode == 0 }

// sshExec runs one remote shell command over the ssh exec channel, feeding
// stdin (nil for none) and capturing stdout/stderr. The production
// implementation rides sshconn.ResolveInvocation so auth / jump / port /
// password are inherited identically and NO address is ever re-resolved; tests
// inject a fake, the way client_test.go fakes the runner.
type sshExec interface {
	run(ctx context.Context, srv *inventory.Server, remoteCmd string, stdin []byte) execResult
}

// Bootstrapper probes and, when needed, pushes the embedded runner onto remote
// hosts, caching the winning cache_root environment per host. One Bootstrapper
// is shared across steps and hosts (the per-host cache is mutex-guarded).
type Bootstrapper struct {
	exec sshExec
	log  *log.Logger

	mu    sync.Mutex
	hosts map[string]hostEnv // host name → resolved cache_root + raw uname
}

// hostEnv is a host's resolved bootstrap environment: the winning cache_root
// shell expression plus the raw uname tokens (mapped to goos/goarch per call).
type hostEnv struct {
	rootExpr string
	sysname  string // raw `uname -s`
	machine  string // raw `uname -m`
}

// NewBootstrapper returns a Bootstrapper wired to the real ssh transport and the
// standard logger (distinct diagnostics land there, security #5).
func NewBootstrapper() *Bootstrapper {
	return &Bootstrapper{
		exec:  resolveExec{},
		log:   log.Default(),
		hosts: make(map[string]hostEnv),
	}
}

// Bootstrap returns the verified remote runner path or a fallback verdict for
// srv. It NEVER returns an error and NEVER step-fails: every failure yields
// Fallback=true with a diagnostic Reason.
func (b *Bootstrapper) Bootstrap(ctx context.Context, srv *inventory.Server) Result {
	env, ok := b.resolveRoot(ctx, srv)
	if !ok {
		// noexec / read-only on ALL candidates — one explicit diagnostic, fail
		// fast, fall back (E&R: bootstrap ro/noexec).
		return fallback(fmt.Sprintf(
			"runner bootstrap failed on %s (no writable+executable cache dir: ~/.cache, $XDG_CACHE_HOME, /var/tmp, /dev/shm all unusable — noexec or read-only home) — ran under legacy path",
			srv.Name))
	}

	// Map uname to Go's platform spelling and select the embedded binary. An
	// unmatched arch is a DISTINCT fallback, never a push of the wrong binary
	// (E&R: arch unknown).
	goos, goarch, ok := mapUname(env.sysname, env.machine)
	if !ok {
		return fallback(fmt.Sprintf(
			"runner bootstrap skipped on %s (no embedded runner for uname %s/%s) — ran under legacy path",
			srv.Name, env.sysname, env.machine))
	}
	gz, err := RunnerGz(goos, goarch)
	if err != nil {
		return fallback(fmt.Sprintf(
			"runner bootstrap skipped on %s (no embedded runner for %s/%s) — ran under legacy path",
			srv.Name, goos, goarch))
	}

	final := runnerPath(env.rootExpr, goos, goarch)

	// Warm HIT: the right binary is already present.
	if b.versionMatches(ctx, srv, final) {
		return Result{RunnerPath: final}
	}

	// Miss: decompress daemon-side (push raw bytes so the remote needs only
	// cat/chmod/mv — no gunzip dependency, decision #3) and compute the sha256 of
	// the exact bytes we push (security #1).
	raw, err := gunzip(gz)
	if err != nil {
		return fallback(fmt.Sprintf(
			"runner bootstrap failed on %s (corrupt embedded runner for %s/%s: %v) — ran under legacy path",
			srv.Name, goos, goarch, err))
	}
	sum := sha256.Sum256(raw)
	wantSum := hex.EncodeToString(sum[:])

	// Push + verify, re-pushing ONCE on a digest OR exec-test mismatch (task
	// step 4). A transport/filesystem failure short of a mismatch is not retried.
	var lastReason string
	for attempt := 1; attempt <= 2; attempt++ {
		po := b.push(ctx, srv, env.rootExpr, final, raw, wantSum)
		switch po.kind {
		case pushFailed:
			return fallback(po.reason)
		case pushDigestMismatch:
			lastReason = po.reason
			continue // re-push (mv did not commit; no exec-test to run)
		case pushOK:
			if b.versionMatches(ctx, srv, final) {
				return Result{RunnerPath: final}
			}
			lastReason = fmt.Sprintf(
				"runner exec-test failed on %s (pushed runner did not report version %s)",
				srv.Name, RunnerVersion)
			b.log.Printf("shellkit bootstrap: %s (attempt %d/2)", lastReason, attempt)
		}
	}
	return fallback(lastReason + " — ran under legacy path")
}

// resolveRoot returns the host's cache_root environment (cached after the first
// probe). It walks the candidate chain (override → ~/.cache → $XDG_CACHE_HOME →
// /var/tmp → /dev/shm), smoke-testing each with a write+chmod+exec probe before
// committing, and reads uname from the same command. ok is false when every
// candidate is unusable (noexec / read-only everywhere).
func (b *Bootstrapper) resolveRoot(ctx context.Context, srv *inventory.Server) (hostEnv, bool) {
	b.mu.Lock()
	if e, hit := b.hosts[srv.Name]; hit {
		b.mu.Unlock()
		return e, true
	}
	b.mu.Unlock()

	for _, cand := range candidateRoots(srv) {
		res := b.exec.run(ctx, srv, smokeTestCmd(cand), nil)
		if !res.ok() {
			continue
		}
		sys, mach, ok := parseUname(string(res.Stdout))
		if !ok {
			// Dir works but no uname line came back — bizarre; try the next.
			continue
		}
		e := hostEnv{rootExpr: cand.expr, sysname: sys, machine: mach}
		b.mu.Lock()
		b.hosts[srv.Name] = e
		b.mu.Unlock()
		return e, true
	}
	return hostEnv{}, false
}

// versionMatches runs `<path> --version` and reports whether the output carries
// RunnerVersion — the probe HIT check and the post-push exec-test share it.
func (b *Bootstrapper) versionMatches(ctx context.Context, srv *inventory.Server, path string) bool {
	res := b.exec.run(ctx, srv, versionCmd(path), nil)
	if !res.ok() {
		return false
	}
	return strings.Contains(strings.TrimSpace(string(res.Stdout)), RunnerVersion)
}

// pushKind is the coarse outcome of one push attempt.
type pushKind int

const (
	pushOK pushKind = iota
	pushDigestMismatch
	pushFailed
)

type pushOutcome struct {
	kind   pushKind
	reason string
}

// push cats raw in-band to a hidden dotname tmp under rootExpr, chmod +x,
// verifies the remote sha256 equals wantSum (the daemon-side digest of the exact
// pushed bytes, security #1), then atomically `mv -f` to final. A digest
// mismatch is logged distinctly (security #5) and returned as a re-pushable
// outcome; any other failure returns pushFailed with a fallback reason.
func (b *Bootstrapper) push(ctx context.Context, srv *inventory.Server, rootExpr, final string, raw []byte, wantSum string) pushOutcome {
	res := b.exec.run(ctx, srv, pushCmd(rootExpr, final, wantSum), raw)
	gotSum := parseDigest(string(res.Stdout))

	if res.Err != nil {
		return pushOutcome{pushFailed, fmt.Sprintf(
			"runner bootstrap failed on %s (push transport error: %v) — ran under legacy path", srv.Name, res.Err)}
	}
	// The daemon is the authority on the digest: a remote-signalled mismatch
	// (exit 34) OR an echoed digest that disagrees with ours both count.
	if res.ExitCode == exitDigestMismatch || (gotSum != "" && gotSum != wantSum) {
		reason := fmt.Sprintf(
			"runner digest mismatch on %s (pushed sha256:%s, remote sha256:%s) — re-pushing once",
			srv.Name, short(wantSum), short(gotSum))
		b.log.Printf("shellkit bootstrap: %s", reason)
		return pushOutcome{pushDigestMismatch, reason}
	}
	if !res.ok() {
		return pushOutcome{pushFailed, fmt.Sprintf(
			"runner bootstrap failed on %s (push failed, exit %d) — ran under legacy path", srv.Name, res.ExitCode)}
	}
	return pushOutcome{pushOK, ""}
}

// runnerPath is the hash-named immutable final path for a platform under rootExpr.
func runnerPath(rootExpr, goos, goarch string) string {
	return fmt.Sprintf("%s/runner-%s-%s-%s", rootExpr, RunnerVersion, goos, goarch)
}

// gunzip decompresses the embedded runner blob to its raw executable bytes.
func gunzip(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func fallback(reason string) Result { return Result{Fallback: true, Reason: reason} }

func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}

// resolveExec is the production transport: it rides sshconn.ResolveInvocation so
// the runner exec inherits auth / jump / port / password and NO address is ever
// re-resolved (PortFor regression guard). remoteCmd is passed as the single
// remote command string; the remote login shell interprets it.
type resolveExec struct{}

func (resolveExec) run(ctx context.Context, srv *inventory.Server, remoteCmd string, stdin []byte) execResult {
	name, args, env, err := sshconn.ResolveInvocation(ctx, srv, remoteCmd)
	if err != nil {
		return execResult{ExitCode: -1, Err: fmt.Errorf("resolve ssh invocation for %s: %w", srv.Name, err)}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	res := execResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = err
		}
	}
	return res
}
