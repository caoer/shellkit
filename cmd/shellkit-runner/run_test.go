package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// fixedLookup returns a lookup func over a fixed env for buildStepEnv tests, so
// the policy is exercised independent of the test host's real environment.
func fixedLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func pairsToMap(pairs []string) map[string]string {
	m := map[string]string{}
	for _, p := range pairs {
		if i := strings.IndexByte(p, '='); i >= 0 {
			m[p[:i]] = p[i+1:]
		}
	}
	return m
}

// TestBuildStepEnv_ClosedAllowlist is the decision #17 security assertion: an
// extra key on the run frame must NOT reach the step env, a frame PATH override
// must NOT win over the runner's base PATH, and OUTPUT is forced to the
// runner-owned scratch path.
func TestBuildStepEnv_ClosedAllowlist(t *testing.T) {
	base := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/home/runner",
		"SECRET": "should-not-be-read", // not in baseEnvAllowlist → must be ignored
	}
	frameEnv := map[string]string{
		"SSHPASS": "hunter2",          // secret the daemon must never be able to inject
		"OUTPUT":  "/attacker/output", // frame OUTPUT must lose to the runner path
		"PATH":    "/evil/bin",        // PATH override attempt must be dropped
	}

	got := pairsToMap(buildStepEnv(frameEnv, "/scratch/.shellkit-output", fixedLookup(base)))

	if _, leaked := got["SSHPASS"]; leaked {
		t.Fatalf("frame secret SSHPASS leaked into step env: %v", got)
	}
	if _, leaked := got["SECRET"]; leaked {
		t.Fatalf("non-allowlisted base key SECRET leaked into step env: %v", got)
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("frame overrode PATH: got %q, want the runner base /usr/bin:/bin", got["PATH"])
	}
	if got["HOME"] != "/home/runner" {
		t.Fatalf("base HOME missing/wrong: got %q", got["HOME"])
	}
	if got["OUTPUT"] != "/scratch/.shellkit-output" {
		t.Fatalf("OUTPUT not forced to runner scratch path: got %q", got["OUTPUT"])
	}
}

func TestBuildStepEnv_Deterministic(t *testing.T) {
	base := map[string]string{"PATH": "/bin", "HOME": "/h"}
	a := buildStepEnv(nil, "/o", fixedLookup(base))
	b := buildStepEnv(nil, "/o", fixedLookup(base))
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("buildStepEnv not deterministic:\n%v\n%v", a, b)
	}
}

func TestSafeScratchPath(t *testing.T) {
	scratch := "/tmp/scratch"
	cases := []struct {
		name string
		ok   bool
	}{
		{"output.txt", true},
		{"step.output", true},
		{".hidden", true},
		{"../etc/x", false},  // traversal
		{"/abs/path", false}, // absolute
		{"a/b", false},       // directory component
		{"..", false},        // parent
		{".", false},         // current
		{"", false},          // empty
	}
	for _, tc := range cases {
		dest, err := safeScratchPath(scratch, tc.name)
		if tc.ok {
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.name, err)
				continue
			}
			if filepath.Dir(dest) != scratch {
				t.Errorf("%q: staged outside scratch: %q", tc.name, dest)
			}
		} else if err == nil {
			t.Errorf("%q: expected refusal, got dest %q", tc.name, dest)
		}
	}
}

func TestCollectOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// "=" inside a value must SplitN at the first '='; blank lines skipped; a
	// leading-'=' line (idx==0) is ignored, matching mcp.ParseOutputs.
	writeFile(t, out, "x=1\n\n  y = a=b \n=leading\nzonly\n")
	got := collectOutput(out)
	want := map[string]string{"x": "1", "y": "a=b"}
	if len(got) != len(want) {
		t.Fatalf("collectOutput = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestCollectOutput_Missing(t *testing.T) {
	if got := collectOutput(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Fatalf("missing output file: got %v, want nil", got)
	}
}

// TestRecoverInterpPanic verifies the recover→result mapping directly (a real
// upstream panic is version-fragile to trigger): exit 2 and the fallback hint.
func TestRecoverInterpPanic(t *testing.T) {
	code, msg := func() (code int, msg string) {
		defer recoverInterpPanic(&code, &msg)
		panic("boom")
	}()
	if code != 2 {
		t.Fatalf("panic exit = %d, want 2", code)
	}
	if !strings.Contains(msg, "interp panicked") || !strings.Contains(msg, panicFallbackHint) {
		t.Fatalf("panic message missing context/hint: %q", msg)
	}
}

// TestRunInterp_OutputAndExit runs a real bash body through interp: it writes to
// $OUTPUT and exits non-zero, proving env wiring ($OUTPUT resolves), output
// collection, and exit propagation.
func TestRunInterp_OutputAndExit(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)
	writeFile(t, outputPath, "") // pre-created, as handleRun does

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{Program: []byte("printf 'k=v\\n' > \"$OUTPUT\"\nexit 4\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, outputPath)
	if runErr != "" {
		t.Fatalf("unexpected runner error: %q", runErr)
	}
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if got := collectOutput(outputPath); got["k"] != "v" {
		t.Fatalf("$OUTPUT not collected: %v", got)
	}
}

// TestRunInterp_EnvClosed proves at runtime that a secret on the frame env is
// not visible to the body: the body prints $SSHPASS, which must be empty.
func TestRunInterp_EnvClosed(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{
		Program: []byte(`printf 'leak=[%s]\n' "$SSHPASS"`),
		Env:     map[string]string{"SSHPASS": "hunter2"},
	}
	if code, runErr := r.runInterp(context.Background(), rf, scratch, outputPath); runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}
	frames := decodeFrames(t, &buf)
	var stdout strings.Builder
	for _, f := range frames {
		if f.Type == runnerproto.FrameIO && f.IO.FD == 1 {
			b, _ := f.IO.Bytes()
			stdout.Write(b)
		}
	}
	if !strings.Contains(stdout.String(), "leak=[]") {
		t.Fatalf("SSHPASS leaked into step: stdout = %q", stdout.String())
	}
}

// TestRunInterp_InheritsProcessCWD is the CWD-parity assertion (#4): the interp
// body must run in the runner PROCESS's inherited working directory (the remote
// login dir), NOT the ephemeral scratch dir. It chdirs the test process to a
// known dir, runs `pwd`, and asserts the reported cwd is that dir — never scratch
// — so `cat .env` / `./deploy.sh` / relative paths behave as under legacy bash.
func TestRunInterp_InheritsProcessCWD(t *testing.T) {
	loginDir := t.TempDir()
	// os.MkdirTemp can hand back a /var symlink to /private/var on macOS; resolve
	// both sides so the comparison is against the same canonical path pwd reports.
	loginDir, err := filepath.EvalSymlinks(loginDir)
	if err != nil {
		t.Fatalf("evalsymlinks login dir: %v", err)
	}
	chdir(t, loginDir)

	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)
	writeFile(t, outputPath, "")

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{Program: []byte("pwd\n")}

	if code, runErr := r.runInterp(context.Background(), rf, scratch, outputPath); runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}
	got := strings.TrimSpace(stdoutOf(t, &buf))
	if got != loginDir {
		t.Fatalf("interp cwd = %q, want inherited login dir %q (scratch was %q)", got, loginDir, scratch)
	}
	if got == scratch {
		t.Fatalf("interp ran in scratch %q — CWD parity lost", scratch)
	}
}

// TestCapOutputs_UnderCap leaves small $OUTPUT sets untouched (no truncation, no
// diagnostic).
func TestCapOutputs_UnderCap(t *testing.T) {
	vals := map[string]string{"a": "1", "b": strings.Repeat("x", 1000)}
	note := capOutputs(vals)
	if note != "" {
		t.Fatalf("small outputs were capped: %q", note)
	}
	if vals["b"] != strings.Repeat("x", 1000) {
		t.Fatalf("small value mutated: len=%d", len(vals["b"]))
	}
}

// TestCapOutputs_TruncatesOversized proves an oversized $OUTPUT value is
// truncated (with the marker) so the aggregate stays under the cap, the key is
// NEVER dropped, and a diagnostic note is returned.
func TestCapOutputs_TruncatesOversized(t *testing.T) {
	huge := strings.Repeat("A", 2*maxOutputAggregateBytes)
	vals := map[string]string{"big": huge, "small": "keep"}
	note := capOutputs(vals)
	if note == "" {
		t.Fatalf("oversized output was not reported")
	}
	if _, ok := vals["big"]; !ok {
		t.Fatalf("oversized key was dropped instead of truncated")
	}
	if _, ok := vals["small"]; !ok {
		t.Fatalf("second key was dropped")
	}
	if !strings.HasSuffix(vals["big"], outputTruncatedMarker) {
		t.Fatalf("truncated value missing marker: tail=%q", tail(vals["big"], 40))
	}
	// The property that matters (#1a): the serialized output frame must stay under
	// MaxLineBytes so the peer decoder accepts it. Encode the capped frame and check
	// its on-wire length (JSON-escaped values included).
	var line bytes.Buffer
	if err := runnerproto.NewEncoder(&line).Encode(runnerproto.Frame{
		Type:   runnerproto.FrameOutput,
		Output: &runnerproto.OutputFrame{Values: vals},
	}); err != nil {
		t.Fatalf("encode capped output frame: %v", err)
	}
	if line.Len() >= runnerproto.MaxLineBytes {
		t.Fatalf("capped output frame line = %d bytes, want < MaxLineBytes (%d)", line.Len(), runnerproto.MaxLineBytes)
	}
}

// TestHandleRun_HugeOutputRoundTrips is the #1a regression proof: a body writing
// a >1 MiB $OUTPUT value round-trips to the step's REAL exit code through the
// full frame loop — the output frame is truncated under MaxLineBytes, decoded
// cleanly by the peer, and the result frame reports the real exit, not a protocol
// failure surfaced as exit -1.
func TestHandleRun_HugeOutputRoundTrips(t *testing.T) {
	// 2 MiB single value, well over MaxLineBytes; the body exits 3.
	body := "yes A | head -c 2097152 | tr -d '\\n' | sed 's/^/big=/' > \"$OUTPUT\"\nexit 3\n"
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run:  &runnerproto.RunFrame{Program: []byte(body)},
	}
	in := bytes.NewReader(encodeFrames(t, daemonHello(), runFrame))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The peer decoder must accept every frame (no ErrProtocol on an oversized
	// output line) — decodeFrames stops at the first decode error, so a rejected
	// output frame would drop the trailing result frame.
	frames := decodeFrames(t, &out)
	var sawOutput, sawResult bool
	var exit int
	var bigVal string
	for _, f := range frames {
		switch f.Type {
		case runnerproto.FrameOutput:
			sawOutput = true
			bigVal = f.Output.Values["big"]
		case runnerproto.FrameResult:
			sawResult = true
			exit = f.Result.Exit
		}
	}
	if !sawResult {
		t.Fatalf("no result frame — oversized output frame was likely rejected as a protocol error")
	}
	if exit != 3 {
		t.Fatalf("exit = %d, want 3 (the real step exit, not a protocol -1)", exit)
	}
	if !sawOutput {
		t.Fatalf("no output frame emitted")
	}
	if !strings.HasSuffix(bigVal, outputTruncatedMarker) {
		t.Fatalf("huge output was not truncated with the marker: len=%d tail=%q", len(bigVal), tail(bigVal, 40))
	}
	// And the diagnostic landed on the self-diagnostics channel, never a frame.
	if !strings.Contains(errOut.String(), "$OUTPUT exceeded") {
		t.Fatalf("expected a truncation diagnostic on stderr, got %q", errOut.String())
	}
}

// TestHandleRun_OutputCreateFailure proves #13: when $OUTPUT cannot be created,
// the step is reported as a FAILED result BEFORE the body runs — never a silent
// exit-0-with-empty-output. Scratch is forced to a path where create fails.
func TestHandleRun_OutputCreateFailure(t *testing.T) {
	// Point scratch at a regular file so filepath.Join(scratch, name) can't be
	// created (ENOTDIR).
	notADir := filepath.Join(t.TempDir(), "iamafile")
	writeFile(t, notADir, "x")

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf, scratch: notADir}
	// A body whose LAST command exits 0 — the trap the bug set: without the guard
	// this would report success while the declared output silently vanished.
	rf := &runnerproto.RunFrame{Program: []byte("true\n")}

	if err := r.handleRun(context.Background(), rf); err != nil {
		t.Fatalf("handleRun: %v", err)
	}
	frames := decodeFrames(t, &buf)
	var res *runnerproto.ResultFrame
	for _, f := range frames {
		if f.Type == runnerproto.FrameResult {
			res = f.Result
		}
	}
	if res == nil {
		t.Fatalf("no result frame emitted")
	}
	if res.Exit == 0 || res.Error == "" {
		t.Fatalf("create failure reported as success: exit=%d error=%q", res.Exit, res.Error)
	}
	if !strings.Contains(res.Error, "$OUTPUT create failed") {
		t.Fatalf("result error missing create-failure context: %q", res.Error)
	}
}

// stdoutOf concatenates the fd-1 io frame payloads from a runner output buffer.
func stdoutOf(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range decodeFrames(t, buf) {
		if f.Type == runnerproto.FrameIO && f.IO.FD == 1 {
			b, _ := f.IO.Bytes()
			sb.Write(b)
		}
	}
	return sb.String()
}

// chdir changes the working dir for the test and restores it after (t.Chdir
// exists in Go 1.24+, but restore-on-cleanup is spelled out here to stay
// toolchain-agnostic).
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// tail returns the last n bytes of s for compact failure messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestHasBackgroundStmt locks the static background-detection contract that gates
// the run-exit reap: a `&` anywhere in the body (top-level or nested inside a
// function, subshell, loop, or conditional — interp backgrounds all of them via an
// un-joined goroutine) must be detected, while a body with no `&` must not be. A
// false negative would let a late-registering orphan leak; a false positive only
// costs a bounded reap poll on a step that never backgrounds.
func TestHasBackgroundStmt(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"top-level bg", "sleep 30 &\n", true},
		{"bg no wait after fg", "echo hi\nsleep 30 &\n", true},
		{"bg inside function", "f() { sleep 30 & }\nf\n", true},
		{"bg inside subshell", "( sleep 30 & )\n", true},
		{"bg inside loop", "for i in 1 2 3; do sleep 30 & done\n", true},
		{"bg inside if", "if true; then sleep 30 & fi\n", true},
		{"no bg simple", "echo hi\nsleep 1\n", false},
		{"ampersand in string literal", "echo 'a & b'\n", false},
		{"logical and not bg", "true && echo ok\n", false},
		{"empty body", "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := interpParser().Parse(strings.NewReader(tc.body), "")
			if err != nil {
				t.Fatalf("parse %q: %v", tc.body, err)
			}
			if got := hasBackgroundStmt(prog); got != tc.want {
				t.Fatalf("hasBackgroundStmt(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileErr(path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
