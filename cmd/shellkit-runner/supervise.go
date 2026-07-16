package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// killGracePeriod is the per-command TERM→KILL window (plan decision #19). When a
// step is cancelled — the run frame's wall-clock timeout OR an in-band signal
// frame — the runner SIGTERMs each running external command's process group,
// waits this long for it to unwind, then escalates to SIGKILL on the whole group.
const killGracePeriod = 5 * time.Second

// killedExitCode is the exit status the runner reports for a step torn down by a
// timeout or a signal frame. It is the conventional 128+SIGKILL(9) code the
// daemon asserts on the timeout/cancel path (rundaemon/client_test.go), reported
// deterministically regardless of which signal (TERM or KILL) actually landed the
// kill or which shape the interpreter surfaced the cancellation as.
const killedExitCode = 137

// supervisor is the shared, mutex-guarded handle between the concurrent frame
// reader (run/runStep) and the exec path (tracer.runExternal, runSubprocess).
// Each external command runs as a process-group leader (setpgid); the supervisor
// tracks the live groups so:
//
//   - a signal frame cancels the running step's context, driving each running
//     command's TERM→grace→KILL escalation (decisions #5/#19);
//   - the stdin-EOF watchdog SIGKILLs every still-registered group before the
//     runner exits on wire death.
//
// interp at the v3.13.1 pin signals only the DIRECT child, and the runner's own
// setpgid + kill(-pgid) is what reaches the whole descendant tree (decision #5).
type supervisor struct {
	mu     sync.Mutex
	groups map[int]struct{}   // pgids of external commands currently running
	cancel context.CancelFunc // cancels the running step's interp/subprocess context; nil when idle

	logMu  sync.Mutex // serializes errOut writes across the escalate goroutine and the frame loop
	errOut io.Writer  // diagnostics only (SIGKILL escalation notes); never the protocol stdout
}

// newSupervisor returns a supervisor writing diagnostics to errOut.
func newSupervisor(errOut io.Writer) *supervisor {
	return &supervisor{groups: make(map[int]struct{}), errOut: errOut}
}

// beginStep records the cancel func for the step about to run so a signal frame
// or the wire-death watchdog can reach it; endStep clears it.
func (s *supervisor) beginStep(cancel context.CancelFunc) {
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
}

func (s *supervisor) endStep() {
	s.mu.Lock()
	s.cancel = nil
	s.mu.Unlock()
}

func (s *supervisor) addGroup(pgid int) {
	s.mu.Lock()
	s.groups[pgid] = struct{}{}
	s.mu.Unlock()
}

func (s *supervisor) removeGroup(pgid int) {
	s.mu.Lock()
	delete(s.groups, pgid)
	s.mu.Unlock()
}

// cancelStep cancels the running step's context if one is registered. Invoked
// from the signal-frame branch and the wire-death watchdog: cancelling the
// context both aborts the interpreter at its next command boundary AND fires each
// running command's escalation (context.AfterFunc → TERM→grace→KILL).
func (s *supervisor) cancelStep() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// killAll SIGKILLs every still-registered process group immediately. The wire is
// gone, so there is no graceful TERM/grace window — tear the children down and let
// the runner exit. Best-effort: an already-reaped group (ESRCH) is ignored.
func (s *supervisor) killAll() {
	s.mu.Lock()
	pgids := make([]int, 0, len(s.groups))
	for pgid := range s.groups {
		pgids = append(pgids, pgid)
	}
	s.mu.Unlock()
	for _, pgid := range pgids {
		_ = killGroupHard(pgid)
	}
}

// supervise runs one already-configured command as a process-group leader and
// blocks until it exits. It sets setpgid, starts the command, registers its pgid,
// arms the cancellation escalation, waits, and deregisters — returning the raw
// cmd.Wait() error for the caller to map to an exit code. It is the single point
// where the runner constructs and reaps an external process (decision #5).
func (s *supervisor) supervise(ctx context.Context, cmd *exec.Cmd) error {
	// Never START a new process group under an already-cancelled step: on wire
	// death (cancelStep + killAll) or a late timeout, a command that has not yet
	// launched must not slip past the one-shot killAll snapshot and linger.
	if err := ctx.Err(); err != nil {
		return err
	}
	setpgidAttr(cmd) // SysProcAttr{Setpgid:true} on unix; no-op off unix
	if err := cmd.Start(); err != nil {
		return err
	}
	pgid := cmd.Process.Pid // setpgid(0,0) ⇒ the child leads a group whose id is its pid
	s.addGroup(pgid)
	defer s.removeGroup(pgid)

	// The escalation is armed on ctx cancellation. exited is closed the instant
	// Wait returns so a command that dies on TERM within the grace window is never
	// needlessly SIGKILLed, and the escalation goroutine can never outlive the step.
	exited := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { s.escalate(pgid, exited) })
	defer stop()

	err := cmd.Wait()
	close(exited)
	return err
}

// escalate drives the per-command TERM→grace→KILL sequence for one process group
// when the step's context is cancelled. It SIGTERMs the group, waits for either
// the command to exit (exited closed) or killGracePeriod to elapse, and escalates
// to SIGKILL on the whole group only if it ignored TERM. Diagnostics go to errOut,
// never the protocol stdout.
func (s *supervisor) escalate(pgid int, exited <-chan struct{}) {
	_ = termGroup(pgid)
	select {
	case <-exited:
		return
	case <-time.After(killGracePeriod):
	}
	s.logf("shellkit-runner: process group %d ignored SIGTERM after %s; escalating to SIGKILL\n", pgid, killGracePeriod)
	_ = killGroupHard(pgid)
}

// runSupervised is the external-command exec path mounted under interp's
// ExecHandlers middleware (tracer.runExternal). It fully replaces interp's
// DefaultExecHandler: it mirrors that handler's path lookup, env, stdio, and
// exit-code mapping, but runs the command as a process-group leader so a cancel
// tears down the whole descendant tree, and reports the killed code (137)
// deterministically on cancellation rather than surfacing the raw context error.
func (s *supervisor) runSupervised(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}
	cmd := &exec.Cmd{
		Path:   path,
		Args:   args,
		Env:    execEnv(hc.Env),
		Dir:    hc.Dir,
		Stdin:  hc.Stdin,
		Stdout: hc.Stdout,
		Stderr: hc.Stderr,
	}

	werr := s.supervise(ctx, cmd)
	if ctx.Err() != nil {
		// Cancelled by the wall-clock timeout or a signal frame: the group was
		// torn down. Report the killed code (137); returning ctx.Err() here would
		// make interp surface a generic exit 1 and lose the timeout signature.
		return interp.ExitStatus(killedExitCode)
	}

	switch e := werr.(type) {
	case nil:
		return nil
	case *exec.ExitError:
		if code, ok := signaledExit(e); ok {
			return interp.ExitStatus(code)
		}
		return interp.ExitStatus(e.ExitCode())
	default:
		// A start-time failure that slipped past LookPathDir (permissions, ETXTBSY):
		// mirror DefaultExecHandler's command-not-runnable path.
		fmt.Fprintf(hc.Stderr, "%v\n", werr)
		return interp.ExitStatus(127)
	}
}

// logf writes a diagnostic to errOut under a mutex so the escalate goroutine and
// the frame loop never interleave a line. errOut is stderr in production and a
// buffer in tests — neither is the protocol stdout.
func (s *supervisor) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.errOut, format, args...)
}

// execEnv renders an interp environment as the KEY=VALUE list an exec.Cmd needs,
// replicating interp's own unexported helper: only exported string variables are
// forwarded, matching what DefaultExecHandler would have passed the child.
func execEnv(env expand.Environ) []string {
	list := make([]string, 0, 64)
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
		return true
	})
	return list
}
