//go:build unix

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// shrinkWireDeathGrace temporarily lowers the wire-death grace so the watchdog
// test does not wait the production default; it returns a restore func.
func shrinkWireDeathGrace(d time.Duration) func() {
	old := wireDeathGrace
	wireDeathGrace = d
	return func() { wireDeathGrace = old }
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
