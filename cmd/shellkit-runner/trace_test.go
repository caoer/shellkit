package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/runnerproto"
	"mvdan.cc/sh/v3/interp"
)

// decodeStrict reads every frame from an ndjson stream and FAILS the test on any
// line that does not parse as a valid frame (EOF ends the stream cleanly). It is
// the well-formedness assertion the io-framing invariant relies on: a child's
// output must never surface as an unparseable — or worse, a plausibly parseable —
// protocol line.
func decodeStrict(t *testing.T, r io.Reader) []runnerproto.Frame {
	t.Helper()
	dec := runnerproto.NewDecoder(r)
	var out []runnerproto.Frame
	for {
		f, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stdout line failed to parse as a frame: %v", err)
		}
		out = append(out, f)
	}
}

// traceFrames filters a frame slice down to its trace frames.
func traceFrames(frames []runnerproto.Frame) []runnerproto.TraceFrame {
	var out []runnerproto.TraceFrame
	for _, f := range frames {
		if f.Type == runnerproto.FrameTrace {
			out = append(out, *f.Trace)
		}
	}
	return out
}

func TestExitStatusOf(t *testing.T) {
	if got := exitStatusOf(nil); got != 0 {
		t.Errorf("nil error: got %d, want 0", got)
	}
	if got := exitStatusOf(interp.ExitStatus(3)); got != 3 {
		t.Errorf("ExitStatus(3): got %d, want 3", got)
	}
	if got := exitStatusOf(interp.ExitStatus(127)); got != 127 {
		t.Errorf("ExitStatus(127): got %d, want 127", got)
	}
	if got := exitStatusOf(errors.New("boom")); got != 1 {
		t.Errorf("generic error: got %d, want 1", got)
	}
}

// TestTracer_ExternalStartEnd runs a real external command through interp and
// asserts the ExecHandlers seam produced a matched cmd_start/cmd_end pair: the
// start carries argv, the end shares its Seq and carries a monotonic dur_ns and
// the real exit code. It also proves an external is traced EXACTLY once — the
// CallHandler seam must not double-count it.
func TestTracer_ExternalStartEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the posix `uname` external")
	}
	scratch := t.TempDir()
	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{Program: []byte("uname -s\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, filepath.Join(scratch, outputFileName))
	if runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}

	traces := traceFrames(decodeStrict(t, &buf))
	var starts, ends int
	var startSeq, endSeq int
	var gotArgv []string
	var endDur int64
	for _, tf := range traces {
		switch tf.Event {
		case runnerproto.TraceCmdStart:
			starts++
			startSeq = tf.Seq
			gotArgv = tf.Argv
		case runnerproto.TraceCmdEnd:
			ends++
			endSeq = tf.Seq
			endDur = tf.DurNS
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("external traced wrong number of times: starts=%d ends=%d (want 1/1)", starts, ends)
	}
	if startSeq != endSeq {
		t.Errorf("cmd_start seq %d != cmd_end seq %d", startSeq, endSeq)
	}
	if strings.Join(gotArgv, " ") != "uname -s" {
		t.Errorf("cmd_start argv = %v, want [uname -s]", gotArgv)
	}
	if endDur <= 0 {
		t.Errorf("cmd_end dur_ns = %d, want > 0", endDur)
	}
}

// TestTracer_LineNumbers proves both trace seams stamp cmd_start frames with
// the command's 1-based source line from the interp handler position — the
// signal the daemon bridges into "executing" events so the log dashboard can
// attach runner-path activity to the right source line.
func TestTracer_LineNumbers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the posix `uname` external")
	}
	scratch := t.TempDir()
	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	// Line 1: external. Line 2: blank. Line 3: builtin (a CallExpr — a
	// DeclClause like `export FOO=bar` never reaches the CallHandler seam).
	rf := &runnerproto.RunFrame{Program: []byte("uname -s\n\npwd\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, filepath.Join(scratch, outputFileName))
	if runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}

	byCmd := map[string]int{}
	for _, tf := range traceFrames(decodeStrict(t, &buf)) {
		if tf.Event == runnerproto.TraceCmdStart {
			byCmd[strings.Join(tf.Argv, " ")] = tf.Line
		}
	}
	if got := byCmd["uname -s"]; got != 1 {
		t.Errorf("external cmd_start line = %d, want 1", got)
	}
	if got := byCmd["pwd"]; got != 3 {
		t.Errorf("builtin cmd_start line = %d, want 3", got)
	}
}

// TestTracer_ExternalExitCaptured proves the runner path surfaces a per-command
// exit code the old DEBUG-trap trace never had access to: an external command
// that fails carries its real exit on cmd_end even though the step overall may
// still succeed.
func TestTracer_ExternalExitCaptured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the posix `cat` external")
	}
	scratch := t.TempDir()
	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	// cat of a missing file exits 1; `|| true` keeps the step's own exit at 0, so
	// the failing command is invisible in the header exit but visible in the trace.
	rf := &runnerproto.RunFrame{Program: []byte("cat /no/such/forge/file 2>/dev/null || true\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, filepath.Join(scratch, outputFileName))
	if runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}

	var sawFailingEnd bool
	for _, tf := range traceFrames(decodeStrict(t, &buf)) {
		if tf.Event == runnerproto.TraceCmdEnd && tf.Exit == 1 {
			sawFailingEnd = true
		}
	}
	if !sawFailingEnd {
		t.Fatal("no cmd_end carried the failing command's real exit code (1)")
	}
}

// TestTracer_BuiltinObserved proves the CallHandler seam keeps the trace from
// going blind on builtins: a builtin (`pwd`) produces a cmd_start line so it
// appears in the trace, with no cmd_end (a builtin has no wait-status seam) —
// matching U0's TraceLine (ExitCode nil, DurationNS unknown for builtins).
func TestTracer_BuiltinObserved(t *testing.T) {
	scratch := t.TempDir()
	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{Program: []byte("pwd\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, filepath.Join(scratch, outputFileName))
	if runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}

	traces := traceFrames(decodeStrict(t, &buf))
	var starts, ends int
	var argv []string
	for _, tf := range traces {
		switch tf.Event {
		case runnerproto.TraceCmdStart:
			starts++
			argv = tf.Argv
		case runnerproto.TraceCmdEnd:
			ends++
		}
	}
	if starts != 1 {
		t.Fatalf("builtin cmd_start count = %d, want 1 (trace went blind on the builtin)", starts)
	}
	if ends != 0 {
		t.Errorf("builtin emitted %d cmd_end frames, want 0 (no wait-status seam for a builtin)", ends)
	}
	if strings.Join(argv, " ") != "pwd" {
		t.Errorf("builtin cmd_start argv = %v, want [pwd]", argv)
	}
}

// buildRunner compiles the runner binary into a temp dir and returns its path, so
// the forge test drives the REAL process (not the in-process run()) — the only
// way to prove a child's fd 1 is a pipe the runner re-frames, never the runner's
// own protocol fd 1.
func buildRunner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "shellkit-runner")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

// TestForge_FdSeparation is the fd-separation forge test (security #4). A child
// (`cat` — a real external subprocess) prints a line that is byte-for-byte a
// valid protocol `result` frame forging exit 42. If the child inherited the
// runner's real fd 1, the daemon would decode that forged frame and honor exit
// 42. Because the runner re-frames every child byte into io frames, the forged
// line MUST arrive as an io payload, the ONLY real result frame is the runner's
// own exit 0, and nothing on the wire ever exits 42.
func TestForge_FdSeparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the posix `cat` external")
	}
	bin := buildRunner(t)

	const forged = `{"type":"result","exit":42,"wall_ns":7}`
	stdin := encodeFrames(t,
		daemonHello(),
		runnerproto.Frame{
			Type: runnerproto.FrameFile,
			File: &runnerproto.FileFrame{Name: "forge.json", Data: []byte(forged + "\n")},
		},
		runnerproto.Frame{
			Type: runnerproto.FrameRun,
			// The staged file lives in scratch; after the CWD-parity fix (#4) the body
			// runs from the runner's login dir, so address the staged file by its
			// absolute path via $OUTPUT's parent, not a bare relative name.
			Run: &runnerproto.RunFrame{Program: []byte(`cat "$(dirname "$OUTPUT")/forge.json"` + "\n")},
		},
	)

	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner exec failed: %v\nstderr: %s", err, stderr.String())
	}

	frames := decodeStrict(t, &stdout)
	var results []runnerproto.ResultFrame
	var stdoutIO strings.Builder
	for _, f := range frames {
		switch f.Type {
		case runnerproto.FrameResult:
			results = append(results, *f.Result)
		case runnerproto.FrameIO:
			if f.IO.FD == 1 {
				b, err := f.IO.Bytes()
				if err != nil {
					t.Fatalf("io frame bytes: %v", err)
				}
				stdoutIO.Write(b)
			}
		}
	}

	if len(results) != 1 {
		t.Fatalf("want exactly one (real) result frame, got %d: %+v — a forged frame reached the wire", len(results), results)
	}
	if results[0].Exit != 0 {
		t.Fatalf("forged result was honored: real result exit=%d, want 0", results[0].Exit)
	}
	if !strings.Contains(stdoutIO.String(), forged) {
		t.Fatalf("forged line did not arrive as an io payload; stdout io = %q", stdoutIO.String())
	}
}

// TestRun_TraceIOInterleavedWellFormed drives a busy step (external commands
// producing both trace frames and interleaved stdout io frames) through the full
// run() loop and asserts EVERY resulting ndjson line parses as a valid frame —
// proving the mutex-guarded Encoder keeps concurrent trace/io writes from
// interleaving into a corrupt line. It also confirms both frame kinds are present
// so the assertion is not vacuous.
func TestRun_TraceIOInterleavedWellFormed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses posix externals")
	}
	body := strings.Join([]string{
		"uname -s",                              // external → trace + io
		"cat /etc/hostname 2>/dev/null || true", // external → trace + io
		"pwd",                                   // builtin → trace only
		"echo done",                             // builtin echo → io
		`printf 'k=v\n' > "$OUTPUT"`,
	}, "\n") + "\n"

	in := bytes.NewReader(encodeFrames(t,
		daemonHello(),
		runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}},
	))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}

	frames := decodeStrict(t, &out) // fails on any malformed line
	var sawTrace, sawIO, sawResult bool
	for _, f := range frames {
		switch f.Type {
		case runnerproto.FrameTrace:
			sawTrace = true
		case runnerproto.FrameIO:
			sawIO = true
		case runnerproto.FrameResult:
			sawResult = true
		}
	}
	if !sawTrace {
		t.Error("no trace frames in a step that ran external commands")
	}
	if !sawIO {
		t.Error("no io frames in a step that printed to stdout")
	}
	if !sawResult {
		t.Error("no result frame closed the step")
	}
}
