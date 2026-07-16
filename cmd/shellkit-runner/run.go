package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// outputFileName is the basename of the $OUTPUT scratch file the runner mints
// inside each step's scratch dir. The step writes key=value lines to $OUTPUT;
// the runner reads them back after the body completes and emits an output frame.
const outputFileName = ".shellkit-output"

// panicFallbackHint is appended to a recovered-panic result so the caller knows
// the escape hatch. Kept as a const so the U3b/U6b teaching-error copy and the
// test assert the same string.
const panicFallbackHint = `retry this step with "interp": false to run under real bash`

// allowedRunEnvKeys is the CLOSED set of keys a RUN FRAME may contribute to the
// step environment (plan decision #17): OUTPUT and nothing else. Every other key
// carried on the frame — including an attempt to override PATH — is dropped. So a
// daemon can never spray a secret (SSHPASS, sops material, agent creds) or a
// poisoned PATH into a fleet runner through the frame env. This is a security
// boundary; run_test.go asserts an extra frame key never reaches the step env.
var allowedRunEnvKeys = map[string]bool{"OUTPUT": true}

// baseEnvAllowlist names the NON-secret operational vars the runner copies from
// its OWN process environment (the remote ssh session's env — exactly what a
// script inherits today) so external commands resolve (PATH) and behave sanely
// (HOME/LANG/TZ). It is a deliberate, named read — NEVER a wholesale os.Environ()
// copy (decision #17: "never os.Environ() blindly") — so nothing unexpected (an
// forwarded SSH_AUTH_SOCK, a stray token) leaks in, and the run frame cannot
// inject or override any of these.
var baseEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ",
}

// buildStepEnv produces the step's environment as sorted KEY=VALUE pairs:
//
//  1. the operational base pulled by name from the runner's own env (via lookup),
//  2. plus only the allowlisted keys from the run frame's env,
//  3. with OUTPUT forced to the runner-owned scratch path (which always wins, so
//     the runner knows where to read the collected outputs back).
//
// lookup is injected (os.LookupEnv in production) so tests exercise the policy
// against a fixed env. The result is deterministic (sorted).
func buildStepEnv(frameEnv map[string]string, outputPath string, lookup func(string) (string, bool)) []string {
	env := map[string]string{}
	for _, k := range baseEnvAllowlist {
		if v, ok := lookup(k); ok {
			env[k] = v
		}
	}
	for k, v := range frameEnv {
		if allowedRunEnvKeys[k] {
			env[k] = v
		}
	}
	env["OUTPUT"] = outputPath // runner-owned scratch path; always authoritative

	pairs := make([]string, 0, len(env))
	for k, v := range env {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return pairs
}

// handleRun executes one run frame end to end: mint scratch + $OUTPUT, run the
// body (interp for bash, a supervised subprocess otherwise), collect $OUTPUT,
// then emit an output frame (if any) followed by a result frame. A failure to
// set up scratch is reported as an error result rather than tearing down the
// connection.
func (r *runner) handleRun(ctx context.Context, rf *runnerproto.RunFrame) error {
	scratch, err := r.ensureScratch()
	if err != nil {
		return r.emitResult(runnerproto.ResultFrame{Exit: 1, Error: fmt.Sprintf("scratch setup failed: %v", err)})
	}
	defer r.resetScratch()

	outputPath := filepath.Join(scratch, outputFileName)
	// Pre-create $OUTPUT so a step that only appends still finds it, and so an
	// empty result is unambiguous (present-but-empty vs never-written).
	if f, cerr := os.Create(outputPath); cerr == nil {
		_ = f.Close()
	}

	// ctx carries the step's cancellation (wall-clock timeout OR an in-band signal
	// frame); runStep registered its cancel with the supervisor BEFORE this
	// goroutine was spawned, so a signal that lands during setup is never dropped.
	start := time.Now()
	var exitCode int
	var runErr string
	if rf.Entrypoint != "" {
		exitCode, runErr = r.runSubprocess(ctx, rf, scratch, outputPath)
	} else {
		exitCode, runErr = r.runInterp(ctx, rf, scratch, outputPath)
	}
	wallNS := time.Since(start).Nanoseconds()

	if vals := collectOutput(outputPath); len(vals) > 0 {
		if err := r.enc.Encode(runnerproto.Frame{
			Type:   runnerproto.FrameOutput,
			Output: &runnerproto.OutputFrame{Values: vals},
		}); err != nil {
			return err
		}
	}
	return r.emitResult(runnerproto.ResultFrame{Exit: exitCode, WallNS: wallNS, Error: runErr})
}

// runInterp runs the body through the mvdan/sh interpreter and returns the exit
// code and a runner-level error message (empty on normal completion, even for a
// non-zero exit). It never crashes: an upstream interp panic (#2429 class) is
// recovered into an error result with a fallback suggestion.
func (r *runner) runInterp(ctx context.Context, rf *runnerproto.RunFrame, scratch, outputPath string) (exitCode int, runErr string) {
	defer recoverInterpPanic(&exitCode, &runErr)

	prog, err := interpParser().Parse(bytes.NewReader(rf.Program), "")
	if err != nil {
		// The daemon preflights parsing before sending, so this is defensive; a
		// parse failure here is a runner-level error, not a step exit.
		return 2, fmt.Sprintf("parse: %v", err)
	}

	// CLOSED env (decision #17). interp.Env(nil) would fall back to os.Environ()
	// — the exact leak this guards against — so the env is ALWAYS explicit.
	env := expand.ListEnviron(buildStepEnv(rf.Env, outputPath, os.LookupEnv)...)

	// A child NEVER writes to the protocol stdout (fd 1): its stdout/stderr are
	// re-framed into io frames by ioWriter (chunked via SplitIO, b64-iff-binary),
	// and the fd-separation forge test (trace_test.go) proves a child that prints
	// a line shaped like a protocol frame arrives as an io payload, never parses
	// as a frame — security #4.
	//
	// The tracer mounts BOTH capture seams (decision #7): the ExecHandlers
	// middleware traces external commands (argv, real exit, monotonic dur_ns) and
	// CallHandler traces builtins, so the trace never goes blind on cd/export/etc.
	// A fresh tracer per step restarts Seq at 1. Process-group control (setpgid +
	// TERM→grace→KILL(-pgid)) lives in tracer.runExternal via the supervisor (U4).
	tr := newTracer(r.enc, r.supervisor())
	irunner, err := interp.New(
		interp.Env(env),
		interp.Dir(scratch),
		interp.StdIO(nil, r.ioWriter(1), r.ioWriter(2)),
		interp.ExecHandlers(tr.execMiddleware),
		interp.CallHandler(tr.callObserve),
	)
	if err != nil {
		return 2, fmt.Sprintf("interp init: %v", err)
	}

	// ctx is cancelled by EITHER the wall-clock timeout OR an in-band signal frame
	// (both via the cancel runStep registered with the supervisor); either drives
	// each running external command's process-group TERM→grace→KILL escalation (U4).
	rerr := irunner.Run(ctx, prog)
	if ctx.Err() != nil {
		// The wall-clock timeout or a signal frame cancelled the step; the process
		// group was torn down. Report the killed code (137) the daemon asserts,
		// regardless of whether interp surfaced ExitStatus(137) or the raw context
		// error (multi-statement bodies surface the latter).
		return killedExitCode, ""
	}
	if rerr == nil {
		return 0, ""
	}
	var status interp.ExitStatus
	if errors.As(rerr, &status) {
		return int(status), ""
	}
	// A non-exit error (an unsupported construct surfaced as an error): report a
	// non-zero exit with the message rather than crashing.
	return 1, rerr.Error()
}

// stepContext builds a step's cancellable context: a WithCancel base (so a signal
// frame can cancel it) wrapped in WithTimeout when the frame carries a wall-clock
// timeout. The returned cancel cancels the effective context either way, so the
// supervisor can drive process-group teardown from a signal frame just as the
// timeout does.
func stepContext(timeoutNS int64) (context.Context, context.CancelFunc) {
	base, baseCancel := context.WithCancel(context.Background())
	if timeoutNS <= 0 {
		return base, baseCancel
	}
	ctx, cancel := context.WithTimeout(base, time.Duration(timeoutNS))
	return ctx, func() {
		cancel()
		baseCancel()
	}
}

// runSubprocess runs the body under a non-bash entrypoint (python3, node, …) as
// a supervised subprocess with whole-step timing (constraint #4; per-command
// tracing is bash-only and stays in U3b). The body is fed on the child's stdin;
// stdout/stderr are re-framed into io frames — the child never inherits fd 1.
func (r *runner) runSubprocess(ctx context.Context, rf *runnerproto.RunFrame, scratch, outputPath string) (exitCode int, runErr string) {
	// ctx (cancelled by the wall-clock timeout OR an in-band signal frame, via the
	// cancel runStep registered) tears the subprocess group down (U4).
	cmd := exec.Command(rf.Entrypoint) // LookPath resolves the entrypoint; ctx-kill is the supervisor's job
	cmd.Dir = scratch
	cmd.Env = buildStepEnv(rf.Env, outputPath, os.LookupEnv) // SAME closed allowlist (decision #17)
	cmd.Stdin = bytes.NewReader(rf.Program)
	// Child stdout/stderr are re-framed into io frames; the real protocol fd 1 is
	// never inherited (security #4). Per-command tracing is bash-only (the interp
	// path owns the trace seams), so a non-bash subprocess step carries only
	// whole-step timing. The subprocess runs as a process-group leader (setpgid)
	// so a cancel kills its whole descendant tree (U4).
	cmd.Stdout = r.ioWriter(1)
	cmd.Stderr = r.ioWriter(2)

	err := r.supervisor().supervise(ctx, cmd)
	if ctx.Err() != nil {
		return killedExitCode, "" // timeout/signal → 137, same as the interp path
	}
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), ""
	}
	// Entrypoint not found or otherwise failed to start: non-zero + message.
	return 1, err.Error()
}

// handleFile stages a file frame's bytes into the current step's scratch dir,
// refusing any name that is not a bare filename (path-traversal guard, E&R
// file-frame row). A refused file is a diagnostic, not a connection teardown —
// the runner logs it and keeps serving.
func (r *runner) handleFile(ff *runnerproto.FileFrame) error {
	scratch, err := r.ensureScratch()
	if err != nil {
		return fmt.Errorf("scratch setup failed: %w", err)
	}
	dest, err := safeScratchPath(scratch, ff.Name)
	if err != nil {
		// Serialize through the supervisor so this diagnostic can never interleave
		// with an escalation note written by a prior step's still-draining
		// escalate goroutine (both target errOut).
		r.supervisor().logf("shellkit-runner: refused file %q: %v\n", ff.Name, err)
		return nil
	}
	if err := os.WriteFile(dest, ff.Data, 0o600); err != nil {
		return fmt.Errorf("stage file %q: %w", ff.Name, err)
	}
	return nil
}

// safeScratchPath resolves name to a path inside scratch, refusing traversal.
// A staged file MUST be a bare filename: absolute paths, any directory
// component, and "."/".." are rejected. It reduces name to filepath.Base and, as
// defense in depth, confirms the joined path cannot escape scratch.
func safeScratchPath(scratch, name string) (string, error) {
	if name == "" {
		return "", errors.New("empty file name")
	}
	if filepath.IsAbs(name) {
		return "", errors.New("absolute path not allowed")
	}
	base := filepath.Base(name)
	if base != name || base == "." || base == ".." || strings.ContainsRune(name, filepath.Separator) {
		return "", errors.New("path traversal not allowed")
	}
	dest := filepath.Join(scratch, base)
	rel, err := filepath.Rel(scratch, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path escapes scratch")
	}
	return dest, nil
}

// collectOutput reads the $OUTPUT scratch file and parses key=value lines,
// matching today's mcp.ParseOutputs exactly (first '=' at index > 0, both sides
// trimmed). A missing or empty file yields nil.
func collectOutput(outputPath string) map[string]string {
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			vals[key] = val
		}
	}
	return vals
}

// interpParser builds the parser for run bodies with the same options the
// daemon-side preflight uses (bash dialect, comments kept) so the runner parses
// a body identically to how it was screened.
func interpParser() *syntax.Parser {
	return syntax.NewParser(syntax.KeepComments(true), syntax.Variant(syntax.LangBash))
}

// recoverInterpPanic is deferred around interp.Run: an upstream panic (#2429
// class) becomes an exit-2 error result naming the "interp": false escape hatch,
// so the runner never crashes on a malformed body (plan decision #6).
func recoverInterpPanic(exitCode *int, runErr *string) {
	if rec := recover(); rec != nil {
		*exitCode = 2
		*runErr = fmt.Sprintf("interp panicked: %v — %s", rec, panicFallbackHint)
	}
}

// ioWriter returns an io.Writer that re-frames a child's output on file
// descriptor fd into io frames on the protocol stdout, chunked via SplitIO
// (≤MaxIOChunkBytes each, base64-iff-binary). The invariant it upholds is the
// fd-separation guarantee (security #4): a child's bytes are always re-framed,
// never written to the real protocol fd 1 — so nothing a child prints (even a
// line shaped like a protocol frame) can be mistaken for one. The forge test in
// trace_test.go proves this end to end.
func (r *runner) ioWriter(fd int) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		for _, fr := range runnerproto.SplitIO(fd, p) {
			if err := r.enc.Encode(runnerproto.Frame{Type: runnerproto.FrameIO, IO: &fr}); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	})
}

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// ensureScratch returns the current step's scratch dir, minting a fresh one
// (mktemp -d) on the step's first file/run frame so file staging and the run
// share one dir.
func (r *runner) ensureScratch() (string, error) {
	if r.scratch != "" {
		return r.scratch, nil
	}
	dir, err := os.MkdirTemp("", "shellkit-runner-*")
	if err != nil {
		return "", err
	}
	r.scratch = dir
	return dir, nil
}

// resetScratch removes the current step's scratch dir and clears it so the next
// step gets a fresh one. Safe to call when no scratch exists.
func (r *runner) resetScratch() {
	if r.scratch != "" {
		_ = os.RemoveAll(r.scratch)
		r.scratch = ""
	}
}

// emitResult encodes a result frame, closing a step.
func (r *runner) emitResult(res runnerproto.ResultFrame) error {
	return r.enc.Encode(runnerproto.Frame{Type: runnerproto.FrameResult, Result: &res})
}
