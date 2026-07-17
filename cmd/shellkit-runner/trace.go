package main

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
	"mvdan.cc/sh/v3/interp"
)

// tracer emits per-command trace frames onto the runner's protocol stdout,
// backing BOTH capture seams the mvdan/sh interpreter exposes (plan decision #7).
// Neither seam alone is enough:
//
//   - interp's ExecHandlers middleware fires ONLY for EXTERNAL commands; builtins
//     (cd, export, pwd, read, …) are dispatched internally and never reach it. A
//     trace built on ExecHandler alone therefore goes blind on builtins — the
//     exact failure a single-ExecHandler sandbox suffers (mvdan/sh #705 class).
//   - CallHandler runs post-expansion for EVERY simple command, so it can close
//     that gap and give builtins a trace line too.
//
// The two seams share one monotonic per-step sequence counter, so the merged
// stream on the wire is ordered/dedup-able by Seq regardless of which seam
// produced a line (the daemon renders both identically — U0 contract §1.1, no
// ext/builtin tag on the wire).
type tracer struct {
	enc *runnerproto.Encoder
	sup *supervisor // owns process-group setup + TERM→grace→KILL escalation (U4)
	seq int64       // per-step command counter; atomic — a backgrounded `cmd &` fires its handler in a goroutine
}

// newTracer returns a tracer emitting onto enc and running external commands
// under sup's process-group supervision. One tracer serves one step's interp.Run,
// so Seq restarts at 1 per step (runInterp builds a fresh tracer per run frame).
func newTracer(enc *runnerproto.Encoder, sup *supervisor) *tracer {
	return &tracer{enc: enc, sup: sup}
}

// nextSeq returns the next monotonic per-step sequence number. It is atomic
// because a backgrounded command (`cmd &`) runs its ExecHandler inside a
// goroutine that may overlap the foreground command's handler.
func (t *tracer) nextSeq() int {
	return int(atomic.AddInt64(&t.seq, 1))
}

// emit writes one trace frame best-effort. A trace-write failure is swallowed on
// purpose: the trace is diagnostic, never load-bearing (the io/output/result
// frames carry the real step outcome), and returning the error up the
// ExecHandler chain would be treated as a FATAL handler error that aborts the
// step mid-run. If the protocol stdout is genuinely broken, the next io/result
// write surfaces it on the real path instead.
func (t *tracer) emit(tf runnerproto.TraceFrame) {
	_ = t.enc.Encode(runnerproto.Frame{Type: runnerproto.FrameTrace, Trace: &tf})
}

// execMiddleware is the ExecHandlers middleware for EXTERNAL commands. It brackets
// the wrapped exec call with a cmd_start (argv) and a cmd_end (real per-command
// exit code + monotonic dur_ns measured AT the seam, not inferred from a
// neighboring trap line — U0 contract §1.1). It is non-fatal by construction: it
// returns the wrapped handler's own error unchanged, so a command's ExitStatus
// propagates to interp exactly as it would without tracing.
func (t *tracer) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	_ = next // U4 owns the exec (setpgid + escalation); interp's DefaultExecHandler is bypassed
	return func(ctx context.Context, args []string) error {
		seq := t.nextSeq()
		t.emit(runnerproto.TraceFrame{
			Event: runnerproto.TraceCmdStart,
			Seq:   seq,
			Argv:  args,
		})

		startedAt := time.Now()
		err := t.runExternal(ctx, args)
		durNS := time.Since(startedAt).Nanoseconds()

		t.emit(runnerproto.TraceFrame{
			Event: runnerproto.TraceCmdEnd,
			Seq:   seq,
			Exit:  exitStatusOf(err),
			DurNS: durNS,
		})
		return err
	}
}

// runExternal runs one external command under process-group supervision (U4).
// The command leads its own process group (setpgid), so a cancel — the run
// frame's wall-clock timeout OR an in-band signal frame — tears the whole
// descendant group down with TERM→grace(5s)→KILL(-pgid) (decisions #5/#19). It
// owns process construction at this single point, replacing interp's
// DefaultExecHandler; the cmd_start/cmd_end/timing/exit logic in execMiddleware
// stays untouched, so tracing and supervision evolve independently.
func (t *tracer) runExternal(ctx context.Context, args []string) error {
	return t.sup.runSupervised(ctx, args)
}

// callObserve is the CallHandler seam: it runs post-expansion for EVERY simple
// command (functions, builtins, and externals alike). It emits a trace line ONLY
// for builtins — externals are already covered by execMiddleware, so emitting
// here too would double-count them, and a builtin is the one command class that
// never reaches ExecHandler.
//
// A builtin gets a SINGLE cmd_start frame (argv + Seq): CallHandler fires BEFORE
// dispatch and cannot wrap the builtin's execution, so there is no seam to time
// its duration or read an exit code — interp reports a builtin's success/failure
// as a Go error on the Runner, not a wait-status. Emitting no cmd_end leaves the
// duration and exit unset, which maps cleanly to U0's TraceLine (DurationNS 0 =
// "builtin didn't report", ExitCode nil for builtins).
//
// CallHandler MUST be non-fatal: a non-nil error here halts the whole Runner
// (interp treats a CallHandler error as fatal), so it ALWAYS returns the args
// unchanged and a nil error — pure observation.
func (t *tracer) callObserve(_ context.Context, args []string) ([]string, error) {
	if len(args) > 0 && interp.IsBuiltin(args[0]) {
		t.emit(runnerproto.TraceFrame{
			Event: runnerproto.TraceCmdStart,
			Seq:   t.nextSeq(),
			Argv:  args,
		})
	}
	return args, nil
}

// exitStatusOf extracts a command's exit code from the error the exec chain
// returns: nil → 0, an interp.ExitStatus (including the 127 DefaultExecHandler
// reports for command-not-found) → its numeric value, any other non-nil error →
// 1 (a generic runner-level exec failure).
func exitStatusOf(err error) int {
	if err == nil {
		return 0
	}
	var status interp.ExitStatus
	if errors.As(err, &status) {
		return int(status)
	}
	return 1
}
