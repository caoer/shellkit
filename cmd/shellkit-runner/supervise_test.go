//go:build unix

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// waitForPid polls a pid file the step body writes and returns the recorded pid,
// so the test can prove the real OS process(es) exist before it cancels the step.
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
					return pid
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid file %s never populated", path)
	return 0
}

// waitForDead polls until kill(pid, 0) reports the process is gone (ESRCH), the
// direct evidence that a process-group kill actually reaped it.
func waitForDead(t *testing.T, label string, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil { // ESRCH once the process is gone (or EPERM if recycled)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s process %d still alive after kill", label, pid)
}

// killGroupCleanup best-effort SIGKILLs a process group so a failed assertion
// never leaks a 300s sleep past the test.
func killGroupCleanup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// TestSupervise_TimeoutExit137 proves the wall-clock timeout path: a hung
// external command (`sleep 300`) under a short timeout is torn down and the step
// reports exit 137 — the signature the daemon asserts — and the escalation kills
// it near the deadline rather than waiting out the full TERM→KILL grace.
func TestSupervise_TimeoutExit137(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)
	writeFile(t, outputPath, "")

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{
		Program:   []byte("sleep 300\n"),
		TimeoutNS: (100 * time.Millisecond).Nanoseconds(),
	}

	ctx, cancel := stepContext(rf.TimeoutNS)
	defer cancel()
	start := time.Now()
	code, runErr := r.runInterp(ctx, rf, scratch, outputPath)
	elapsed := time.Since(start)

	if code != killedExitCode {
		t.Fatalf("timeout exit = %d, want %d (runErr=%q)", code, killedExitCode, runErr)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s: SIGTERM did not reach the process group near the deadline", elapsed)
	}
}

// TestSupervise_TimeoutExit137_MultiStatement guards the multi-statement body
// shape, where the interpreter surfaces the raw context error (not ExitStatus)
// after the killed command — the runInterp ctx.Err() override must still report 137.
func TestSupervise_TimeoutExit137_MultiStatement(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)
	writeFile(t, outputPath, "")

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{
		Program:   []byte("sleep 300\necho after\n"),
		TimeoutNS: (100 * time.Millisecond).Nanoseconds(),
	}

	ctx, cancel := stepContext(rf.TimeoutNS)
	defer cancel()
	code, runErr := r.runInterp(ctx, rf, scratch, outputPath)
	if code != killedExitCode {
		t.Fatalf("multi-statement timeout exit = %d, want %d (runErr=%q)", code, killedExitCode, runErr)
	}
}

// TestSupervise_SignalKillsProcessGroup is the core anti-orphan proof (decision
// #5): a step forks an external `sh` (a process-group leader via setpgid) that in
// turn forks a `sleep` GRANDCHILD in the same group. An in-band signal frame must
// tear the WHOLE group down — leader AND grandchild — which a direct-child-only
// signal (interp's default at the pin) could not do. The step reports exit 137.
func TestSupervise_SignalKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	leaderFile := filepath.Join(dir, "leader.pid")
	grandFile := filepath.Join(dir, "grand.pid")

	// The `sh -c '…' &` is one external command: setpgid makes it a group leader,
	// pgid == its pid. Inside REAL sh, $$ is the leader's pid and $! is the real OS
	// pid of the backgrounded `sleep` grandchild, both in the leader's group.
	body := fmt.Sprintf(
		"sh -c 'echo $$ > %s; sleep 300 & echo $! > %s; wait' &\nwait\n",
		leaderFile, grandFile)

	pr, pw := io.Pipe()
	defer pw.Close()
	var stdout bytes.Buffer
	var errOut bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(pr, &stdout, &errOut) }()

	enc := runnerproto.NewEncoder(pw)
	mustEncode(t, enc, daemonHello())
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}})

	leaderPid := waitForPid(t, leaderFile)
	grandPid := waitForPid(t, grandFile)
	t.Cleanup(func() { killGroupCleanup(leaderPid) })

	// In-band cancel.
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameSignal, Signal: &runnerproto.SignalFrame{Signal: runnerproto.SignalTERM}})

	// The whole group — leader AND the grandchild sleep — must be gone.
	waitForDead(t, "group leader", leaderPid)
	waitForDead(t, "grandchild", grandPid)

	// Let the runner see EOF and return, then confirm the step reported exit 137.
	pw.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("run returned %v, want nil after a graceful in-band cancel", err)
	}
	if exit, ok := lastResultExit(t, &stdout); !ok || exit != killedExitCode {
		t.Fatalf("result exit = %d (present=%v), want %d", exit, ok, killedExitCode)
	}
}

// TestSupervise_SignalDuringSetupNotDropped is the regression guard for the
// setup-window race: a signal frame sent IMMEDIATELY after the run frame — before
// the step has spawned any external command — must still cancel the step and
// report exit 137. Earlier the cancel was registered inside the step goroutine
// after interp setup, so a signal arriving in that window found a nil cancel, was
// silently dropped, and the step ran to completion reporting exit 0. This test
// deliberately does NOT wait for a pid file (which would hide the window by
// guaranteeing setup is already past); it races the signal against setup.
func TestSupervise_SignalDuringSetupNotDropped(t *testing.T) {
	// No wall-clock timeout: the ONLY thing that can stop the sleep is the signal.
	// If the signal were dropped, the step would run the full sleep and report 0.
	body := "sleep 3\n"

	pr, pw := io.Pipe()
	defer pw.Close()
	var stdout bytes.Buffer
	var errOut bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(pr, &stdout, &errOut) }()

	enc := runnerproto.NewEncoder(pw)
	mustEncode(t, enc, daemonHello())
	start := time.Now()
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}})
	// Back-to-back with the run frame — no synchronization on the step's progress.
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameSignal, Signal: &runnerproto.SignalFrame{Signal: runnerproto.SignalTERM}})

	pw.Close()
	if err := <-runDone; err != nil {
		t.Fatalf("run returned %v, want nil after an in-band cancel (a dropped signal ⇒ EOF ⇒ errWireCut)", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("step ran %s: the signal was dropped and sleep ran toward completion", elapsed)
	}
	if exit, ok := lastResultExit(t, &stdout); !ok || exit != killedExitCode {
		t.Fatalf("result exit = %d (present=%v), want %d — signal frame was dropped during setup", exit, ok, killedExitCode)
	}
}

// TestSupervise_StdinEOFReapsChildren is the wire-death watchdog: when stdin
// closes while a step's child is still running, the runner SIGKILLs the child
// process group and exits non-zero (errWireCut ⇒ exit code 2) instead of
// orphaning a 300s sleep on the remote host.
func TestSupervise_StdinEOFReapsChildren(t *testing.T) {
	restore := shrinkWireDeathGrace(150 * time.Millisecond)
	defer restore()

	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The backgrounded external `sh` leads a group; $$ is its real pid, and it
	// forks a `sleep` child in the same group.
	body := fmt.Sprintf("sh -c 'echo $$ > %s; sleep 300' &\nwait\n", childFile)

	pr, pw := io.Pipe()
	defer pw.Close()
	var stdout bytes.Buffer
	var errOut bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(pr, &stdout, &errOut) }()

	enc := runnerproto.NewEncoder(pw)
	mustEncode(t, enc, daemonHello())
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}})

	childPid := waitForPid(t, childFile)
	t.Cleanup(func() { killGroupCleanup(childPid) })

	// Wire death: stdin closes mid-step.
	pw.Close()

	select {
	case err := <-runDone:
		if err == nil || !errors.Is(err, errWireCut) {
			t.Fatalf("run returned %v, want errWireCut", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after wire death")
	}
	waitForDead(t, "orphaned child", childPid)
}

// TestRun_BackgroundJobNoWaitReaped is the #7 anti-orphan proof for the clean-EOF
// path: a step body backgrounds an external command with NO `wait`, so runStep
// returns after the result frame while the bg group is still alive. The step's
// own context cancel escalates TERM→grace→KILL on the group, but the runner exits
// on the trailing clean EOF long before the 5s KILL fires — so a bg process that
// IGNORES SIGTERM (the realistic hard case: a service that traps signals) would,
// before the fix, survive as an orphan on the remote host after the runner exits
// and scratch is cleaned. reapAll's SIGKILL on run() exit (which cannot be
// trapped) must reap it. The foreground `sleep` gives the bg command time to
// launch and register with the supervisor before the script ends — so THIS test
// covers an ALREADY-registered orphan; the async-registration window (a group that
// registers only after run's first reap snapshot) is modelled separately by
// TestReapAll_KillsLateRegisteredGroup and TestRun_BackgroundOrphanReapedThroughRun.
func TestRun_BackgroundJobNoWaitReaped(t *testing.T) {
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The bg sh traps and ignores TERM/INT, records its own pid (the group leader,
	// setpgid via the supervisor), then sleeps. No `wait` follows the `&`, so the
	// script's foreground finishes (after a short settle sleep) while the bg group
	// is still alive AND immune to the escalation's SIGTERM.
	body := fmt.Sprintf("sh -c 'trap \"\" TERM INT; echo $$ > %s; sleep 600' &\nsleep 1\n", childFile)

	pr, pw := io.Pipe()
	defer pw.Close()
	// The reaped bg command's supervise goroutine may still emit a trailing trace
	// frame onto stdout AFTER run() returns (the process would exit here in
	// production; in-test it lingers), so guard the buffer against a concurrent
	// read/write race with the test's result decode.
	stdout := &syncBuffer{}
	var errOut bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(pr, stdout, &errOut) }()

	enc := runnerproto.NewEncoder(pw)
	mustEncode(t, enc, daemonHello())
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}})

	// Confirm the bg process actually started (and was registered by the
	// supervisor) before we trigger the clean EOF.
	childPid := waitForPid(t, childFile)
	t.Cleanup(func() { killGroupCleanup(childPid) })

	// Clean EOF AFTER the step's result frame: no wire death, the step already
	// finished. run() must still reap the surviving bg group on exit.
	pw.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run returned %v, want nil on a clean EOF after the step completed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after clean EOF")
	}
	// The backgrounded, TERM-immune process must be gone: killAll SIGKILLed its
	// group on run exit.
	waitForDead(t, "orphaned background job", childPid)

	// The step must have reported success (exit 0) — the orphan reaping is a
	// SEPARATE concern from the step's own result. Snapshot the buffer under its
	// lock before decoding.
	if exit, ok := lastResultExit(t, bytes.NewBuffer(stdout.bytes())); !ok || exit != 0 {
		t.Fatalf("result exit = %d (present=%v), want 0", exit, ok)
	}
}

// startLateGroup launches a TERM-immune process-group leader and, after regDelay,
// registers its pgid with sup — modelling mvdan/sh's async-registration window: the
// process is ALIVE (cmd.Start done) but its pgid lands in sup.groups only after a
// production-like lag, exactly like a background command that has exec'd but not yet
// reached the supervise.addGroup seam. Crucially there is NO barrier that forces
// registration before the reaper's first poll — the lag is a real time.Sleep, so
// the test races the reaper against the registration the way production does. The
// leader traps TERM/INT, so only SIGKILL (which reapAll delivers) can reap it. It
// returns the pgid (== the leader's pid via setpgid, so the test can prove it dead
// WITHOUT a pid-file write that a fast SIGKILL could race) and a channel closed once
// the leader's goroutine has deregistered — i.e. the process was reaped.
func startLateGroup(t *testing.T, sup *supervisor, regDelay time.Duration) (pgid int, reaped <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "trap '' TERM INT; sleep 600")
	setpgidAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start late group leader: %v", err)
	}
	pgid = cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(regDelay) // the real async-registration lag; no synchronization
		sup.addGroup(pgid)
		defer sup.removeGroup(pgid)
		_ = cmd.Wait() // returns once the group is SIGKILLed
	}()
	return pgid, done
}

// TestReapAll_KillsLateRegisteredGroup is the async-registration RACE model at the
// supervisor seam, with a NEGATIVE CONTROL proving the race is real. The reviewer
// measured a backgrounded command's pgid appearing ~300ms AFTER interp.Run
// returned; the existing supervise tests only ever kill ALREADY-registered groups
// (they wait for a pid file before tearing down), so none of them exercise this
// window. Here the group registers ~200ms AFTER the reaper begins — a real
// time.Sleep lag with NO barrier that guarantees registration before the first
// poll, so the reaper genuinely races the registration:
//
//   - NEGATIVE CONTROL: the OLD teardown logic — a one-shot killAll, or the prior
//     early-exit that returned after two empty ~20ms polls — takes its snapshot(s)
//     in the first ~40ms, sees the still-empty group set, and returns before the
//     ~200ms registration. The live TERM-immune orphan survives. earlyExitReap
//     reproduces that exact old logic and the control asserts it leaks.
//   - reapAll (background-gated) polls its FULL window with no early-exit, so it is
//     still polling when the group registers at ~200ms and SIGKILLs it — the orphan
//     dies. Same window, same lag; only the early-exit removal changes the outcome.
func TestReapAll_KillsLateRegisteredGroup(t *testing.T) {
	var buf bytes.Buffer
	const regDelay = 200 * time.Millisecond // within the ~300ms measured worst case

	// --- Negative control: the OLD early-exit logic MISSES the late group. ---
	ctlSup := newSupervisor(&buf)
	shrinkReap(ctlSup, 20*time.Millisecond, 1500*time.Millisecond)
	ctlSup.markBackgroundLaunched() // a `&` was seen; even so the OLD logic bailed early
	ctlPid, ctlReaped := startLateGroup(t, ctlSup, regDelay)
	t.Cleanup(func() { killGroupCleanup(ctlPid) })

	// Run the OLD algorithm verbatim. It returns in ~40ms (two empty polls) — long
	// before the 200ms registration — so the orphan is still alive when it returns.
	earlyExitReap(ctlSup)
	if syscall.Kill(ctlPid, 0) != nil {
		t.Fatalf("control: old early-exit reap unexpectedly reaped pid %d — the race is not being modelled", ctlPid)
	}
	// Let the registration land, then clean the control orphan up ourselves.
	waitForGroups(t, ctlSup, 1)
	_ = ctlSup.killAll()
	<-ctlReaped

	// --- reapAll: the SAME window and lag, but polling the full window reaps it. ---
	sup := newSupervisor(&buf)
	shrinkReap(sup, 20*time.Millisecond, 1500*time.Millisecond)
	sup.markBackgroundLaunched() // the AST walk sets this in production before interp.Run
	pgid, reaped := startLateGroup(t, sup, regDelay)
	t.Cleanup(func() { killGroupCleanup(pgid) })

	done := make(chan struct{})
	go func() { defer close(done); sup.reapAll() }()

	// The group registers at ~200ms; reapAll polls until ~1.5s, so it observes and
	// SIGKILLs the group well within its window. If reapAll early-exited on an empty
	// snapshot (the old bug), this TERM-immune orphan would survive and this fails.
	waitForDead(t, "late-registered orphan", pgid)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reapAll did not return within its bounded window")
	}
	<-reaped
}

// earlyExitReap reproduces the OLD (pre-fix) run-exit reap verbatim: a one-shot
// killAll pre-seeding emptyStreak, then a bounded poll that returns after two
// consecutive empty polls. It exists ONLY as the negative control in
// TestReapAll_KillsLateRegisteredGroup: it proves the shipped reapAll's behavior
// (full-window poll) is what reaps the late orphan, by showing this early-exit logic
// returns ~40ms in and leaks it.
func earlyExitReap(s *supervisor) {
	killed := s.killAll()
	deadline := time.Now().Add(s.reapWindow)
	emptyStreak := 0
	if killed == 0 {
		emptyStreak = 1
	}
	for time.Now().Before(deadline) {
		time.Sleep(s.reapPollInterval)
		if n := s.killAll(); n > 0 {
			emptyStreak = 0
			continue
		}
		if emptyStreak++; emptyStreak >= 2 {
			return
		}
	}
}

// TestReapAll_NoBackgroundFastPath proves the common case stays free: a supervisor
// on which no `&` was ever seen reaps in a single snapshot and returns effectively
// instantly — it must NOT poll the (1s default) window. A long window with a fast
// return is the whole point of gating the poll on background detection.
func TestReapAll_NoBackgroundFastPath(t *testing.T) {
	var buf bytes.Buffer
	sup := newSupervisor(&buf)
	// A pathologically long window: if the fast path ever polled it, this test would
	// take seconds. It must not, because no background command was launched.
	shrinkReap(sup, 20*time.Millisecond, 30*time.Second)

	start := time.Now()
	sup.reapAll()
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("no-background reapAll took %s: it polled the window instead of a single snapshot", elapsed)
	}
}

// waitForGroups polls until the supervisor tracks exactly want process groups, so
// a test can synchronize on a registration landing without racing an internal map.
func waitForGroups(t *testing.T, s *supervisor, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.groups)
		s.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("supervisor never reached %d tracked groups", want)
}

// TestRun_BackgroundOrphanReapedThroughRun drives the orphan reap through the FULL
// run() frame loop, not just the supervisor seam. The body backgrounds a
// TERM-immune external command with NO `wait`; the foreground blocks only until the
// bg command has registered, so by the time interp.Run returns the bg command is a
// LIVE, registered, signal-immune process group — the realistic hard case (a
// service that traps signals) that the step's own context-cancel escalation
// (TERM→grace) cannot reap before run() exits on the trailing clean EOF. run()'s
// deferred reapAll must SIGKILL it: no descendant may survive after run() returns.
//
// This complements TestReapAll_KillsLateRegisteredGroup (which isolates the
// async-registration window at the seam): here the whole clean-EOF exit path is
// exercised, proving reapAll is actually wired into run()'s teardown.
func TestRun_BackgroundOrphanReapedThroughRun(t *testing.T) {
	// Production reap defaults apply (run() owns the supervisor, so the per-supervisor
	// shrink hook isn't reachable here). The body contains a `&`, so runInterp's AST
	// walk sets sawBackground before interp.Run and reapAll polls its window. The
	// orphan is registered BEFORE run-exit, so reapAll's FIRST killAll already reaps
	// it; the remaining window polls only find an empty set and the deadline (1s
	// default) bounds them — the runner still exits promptly. Shrink only the
	// wire-death grace so the trailing clean EOF resolves quickly.
	restoreGrace := shrinkWireDeathGrace(300 * time.Millisecond)
	defer restoreGrace()

	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The bg sh traps TERM/INT, records its pid (group leader via setpgid), then
	// sleeps 600s. The foreground spins until the pid file exists, guaranteeing the
	// bg group is registered and alive when interp.Run returns — but no `wait`, so
	// the group outlives the step. Only reapAll's SIGKILL (untrappable) can reap it.
	body := fmt.Sprintf(
		"sh -c 'trap \"\" TERM INT; echo $$ > %s; sleep 600' &\nwhile [ ! -s %s ]; do :; done\n",
		childFile, childFile)

	pr, pw := io.Pipe()
	defer pw.Close()
	// A lingering (soon-reaped) bg goroutine may flush a trailing trace frame after
	// run() returns; guard the buffer against the test's concurrent result decode.
	stdout := &syncBuffer{}
	var errOut bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- run(pr, stdout, &errOut) }()

	enc := runnerproto.NewEncoder(pw)
	mustEncode(t, enc, daemonHello())
	mustEncode(t, enc, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte(body)}})

	childPid := waitForPid(t, childFile)
	t.Cleanup(func() { killGroupCleanup(childPid) })

	// Clean EOF AFTER the step's result frame: no wire death, the step finished.
	// run() must still reap the surviving bg group on exit.
	pw.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run returned %v, want nil on a clean EOF after the step completed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after clean EOF")
	}

	// The backgrounded, TERM-immune process must be gone: reapAll SIGKILLed it.
	waitForDead(t, "background orphan reaped through run()", childPid)

	// The step itself reported success — orphan reaping is separate from the result.
	if exit, ok := lastResultExit(t, bytes.NewBuffer(stdout.bytes())); !ok || exit != 0 {
		t.Fatalf("result exit = %d (present=%v), want 0", exit, ok)
	}
}

// syncBuffer is a minimal mutex-guarded bytes.Buffer so a test can read the
// runner's stdout while a lingering (already-reaped) background goroutine may
// still be flushing a trailing frame.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// shrinkWireDeathGrace temporarily lowers the wire-death grace so the watchdog
// test does not wait the production default; it returns a restore func.
func shrinkWireDeathGrace(d time.Duration) func() {
	old := wireDeathGrace
	wireDeathGrace = d
	return func() { wireDeathGrace = old }
}

// shrinkReap sets a supervisor's run-exit reap cadence/ceiling to a short-but-real
// window (long enough to cover an injected registration lag, short enough not to
// slow the suite). Per-supervisor, so it races nothing: the fields are set before
// any reapAll goroutine can read them.
func shrinkReap(s *supervisor, interval, window time.Duration) {
	s.reapPollInterval = interval
	s.reapWindow = window
}

func mustEncode(t *testing.T, enc *runnerproto.Encoder, f runnerproto.Frame) {
	t.Helper()
	if err := enc.Encode(f); err != nil {
		t.Fatalf("encode %s frame: %v", f.Type, err)
	}
}

// lastResultExit decodes every frame written so far and returns the exit code of
// the last result frame (present=false if none). Call only after run has returned.
func lastResultExit(t *testing.T, buf *bytes.Buffer) (exit int, present bool) {
	t.Helper()
	for _, f := range decodeFrames(t, buf) {
		if f.Type == runnerproto.FrameResult {
			exit, present = f.Result.Exit, true
		}
	}
	return exit, present
}
