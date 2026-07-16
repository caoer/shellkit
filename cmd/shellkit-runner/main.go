// Command shellkit-runner is the remote execution runner that the shellkit
// daemon exec's over an ssh channel. It speaks the ndjson frame protocol defined
// in internal/runnerproto: it reads hello/run/file/signal frames on stdin, runs
// each step through the mvdan/sh interpreter (or a supervised subprocess for
// non-bash entrypoints), and emits io/output/result frames on stdout.
//
// stdout is EXCLUSIVELY protocol frames. Every diagnostic goes to stderr, which
// the daemon treats as free-form runner self-diagnostics, never frames.
//
// The frame loop lives here; U3b added the per-command trace dual-seam and
// io-chunk discipline (trace.go), and U4 added process-group supervision + kill
// (supervise.go) plus a concurrent frame reader so an in-band signal frame or
// wire death can be serviced WHILE a step runs.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// version is the runner binary's content-hash version, stamped at build time via
// `-ldflags "-X main.version=<hash>"` (U7). It rides in the hello ack so the
// daemon can detect a stale or mismatched binary.
var version = "dev"

func main() {
	// `shellkit-runner --version` prints the content-hash version and exits. The
	// daemon's bootstrap probe (internal/rundaemon) runs this to decide whether a
	// host already has the right binary (a version-match is a cache hit) and as the
	// post-push exec-test; it checks that the output CARRIES the expected version,
	// so a bare version line suffices.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			fmt.Println(version)
			return
		}
	}

	err := run(os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "shellkit-runner:", err)
	if errors.Is(err, errWireCut) {
		// Wire death mid-step: children were reaped; exit distinctly (U4 watchdog).
		os.Exit(2)
	}
	os.Exit(1)
}

// errWireCut is returned by run when stdin closes (EOF) while a step is in flight
// AND the step does not finish on its own within wireDeathGrace — a genuine ssh
// drop. main maps it to exit code 2; a clean EOF between steps (or a short step
// whose result is still in flight when a trailing EOF arrives) returns nil.
var errWireCut = errors.New("stdin closed mid-step; children reaped")

// errStreamEnd is runStep's internal signal that stdin reached EOF while a step
// was in flight AND the step then finished on its own — a clean end, not wire
// death. run maps it to a nil (exit 0) return.
var errStreamEnd = errors.New("stdin closed after step completed")

// wireDeathGrace is how long run waits for an in-flight step to finish on its own
// after stdin closes before concluding the wire is dead and tearing the step's
// children down. A trailing EOF after the last buffered frame (a short step whose
// result is merely still in flight) resolves well within it; a genuine ssh drop
// mid-step waits it out, then the children are killed and the runner exits 2. It
// is a var so tests can shrink it; production operation never closes stdin
// mid-step except on wire death, so the wait affects only that recovery path.
var wireDeathGrace = 2 * time.Second

// decodedFrame is one output of the concurrent frame reader: a decoded frame, or
// the terminal read error (io.EOF on a clean close, or a decode/protocol error).
type decodedFrame struct {
	frame runnerproto.Frame
	err   error
}

// run drives the frame loop. Frames are read from in, protocol frames are
// written to out (and ONLY protocol frames), and every diagnostic goes to
// errOut. It returns nil on a clean stdin close (EOF) and an error on a protocol
// violation or an unrecoverable write failure.
func run(in io.Reader, out, errOut io.Writer) error {
	dec := runnerproto.NewDecoder(in)
	enc := runnerproto.NewEncoder(out)

	r := &runner{enc: enc, errOut: errOut, sup: newSupervisor(errOut)}
	// A step's scratch dir may outlive its last run frame only if the stream ends
	// mid-step (files staged, no run); clean it up on exit either way.
	defer r.resetScratch()

	// Handshake: the daemon's opening hello may be preceded by login banner/MOTD
	// noise, which DecodeHello tolerates. After it returns, noise is a protocol
	// error.
	if _, err := dec.DecodeHello(); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if err := enc.Encode(helloAck()); err != nil {
		return fmt.Errorf("hello ack: %w", err)
	}

	// A single decoder goroutine feeds every frame (and the terminal read error)
	// to frames. The pre-U4 loop blocked in Decode while a step ran, so an in-band
	// signal or wire death could not be seen mid-step; reading on a goroutine lets
	// the main loop keep servicing the stream WHILE a step's goroutine runs. The
	// channel is unbuffered and has a single consumer at any instant (the main
	// loop between steps, or runStep during one), so frames are handed off in order.
	frames := make(chan decodedFrame)
	go func() {
		for {
			f, err := dec.Decode()
			frames <- decodedFrame{frame: f, err: err}
			if err != nil {
				return // terminal read: stop reading; consumers return without reading again
			}
		}
	}()

	for {
		df := <-frames
		if df.err != nil {
			if errors.Is(df.err, io.EOF) {
				// Clean stdin close BETWEEN steps: no step in flight, nothing to reap.
				return nil
			}
			return fmt.Errorf("decode: %w", df.err)
		}

		switch df.frame.Type {
		case runnerproto.FrameHello:
			// A second hello is unexpected after the handshake but harmless; re-ack
			// rather than tearing the connection down.
			if err := enc.Encode(helloAck()); err != nil {
				return fmt.Errorf("hello ack: %w", err)
			}
		case runnerproto.FrameRun:
			switch err := r.runStep(df.frame.Run, frames); {
			case err == nil:
				// Step finished; stream still open — read the next frame.
			case errors.Is(err, errStreamEnd):
				// Trailing EOF consumed during the step, which then finished cleanly.
				return nil
			default:
				return err // errWireCut, a decode error, or a fatal write failure
			}
		case runnerproto.FrameFile:
			if err := r.handleFile(df.frame.File); err != nil {
				return err
			}
		case runnerproto.FrameSignal:
			// Between steps a signal frame has no running process group to reach.
			r.sup.logf("shellkit-runner: signal frame %q with no step running\n", df.frame.Signal.Signal)
		default:
			return fmt.Errorf("unexpected %q frame on stdin", df.frame.Type)
		}
	}
}

// runStep executes one run frame while the concurrent frame reader keeps
// delivering frames, so an in-band signal can cancel the step and wire death can
// tear its children down. It runs the step on its own goroutine and pumps frames
// until the step finishes:
//
//   - a signal frame (any of TERM/KILL/INT) cancels the step's context, driving
//     each running command's process-group TERM→grace→KILL escalation (U4);
//   - stdin EOF mid-step gives the step wireDeathGrace to finish on its own (a
//     trailing EOF after the last buffered frame); if it does not, the wire is
//     dead — every in-flight process group is SIGKILLed and errWireCut returned;
//   - a decode/protocol error mid-step tears the children down and propagates.
//
// It returns nil when the step completes with the stream still open (the caller
// reads the next frame), errStreamEnd when a trailing EOF was consumed and the
// step then finished cleanly, or a terminal error otherwise.
func (r *runner) runStep(rf *runnerproto.RunFrame, frames <-chan decodedFrame) error {
	// Register the step's cancel BEFORE the step goroutine is spawned or any frame
	// is pumped, so a signal frame that arrives during step setup (interp parse,
	// interp.New, exec setup) reaches a LIVE cancel, never a nil one. Registering
	// inside the step goroutine (as an earlier draft did) left a window where
	// runStep could dequeue a signal against an unregistered cancel and drop it —
	// the step would then run to completion and wrongly report exit 0.
	ctx, cancel := stepContext(rf.TimeoutNS)
	defer cancel()
	r.supervisor().beginStep(cancel)
	defer r.supervisor().endStep()

	done := make(chan error, 1)
	go func() { done <- r.handleRun(ctx, rf) }()

	for {
		select {
		case err := <-done:
			return err
		case df := <-frames:
			if df.err != nil {
				if errors.Is(df.err, io.EOF) {
					// stdin closed mid-step. Let a short step finish on its own (a
					// trailing EOF merely signalling "no more frames"); if it hangs,
					// the wire is gone — tear the children down and exit non-zero.
					select {
					case err := <-done:
						if err != nil {
							return err
						}
						return errStreamEnd
					case <-time.After(wireDeathGrace):
						r.sup.cancelStep()
						r.sup.killAll()
						<-done
						return errWireCut
					}
				}
				// A decode/protocol error mid-step: tear children down and propagate.
				r.sup.cancelStep()
				r.sup.killAll()
				<-done
				return fmt.Errorf("decode: %w", df.err)
			}
			switch df.frame.Type {
			case runnerproto.FrameSignal:
				// In-band cancel: cancel the step's context, driving each running
				// command's process-group escalation. Any signal name cancels.
				r.sup.cancelStep()
			case runnerproto.FrameHello:
				if err := r.enc.Encode(helloAck()); err != nil {
					r.sup.cancelStep()
					<-done
					return fmt.Errorf("hello ack: %w", err)
				}
			default:
				// The daemon is request/response, so a run/file frame mid-step is a
				// protocol slip; note it and keep waiting for this step to finish.
				r.sup.logf("shellkit-runner: %q frame arrived mid-step; ignored\n", df.frame.Type)
			}
		}
	}
}

// helloAck builds the runner's hello ack, reporting the protocol version and the
// binary's platform + content-hash version so the daemon can verify the
// bootstrapped binary matches what it pushed.
func helloAck() runnerproto.Frame {
	return runnerproto.Frame{
		Type: runnerproto.FrameHello,
		Hello: &runnerproto.HelloFrame{
			Proto:   runnerproto.ProtoVersion,
			Role:    runnerproto.RoleRunner,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: version,
		},
	}
}

// runner holds the per-connection execution state: the frame encoder, the
// diagnostic sink, the process supervisor, and the current step's scratch
// directory. One runner serves the whole ssh exec channel; a fresh scratch dir is
// minted per step.
type runner struct {
	enc     *runnerproto.Encoder
	errOut  io.Writer
	sup     *supervisor // process-group setup + cancellation (U4); lazily created for direct-call tests
	scratch string      // current step's scratch dir; "" until the step's first file/run frame
}

// supervisor returns the runner's process supervisor, lazily binding one to
// errOut on first use so a directly constructed runner (unit tests calling
// runInterp/runSubprocess) is never nil. run() always sets sup up front, so the
// lazy path is single-goroutine and race-free.
func (r *runner) supervisor() *supervisor {
	if r.sup == nil {
		r.sup = newSupervisor(r.errOut)
	}
	return r.sup
}
