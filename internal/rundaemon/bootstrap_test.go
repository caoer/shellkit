package rundaemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/caoer/shellkit/internal/inventory"
)

// hostSim is an injectable sshExec that scripts a remote host's responses to the
// bootstrap's probe / push / version commands — no real ssh, the way
// client_test.go fakes the runner. It classifies each command by its sentinel
// and replays scripted results, recording every call for assertions.
type hostSim struct {
	t *testing.T

	// rootOK reports whether the candidate whose smoke-test command is cmd should
	// pass its write+chmod+exec probe. nil ⇒ only the $HOME/.cache candidate passes.
	rootOK func(cmd string) bool
	// uname is the "<sysname> <machine>" the smoke test reports; "" ⇒ Linux x86_64.
	uname string

	// versionSeq scripts the "is the on-disk binary the one we want" checks, in
	// order: the FIRST entry answers the warm-hit cached-digest probe
	// (cachedDigestCmd — true ⇒ digest matches, a warm HIT with no push), and each
	// SUBSEQUENT entry answers a post-push exec-test (`--version`). Exhausted ⇒
	// miss/fail. Authoring convention is unchanged from the pre-digest-check flow:
	// entry[0] is the probe result, later entries are exec-tests.
	versionSeq []bool
	// pushSeq is consumed one entry per push; exhausted ⇒ a clean matching push.
	pushSeq []pushSim

	mu    sync.Mutex
	calls []simCall
}

type pushSim struct {
	digest       string // remote-reported sha256; "" ⇒ echo the daemon's want (match)
	exit         int    // non-zero ⇒ filesystem failure (mkdir/chmod/mv)
	transportErr bool   // ssh could not run at all
}

type simCall struct {
	kind  string
	cmd   string
	stdin []byte
	srv   *inventory.Server
}

func cmdKind(cmd string) string {
	switch {
	case strings.Contains(cmd, "SHELLKIT_ROOT_OK"):
		return "smoke"
	case strings.Contains(cmd, "--version"):
		return "version"
	case strings.Contains(cmd, "SHELLKIT_PUSH_OK"):
		return "push"
	// The warm-hit digest check (cachedDigestCmd) prints SHELLKIT_DIGEST like a
	// push does, but reads a cached file (`[ -f`) instead of committing one — so
	// it is classified before the generic SHELLKIT_DIGEST/push fallthrough.
	case strings.Contains(cmd, "SHELLKIT_DIGEST=") && strings.Contains(cmd, "[ -f "):
		return "cacheddigest"
	default:
		return "unknown"
	}
}

func (h *hostSim) run(_ context.Context, srv *inventory.Server, cmd string, stdin []byte) execResult {
	kind := cmdKind(cmd)
	h.mu.Lock()
	h.calls = append(h.calls, simCall{kind, cmd, append([]byte(nil), stdin...), srv})
	h.mu.Unlock()

	switch kind {
	case "smoke":
		pass := h.rootOK
		if pass == nil {
			pass = func(c string) bool { return strings.Contains(c, `d="$HOME/.cache/shellkit"`) }
		}
		if !pass(cmd) {
			return execResult{ExitCode: 14} // noexec
		}
		uname := h.uname
		if uname == "" {
			uname = "Linux x86_64"
		}
		return execResult{Stdout: []byte("SHELLKIT_UNAME=" + uname + "\nSHELLKIT_ROOT_OK\n")}
	case "cacheddigest":
		// The warm-hit trust check: on a scripted HIT, echo the daemon's own
		// wanted digest back (a byte-exact match); on a miss, echo a bad digest so
		// the daemon treats the cache as untrusted and re-pushes.
		want := digestFromCachedCmd(cmd)
		if h.popVersion() {
			return execResult{Stdout: []byte("SHELLKIT_DIGEST=" + want + "\n")}
		}
		return execResult{Stdout: []byte("SHELLKIT_DIGEST=cafebabe0000\n")}
	case "version":
		if h.popVersion() {
			return execResult{Stdout: []byte("shellkit-runner " + RunnerVersion + "\n")}
		}
		return execResult{ExitCode: 40} // not present / version miss
	case "push":
		p := h.popPush()
		if p.transportErr {
			return execResult{ExitCode: -1, Err: errors.New("ssh: connect timeout")}
		}
		want := extractShellVar(cmd, "want")
		got := p.digest
		if got == "" {
			got = want
		}
		out := "SHELLKIT_DIGEST=" + got + "\n"
		switch {
		case p.exit != 0:
			return execResult{Stdout: []byte(out), ExitCode: p.exit}
		case got != want:
			// Mirror the real remote command: abort with exit 34, no mv.
			return execResult{Stdout: []byte(out), ExitCode: exitDigestMismatch}
		default:
			return execResult{Stdout: []byte(out + "SHELLKIT_PUSH_OK\n")}
		}
	}
	h.t.Fatalf("hostSim: unexpected command: %q", cmd)
	return execResult{}
}

func (h *hostSim) popVersion() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.versionSeq) == 0 {
		return false
	}
	v := h.versionSeq[0]
	h.versionSeq = h.versionSeq[1:]
	return v
}

func (h *hostSim) popPush() pushSim {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pushSeq) == 0 {
		return pushSim{}
	}
	p := h.pushSeq[0]
	h.pushSeq = h.pushSeq[1:]
	return p
}

func (h *hostSim) callsOf(kind string) []simCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []simCall
	for _, c := range h.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// digestFromCachedCmd computes the sha256 the daemon expects for the cached
// binary named in a cachedDigestCmd. The command sets `p="<root>/runner-<ver>-
// <goos>-<goarch>"`; the fake parses the goos/goarch off the path, decompresses
// the embedded binary for that platform, and returns its sha256 so a scripted
// warm HIT echoes a byte-exact match.
func digestFromCachedCmd(cmd string) string {
	p := extractShellVar(cmd, "p")
	// p is ".../runner-<ver>-<goos>-<goarch>"; take the last two dash fields.
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	fields := strings.Split(base, "-")
	if len(fields) < 2 {
		return ""
	}
	goos := fields[len(fields)-2]
	goarch := fields[len(fields)-1]
	gz, err := RunnerGz(goos, goarch)
	if err != nil {
		return ""
	}
	raw, err := gunzip(gz)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// extractShellVar pulls the value of a `name="<value>"` assignment from a
// generated command (used to read the daemon's wanted digest out of pushCmd).
func extractShellVar(cmd, name string) string {
	marker := name + `="`
	i := strings.Index(cmd, marker)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func newTestBootstrapper(exec sshExec, logw io.Writer) *Bootstrapper {
	return &Bootstrapper{exec: exec, log: log.New(logw, "", 0), hosts: map[string]hostEnv{}}
}

func testServer(name string) *inventory.Server { return &inventory.Server{Name: name} }

func mustRunnerGz(t *testing.T, goos, goarch string) []byte {
	t.Helper()
	b, err := RunnerGz(goos, goarch)
	if err != nil {
		t.Fatalf("RunnerGz(%s, %s): %v", goos, goarch, err)
	}
	return b
}

// wantLinuxAmd64Path is the path bootstrap resolves for the default sim
// (Linux/x86_64 host, $HOME/.cache candidate winning).
func wantLinuxAmd64Path() string {
	return "$HOME/.cache/shellkit/runner-" + RunnerVersion + "-linux-amd64"
}

func TestBootstrap_WarmHitReturnsPath(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{true}}
	b := newTestBootstrapper(sim, io.Discard)
	srv := testServer("host-a")

	res := b.Bootstrap(context.Background(), srv)
	if res.Fallback {
		t.Fatalf("warm hit → fallback: %+v", res)
	}
	if res.RunnerPath != wantLinuxAmd64Path() {
		t.Errorf("RunnerPath = %q, want %q", res.RunnerPath, wantLinuxAmd64Path())
	}
	if n := len(sim.callsOf("push")); n != 0 {
		t.Errorf("push calls = %d, want 0 (warm hit must not push)", n)
	}
}

func TestBootstrap_MissPushesThenReturnsPath(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{false, true}} // probe miss, exec-test hit
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("host-b"))
	if res.Fallback {
		t.Fatalf("miss→push→hit should succeed, got fallback: %s", res.Reason)
	}
	if res.RunnerPath != wantLinuxAmd64Path() {
		t.Errorf("RunnerPath = %q, want %q", res.RunnerPath, wantLinuxAmd64Path())
	}

	pushes := sim.callsOf("push")
	if len(pushes) != 1 {
		t.Fatalf("push calls = %d, want 1", len(pushes))
	}
	// The pushed bytes are the DECOMPRESSED embedded binary (decision #3: no
	// remote gunzip dependency) and the digest in the command is their sha256
	// (security #1: the daemon holds the exact bytes it pushed).
	raw, err := gunzip(mustRunnerGz(t, "linux", "amd64"))
	if err != nil {
		t.Fatalf("gunzip embedded runner: %v", err)
	}
	if !bytes.Equal(pushes[0].stdin, raw) {
		t.Errorf("pushed stdin (%d bytes) != decompressed embedded binary (%d bytes)", len(pushes[0].stdin), len(raw))
	}
	sum := sha256.Sum256(raw)
	if got := extractShellVar(pushes[0].cmd, "want"); got != hex.EncodeToString(sum[:]) {
		t.Errorf("push digest want=%q, expected sha256 of pushed bytes %q", got, hex.EncodeToString(sum[:]))
	}
}

// TestBootstrap_WarmHitVerifiesDigestNotVersion pins the security fix: a warm hit
// is trusted ONLY on a byte-exact digest match, and the trust check reads the
// cached binary's sha256 (cachedDigestCmd) — it never trusts a --version string.
func TestBootstrap_WarmHitVerifiesDigestNotVersion(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{true}} // cached-digest probe matches
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("warm"))
	if res.Fallback {
		t.Fatalf("digest-matched warm hit → fallback: %+v", res)
	}
	if res.RunnerPath != wantLinuxAmd64Path() {
		t.Errorf("RunnerPath = %q, want %q", res.RunnerPath, wantLinuxAmd64Path())
	}
	if n := len(sim.callsOf("cacheddigest")); n != 1 {
		t.Errorf("cacheddigest calls = %d, want 1 (warm hit must verify the digest)", n)
	}
	if n := len(sim.callsOf("version")); n != 0 {
		t.Errorf("version calls = %d, want 0 (warm hit must not trust --version)", n)
	}
	if n := len(sim.callsOf("push")); n != 0 {
		t.Errorf("push calls = %d, want 0 (digest match ⇒ no push)", n)
	}
}

// TestBootstrap_WarmHitWrongDigestRepushes proves a version-spoofing plant is
// rejected: a binary sits at the predictable path but its digest does NOT match
// the daemon's embedded bytes (the attack #8 describes), so bootstrap treats it
// as untrusted, re-pushes the real bytes, and exec-tests the pushed binary.
func TestBootstrap_WarmHitWrongDigestRepushes(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{false, true}} // cached-digest miss, post-push exec-test hit
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("planted"))
	if res.Fallback {
		t.Fatalf("wrong-digest cache should re-push and succeed, got fallback: %s", res.Reason)
	}
	if res.RunnerPath != wantLinuxAmd64Path() {
		t.Errorf("RunnerPath = %q, want %q", res.RunnerPath, wantLinuxAmd64Path())
	}
	if n := len(sim.callsOf("cacheddigest")); n != 1 {
		t.Errorf("cacheddigest calls = %d, want 1", n)
	}
	if n := len(sim.callsOf("push")); n != 1 {
		t.Errorf("push calls = %d, want 1 (untrusted cache ⇒ re-push the real bytes)", n)
	}
}

func TestBootstrap_NoExecAllCandidatesFallsBack(t *testing.T) {
	sim := &hostSim{t: t, rootOK: func(string) bool { return false }} // every candidate noexec/ro
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("ro-host"))
	if !res.Fallback {
		t.Fatalf("noexec everywhere must fall back, got %+v", res)
	}
	if !strings.Contains(res.Reason, "noexec or read-only") {
		t.Errorf("Reason = %q, want a noexec/read-only diagnostic", res.Reason)
	}
	if n := len(sim.callsOf("smoke")); n != 4 {
		t.Errorf("smoke calls = %d, want 4 (full candidate chain tried)", n)
	}
	if n := len(sim.callsOf("version")) + len(sim.callsOf("push")); n != 0 {
		t.Errorf("no version/push must be attempted after root resolution fails; got %d", n)
	}
}

func TestBootstrap_FallsThroughToVarTmp(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{false, true},
		rootOK: func(c string) bool { return strings.Contains(c, `d="/var/tmp/shellkit"`) }}
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("noexec-home"))
	if res.Fallback {
		t.Fatalf("should fall through to /var/tmp, got fallback: %s", res.Reason)
	}
	if !strings.HasPrefix(res.RunnerPath, "/var/tmp/shellkit/runner-") {
		t.Errorf("RunnerPath = %q, want /var/tmp/shellkit/... (fell through the chain)", res.RunnerPath)
	}
}

func TestBootstrap_RunnerTmpOverrideTriedFirst(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{true},
		rootOK: func(c string) bool { return strings.Contains(c, `d="/mnt/fast/shellkit"`) }}
	b := newTestBootstrapper(sim, io.Discard)
	srv := testServer("ovr")
	srv.RunnerTmp = "/mnt/fast/shellkit"

	res := b.Bootstrap(context.Background(), srv)
	if res.Fallback {
		t.Fatalf("override root should win, got fallback: %s", res.Reason)
	}
	if !strings.HasPrefix(res.RunnerPath, "/mnt/fast/shellkit/runner-") {
		t.Errorf("RunnerPath = %q, want the override root", res.RunnerPath)
	}
	smokes := sim.callsOf("smoke")
	if len(smokes) == 0 || !strings.Contains(smokes[0].cmd, `d="/mnt/fast/shellkit"`) {
		t.Errorf("first smoke test must probe the runner_tmp override; got %d calls", len(smokes))
	}
}

func TestBootstrap_DigestMismatchRepushesOnce(t *testing.T) {
	var logbuf bytes.Buffer
	sim := &hostSim{t: t,
		versionSeq: []bool{false, true},                     // probe miss, exec-test after 2nd push hit
		pushSeq:    []pushSim{{digest: "deadbeefcafe"}, {}}} // push1 corrupts, push2 matches
	b := newTestBootstrapper(sim, &logbuf)

	res := b.Bootstrap(context.Background(), testServer("flaky"))
	if res.Fallback {
		t.Fatalf("digest mismatch then clean re-push should succeed, got fallback: %s", res.Reason)
	}
	if n := len(sim.callsOf("push")); n != 2 {
		t.Errorf("push calls = %d, want 2 (re-push once on digest mismatch)", n)
	}
	if !strings.Contains(logbuf.String(), "digest mismatch") {
		t.Errorf("digest mismatch must be a DISTINCT logged diagnostic; log = %q", logbuf.String())
	}
}

func TestBootstrap_DigestMismatchTwiceFallsBack(t *testing.T) {
	var logbuf bytes.Buffer
	sim := &hostSim{t: t, versionSeq: []bool{false},
		pushSeq: []pushSim{{digest: "bad1bad1bad1"}, {digest: "bad2bad2bad2"}}}
	b := newTestBootstrapper(sim, &logbuf)

	res := b.Bootstrap(context.Background(), testServer("corrupt"))
	if !res.Fallback {
		t.Fatalf("persistent digest mismatch must fall back, got %+v", res)
	}
	if n := len(sim.callsOf("push")); n != 2 {
		t.Errorf("push calls = %d, want 2 (one re-push, then fallback)", n)
	}
	if !strings.Contains(res.Reason, "digest mismatch") {
		t.Errorf("Reason = %q, want a digest-mismatch diagnostic", res.Reason)
	}
}

func TestBootstrap_ArchUnknownFallsBack(t *testing.T) {
	sim := &hostSim{t: t, uname: "Linux riscv64", rootOK: func(string) bool { return true }}
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("risc"))
	if !res.Fallback {
		t.Fatalf("unknown arch must fall back, got %+v", res)
	}
	if !strings.Contains(res.Reason, "no embedded runner") {
		t.Errorf("Reason = %q, want an arch-unknown diagnostic", res.Reason)
	}
	if n := len(sim.callsOf("push")); n != 0 {
		t.Errorf("push calls = %d, want 0 (never push the wrong binary)", n)
	}
}

func TestBootstrap_ExecTestFailsFallsBack(t *testing.T) {
	var logbuf bytes.Buffer
	sim := &hostSim{t: t,
		versionSeq: []bool{false, false, false}, // probe + both exec-tests miss
		pushSeq:    []pushSim{{}, {}}}           // both pushes are clean digest matches
	b := newTestBootstrapper(sim, &logbuf)

	res := b.Bootstrap(context.Background(), testServer("badbin"))
	if !res.Fallback {
		t.Fatalf("exec-test failing twice must fall back, got %+v", res)
	}
	if n := len(sim.callsOf("push")); n != 2 {
		t.Errorf("push calls = %d, want 2 (re-push once on exec-test failure)", n)
	}
	if !strings.Contains(res.Reason, "exec-test failed") {
		t.Errorf("Reason = %q, want an exec-test diagnostic", res.Reason)
	}
	if !strings.Contains(logbuf.String(), "exec-test failed") {
		t.Errorf("exec-test failure must be a DISTINCT logged diagnostic; log = %q", logbuf.String())
	}
}

func TestBootstrap_PushTransportErrorFallsBack(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{false},
		pushSeq: []pushSim{{transportErr: true}}}
	b := newTestBootstrapper(sim, io.Discard)

	res := b.Bootstrap(context.Background(), testServer("down"))
	if !res.Fallback {
		t.Fatalf("push transport error must fall back, got %+v", res)
	}
	if n := len(sim.callsOf("push")); n != 1 {
		t.Errorf("push calls = %d, want 1 (transport error is not retried)", n)
	}
	if !strings.Contains(res.Reason, "transport error") {
		t.Errorf("Reason = %q, want a transport-error diagnostic", res.Reason)
	}
}

// TestBootstrap_NoAddressReResolvedAndCaches proves two invariants at once: the
// transport only ever receives the exact *inventory.Server bootstrap was handed
// (nothing re-resolves or clones an address — the PortFor regression guard), and
// the resolved cache_root is reused across calls (one smoke test, not two).
func TestBootstrap_NoAddressReResolvedAndCaches(t *testing.T) {
	sim := &hostSim{t: t, versionSeq: []bool{true, true}} // two warm hits
	b := newTestBootstrapper(sim, io.Discard)
	srv := testServer("cached")

	r1 := b.Bootstrap(context.Background(), srv)
	r2 := b.Bootstrap(context.Background(), srv)
	if r1.Fallback || r2.Fallback || r1.RunnerPath != r2.RunnerPath {
		t.Fatalf("expected two identical warm hits, got %+v / %+v", r1, r2)
	}
	for i, c := range sim.calls {
		if c.srv != srv {
			t.Errorf("call %d handed a different *Server (%p != %p) — an address was re-resolved/cloned", i, c.srv, srv)
		}
	}
	if n := len(sim.callsOf("smoke")); n != 1 {
		t.Errorf("smoke calls = %d, want 1 (cache_root cached across Bootstrap calls)", n)
	}
}

// TestNewBootstrapper_RidesResolveInvocation pins that the production transport
// is the ResolveInvocation-riding one (its body builds no ssh args itself).
func TestNewBootstrapper_RidesResolveInvocation(t *testing.T) {
	b := NewBootstrapper()
	if _, ok := b.exec.(resolveExec); !ok {
		t.Fatalf("production transport = %T, want resolveExec", b.exec)
	}
}

func TestMapUname(t *testing.T) {
	cases := []struct {
		sys, mach    string
		goos, goarch string
		ok           bool
	}{
		{"Linux", "x86_64", "linux", "amd64", true},
		{"Linux", "aarch64", "linux", "arm64", true},
		{"Darwin", "arm64", "darwin", "arm64", true},
		{"Darwin", "x86_64", "darwin", "amd64", true},
		{"Linux", "riscv64", "", "", false},
		{"FreeBSD", "amd64", "", "", false},
	}
	for _, c := range cases {
		goos, goarch, ok := mapUname(c.sys, c.mach)
		if ok != c.ok || goos != c.goos || goarch != c.goarch {
			t.Errorf("mapUname(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.sys, c.mach, goos, goarch, ok, c.goos, c.goarch, c.ok)
		}
	}
}

func TestTaggedLineIgnoresBannerNoise(t *testing.T) {
	out := "Welcome to Ubuntu 22.04\nLast login: today\nSHELLKIT_UNAME=Linux x86_64\nSHELLKIT_ROOT_OK\n"
	sys, mach, ok := parseUname(out)
	if !ok || sys != "Linux" || mach != "x86_64" {
		t.Errorf("parseUname past banner noise = (%q,%q,%v), want (Linux,x86_64,true)", sys, mach, ok)
	}
}
