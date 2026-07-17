package rundaemon

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// fakeRunner is a test double that speaks the runner side of the protocol over
// two in-process pipes — no real ssh. A background goroutine reads the daemon's
// stdin frames (capturing file frames, the run frame, and any signal frames);
// the test's script closure emits stdout frames in response.
type fakeRunner struct {
	dec  *runnerproto.Decoder
	enc  *runnerproto.Encoder
	inR  *io.PipeReader
	outW *io.PipeWriter

	proto int // protocol version to advertise in the hello ack (0 → current)
	// role and version override the hello ack for negative handshake tests. Empty
	// role → RoleRunner; version is sent verbatim (default "testhash").
	role    string
	version string

	mu         sync.Mutex
	gotFiles   []runnerproto.FileFrame
	gotSignals []string

	runCh       chan runnerproto.RunFrame
	stdinClosed chan struct{}
}

// newFakeConn wires a Client to a fakeRunner through two io.Pipes and returns
// both. stderr is nil (the fake speaks only frames).
func newFakeConn() (*Client, *fakeRunner) {
	inR, inW := io.Pipe()   // daemon → runner
	outR, outW := io.Pipe() // runner → daemon
	c := NewClient(inW, outR, nil)
	f := &fakeRunner{
		dec:         runnerproto.NewDecoder(inR),
		enc:         runnerproto.NewEncoder(outW),
		inR:         inR,
		outW:        outW,
		runCh:       make(chan runnerproto.RunFrame, 1),
		stdinClosed: make(chan struct{}),
	}
	return c, f
}

// serve runs the fake: handshake, then dispatch stdin frames in the background
// while script emits the stdout side. script runs once the run frame arrives.
func (f *fakeRunner) serve(t *testing.T, script func(run runnerproto.RunFrame)) {
	t.Helper()
	hello, err := f.dec.DecodeHello()
	if err != nil {
		t.Errorf("fake: decode daemon hello: %v", err)
		return
	}
	if hello.Role != runnerproto.RoleDaemon {
		t.Errorf("fake: daemon hello role = %q, want %q", hello.Role, runnerproto.RoleDaemon)
	}
	proto := f.proto
	if proto == 0 {
		proto = runnerproto.ProtoVersion
	}
	role := f.role
	if role == "" {
		role = runnerproto.RoleRunner
	}
	version := f.version
	if version == "" {
		version = "testhash"
	}
	if err := f.enc.Encode(runnerproto.Frame{
		Type: runnerproto.FrameHello,
		Hello: &runnerproto.HelloFrame{
			Proto: proto, Role: role,
			OS: "linux", Arch: "amd64", Version: version,
		},
	}); err != nil {
		return // client aborted (e.g. proto mismatch closes the pipe)
	}

	go f.readStdin()

	select {
	case run := <-f.runCh:
		if script != nil {
			script(run)
		}
	case <-f.stdinClosed:
		// client aborted before sending a run frame (e.g. proto mismatch).
	}
}

// readStdin drains all daemon→runner frames after the handshake, capturing
// files, the run frame, and signals until stdin closes.
func (f *fakeRunner) readStdin() {
	for {
		fr, err := f.dec.Decode()
		if err != nil {
			close(f.stdinClosed)
			return
		}
		switch fr.Type {
		case runnerproto.FrameFile:
			f.mu.Lock()
			f.gotFiles = append(f.gotFiles, *fr.File)
			f.mu.Unlock()
		case runnerproto.FrameRun:
			f.runCh <- *fr.Run
		case runnerproto.FrameSignal:
			f.mu.Lock()
			f.gotSignals = append(f.gotSignals, fr.Signal.Signal)
			f.mu.Unlock()
		}
	}
}

func (f *fakeRunner) signals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotSignals...)
}

func (f *fakeRunner) files() []runnerproto.FileFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runnerproto.FileFrame(nil), f.gotFiles...)
}

// emit is a convenience for the script to write one stdout frame.
func (f *fakeRunner) emit(t *testing.T, fr runnerproto.Frame) {
	t.Helper()
	if err := f.enc.Encode(fr); err != nil {
		t.Errorf("fake: emit %s: %v", fr.Type, err)
	}
}

func traceStart(seq int, argv ...string) runnerproto.Frame {
	return runnerproto.Frame{Type: runnerproto.FrameTrace, Trace: &runnerproto.TraceFrame{
		Event: runnerproto.TraceCmdStart, Seq: seq, Argv: argv,
	}}
}

func traceEnd(seq, exit int, durNS int64) runnerproto.Frame {
	return runnerproto.Frame{Type: runnerproto.FrameTrace, Trace: &runnerproto.TraceFrame{
		Event: runnerproto.TraceCmdEnd, Seq: seq, Exit: exit, DurNS: durNS,
	}}
}

func ioFrame(fd int, s string) runnerproto.Frame {
	return runnerproto.Frame{Type: runnerproto.FrameIO, IO: &runnerproto.IOFrame{FD: fd, Data: s}}
}

func resultFrame(exit int, wallNS int64, errStr string) runnerproto.Frame {
	return runnerproto.Frame{Type: runnerproto.FrameResult, Result: &runnerproto.ResultFrame{
		Exit: exit, WallNS: wallNS, Error: errStr,
	}}
}

func TestRunStep_HappyPath(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		if string(run.Program) != "git pull --ff-only\n" {
			t.Errorf("run program = %q", run.Program)
		}
		f.emit(t, traceStart(1, "git", "pull", "--ff-only"))
		f.emit(t, traceEnd(1, 0, 842_000_000))
		f.emit(t, ioFrame(1, "Already up to date.\n"))
		f.emit(t, runnerproto.Frame{Type: runnerproto.FrameOutput, Output: &runnerproto.OutputFrame{
			Values: map[string]string{"status": "ok"},
		}})
		f.emit(t, resultFrame(0, 900_000_000, ""))
	})

	out, err := c.RunStep(context.Background(), Step{
		Program: []byte("git pull --ff-only\n"),
		Name:    "deploy", Host: "host-a",
	})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.Exit != 0 {
		t.Errorf("Exit = %d, want 0", out.Exit)
	}
	if out.WallNS != 900_000_000 {
		t.Errorf("WallNS = %d, want 900000000", out.WallNS)
	}
	if out.WireCut || out.ProtocolError || out.ProtoMismatch {
		t.Errorf("unexpected failure flags: %+v", out)
	}
	if out.Stdout != "Already up to date.\n" {
		t.Errorf("Stdout = %q", out.Stdout)
	}
	if out.Outputs["status"] != "ok" {
		t.Errorf("Outputs = %v", out.Outputs)
	}
	if out.RunnerHello.OS != "linux" || out.RunnerHello.Version != "testhash" {
		t.Errorf("RunnerHello = %+v", out.RunnerHello)
	}
	if len(out.Trace) != 1 {
		t.Fatalf("Trace len = %d, want 1", len(out.Trace))
	}
	tl := out.Trace[0]
	if tl.Command != "git pull --ff-only" {
		t.Errorf("Trace[0].Command = %q", tl.Command)
	}
	if tl.DurationNS != 842_000_000 {
		t.Errorf("Trace[0].DurationNS = %d", tl.DurationNS)
	}
	if tl.Exit == nil || *tl.Exit != 0 {
		t.Errorf("Trace[0].Exit = %v, want *0", tl.Exit)
	}
	if tl.ElapsedNS < 0 {
		t.Errorf("Trace[0].ElapsedNS = %d, want >= 0", tl.ElapsedNS)
	}
}

func TestRunStep_PerCommandExitSurfaced(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		// A non-fatal failing command mid-script: the runner path surfaces its
		// own exit even though the step's final exit is 0 (U0 §1.2).
		f.emit(t, traceStart(1, "false"))
		f.emit(t, traceEnd(1, 1, 5000))
		f.emit(t, traceStart(2, "echo", "done"))
		f.emit(t, traceEnd(2, 0, 3000))
		f.emit(t, resultFrame(0, 1_000_000, ""))
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("false; echo done\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if len(out.Trace) != 2 {
		t.Fatalf("Trace len = %d, want 2", len(out.Trace))
	}
	if out.Trace[0].Exit == nil || *out.Trace[0].Exit != 1 {
		t.Errorf("Trace[0].Exit = %v, want *1", out.Trace[0].Exit)
	}
	if out.Trace[1].Exit == nil || *out.Trace[1].Exit != 0 {
		t.Errorf("Trace[1].Exit = %v, want *0", out.Trace[1].Exit)
	}
	if out.Exit != 0 {
		t.Errorf("Exit = %d, want 0", out.Exit)
	}
}

func TestRunStep_RunnerLevelError(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		f.emit(t, resultFrame(2, 500_000, "interp: recovered panic running program"))
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("boom\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.Exit != 2 {
		t.Errorf("Exit = %d, want 2", out.Exit)
	}
	if !strings.Contains(out.Error, "recovered panic") {
		t.Errorf("Error = %q, want runner error text", out.Error)
	}
	if out.WireCut || out.ProtocolError {
		t.Errorf("a runner-level error must not set wire/protocol flags: %+v", out)
	}
}

func TestRunStep_WireCutMidStep(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		// A command starts, then the stream dies before any result frame.
		f.emit(t, traceStart(1, "sleep", "100"))
		f.outW.Close() // stdout EOF before result → wire cut
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("sleep 100\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1 (wire-cut signature)", out.Exit)
	}
	if !out.WireCut {
		t.Errorf("WireCut = false, want true")
	}
	if !strings.Contains(out.Error, "wire cut") {
		t.Errorf("Error = %q, want wire-cut text", out.Error)
	}
	// The in-flight command is preserved, un-closed: unknown exit, zero duration.
	if len(out.Trace) != 1 {
		t.Fatalf("Trace len = %d, want 1 (the truncated cmd_start)", len(out.Trace))
	}
	if out.Trace[0].Exit != nil {
		t.Errorf("truncated trace line Exit = %v, want nil", out.Trace[0].Exit)
	}
	if out.Trace[0].DurationNS != 0 {
		t.Errorf("truncated trace line DurationNS = %d, want 0", out.Trace[0].DurationNS)
	}
}

func TestRunStep_ProtocolErrorGarbageLine(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		if _, err := f.outW.Write([]byte("this is not a frame\n")); err != nil {
			t.Errorf("fake: write garbage: %v", err)
		}
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("ok\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1", out.Exit)
	}
	if !out.ProtocolError {
		t.Errorf("ProtocolError = false, want true")
	}
	if out.WireCut {
		t.Errorf("WireCut = true, want false (garbage is a protocol error, not a cut)")
	}
	if !strings.Contains(out.Error, "protocol error") {
		t.Errorf("Error = %q, want protocol-error text", out.Error)
	}
}

func TestRunStep_ProtoMismatch(t *testing.T) {
	c, f := newFakeConn()
	f.proto = runnerproto.ProtoVersion + 99
	go f.serve(t, func(run runnerproto.RunFrame) {
		t.Errorf("run frame must not be sent on a proto mismatch")
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.ProtoMismatch {
		t.Errorf("ProtoMismatch = false, want true")
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1", out.Exit)
	}
	if out.RunnerHello.Proto != runnerproto.ProtoVersion+99 {
		t.Errorf("RunnerHello.Proto = %d", out.RunnerHello.Proto)
	}
}

func TestRunStep_WrongRoleRejectedAtHandshake(t *testing.T) {
	c, f := newFakeConn()
	f.role = runnerproto.RoleDaemon // a non-runner endpoint on the same proto
	go f.serve(t, func(run runnerproto.RunFrame) {
		t.Errorf("run frame must not be sent to a wrong-role endpoint")
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.ProtoMismatch {
		t.Errorf("ProtoMismatch = false, want true for a wrong-role hello")
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1", out.Exit)
	}
	if !strings.Contains(out.Error, "role mismatch") {
		t.Errorf("Error = %q, want a role-mismatch message", out.Error)
	}
}

func TestRunStep_VersionMismatchRejectedAtHandshake(t *testing.T) {
	c, f := newFakeConn()
	f.version = "staleversion" // a stale runner binary, not the bootstrapped one
	go f.serve(t, func(run runnerproto.RunFrame) {
		t.Errorf("run frame must not be sent to a version-mismatched endpoint")
	})

	out, err := c.RunStep(context.Background(), Step{
		Program:       []byte("x\n"),
		ExpectVersion: "wanthash",
	})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.ProtoMismatch {
		t.Errorf("ProtoMismatch = false, want true for a version mismatch")
	}
	if !strings.Contains(out.Error, "version mismatch") {
		t.Errorf("Error = %q, want a version-mismatch message", out.Error)
	}
}

func TestRunStep_MatchingVersionAccepted(t *testing.T) {
	c, f := newFakeConn()
	f.version = "goodhash"
	go f.serve(t, func(run runnerproto.RunFrame) {
		f.emit(t, resultFrame(0, 1000, ""))
	})

	out, err := c.RunStep(context.Background(), Step{
		Program:       []byte("ok\n"),
		ExpectVersion: "goodhash",
	})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.ProtoMismatch {
		t.Errorf("ProtoMismatch = true, want false for a matching version")
	}
	if out.Exit != 0 {
		t.Errorf("Exit = %d, want 0", out.Exit)
	}
}

func TestRunStep_UnexpectedFrameIsProtocolError(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		// The runner must never send a run frame back on stdout.
		f.emit(t, runnerproto.Frame{Type: runnerproto.FrameRun, Run: &runnerproto.RunFrame{Program: []byte("x")}})
	})

	out, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.ProtocolError {
		t.Errorf("ProtocolError = false, want true for an unexpected frame")
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1", out.Exit)
	}
}

func TestRunStep_FileFramesStagedBeforeRun(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		f.emit(t, resultFrame(0, 1000, ""))
	})

	_, err := c.RunStep(context.Background(), Step{
		Program: []byte("cat prev.out\n"),
		Files: []FileStage{
			{Name: "prev.out", Data: []byte("upstream output\n")},
			{Name: "other.bin", Data: []byte{0x00, 0x01, 0x02}},
		},
	})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	files := f.files()
	if len(files) != 2 {
		t.Fatalf("fake received %d file frames, want 2", len(files))
	}
	if files[0].Name != "prev.out" || string(files[0].Data) != "upstream output\n" {
		t.Errorf("file[0] = %+v", files[0])
	}
	if files[1].Name != "other.bin" || len(files[1].Data) != 3 {
		t.Errorf("file[1] = %+v (binary must round-trip)", files[1])
	}
}

func TestRunStep_CancellationSendsSignal(t *testing.T) {
	c, f := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	sawSignal := make(chan struct{})

	go f.serve(t, func(run runnerproto.RunFrame) {
		f.emit(t, traceStart(1, "sleep", "100"))
		cancel() // simulate a step timeout / user cancel
		// wait until the daemon's in-band TERM arrives, then close the step.
		for {
			if len(f.signals()) > 0 {
				close(sawSignal)
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		f.emit(t, resultFrame(137, 100_000_000, "terminated"))
	})

	out, err := c.RunStep(ctx, Step{Program: []byte("sleep 100\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	select {
	case <-sawSignal:
	default:
		t.Fatalf("fake never received an in-band signal")
	}
	sigs := f.signals()
	if len(sigs) == 0 || sigs[0] != runnerproto.SignalTERM {
		t.Errorf("signals = %v, want first = TERM", sigs)
	}
	if out.Exit != 137 {
		t.Errorf("Exit = %d, want 137", out.Exit)
	}
}

func TestRunStep_HandshakeWireCut(t *testing.T) {
	c, f := newFakeConn()
	go func() {
		// Read the daemon hello, then die before acking.
		if _, err := f.dec.DecodeHello(); err != nil {
			return
		}
		f.outW.Close()
	}()

	out, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if out.Exit != -1 || !out.WireCut {
		t.Errorf("handshake death should be a wire cut, got %+v", out)
	}
}

func TestRunStep_OversizeRunFrameNotSentFallsBack(t *testing.T) {
	c, f := newFakeConn()
	sentRun := make(chan struct{}, 1)
	go f.serve(t, func(run runnerproto.RunFrame) {
		// The run frame must NEVER reach the runner — it's over the wire limit.
		sentRun <- struct{}{}
		f.emit(t, resultFrame(0, 1, ""))
	})

	// A body whose base64 RunFrame line exceeds MaxLineBytes (1 MiB). Raw bytes
	// base64-inflate 4/3×, so 1 MiB of raw program comfortably overshoots.
	big := make([]byte, runnerproto.MaxLineBytes)
	for i := range big {
		big[i] = 'a'
	}
	out, err := c.RunStep(context.Background(), Step{Program: big})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.UnsentOversize {
		t.Errorf("UnsentOversize = false, want true for an over-limit run frame")
	}
	if out.Exit != -1 {
		t.Errorf("Exit = %d, want -1", out.Exit)
	}
	if out.WireCut || out.ProtocolError {
		t.Errorf("oversize must not read as wire cut / protocol error: %+v", out)
	}
	select {
	case <-sentRun:
		t.Errorf("an oversize run frame was sent to the runner; it must be withheld")
	default:
	}
}

func TestRunStep_OversizeFileFrameNotSentFallsBack(t *testing.T) {
	c, f := newFakeConn()
	go f.serve(t, func(run runnerproto.RunFrame) {
		t.Errorf("run frame must not be sent when a prior file frame is oversize")
	})

	big := make([]byte, runnerproto.MaxLineBytes)
	out, err := c.RunStep(context.Background(), Step{
		Program: []byte("cat f\n"),
		Files:   []FileStage{{Name: "f", Data: big}},
	})
	if err != nil {
		t.Fatalf("RunStep error: %v", err)
	}
	if !out.UnsentOversize {
		t.Errorf("UnsentOversize = false, want true for an over-limit file frame")
	}
	if len(f.files()) != 0 {
		t.Errorf("an oversize file frame reached the runner; it must be withheld")
	}
}

func TestRunStep_NilStdinIsMisuseError(t *testing.T) {
	// stdout is a valid reader, stdin is nil: a caller-fixable misuse, surfaced as
	// a non-nil error (never a panic, never an outcome).
	c := NewClient(nil, strings.NewReader(""), nil)
	_, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err == nil || !strings.Contains(err.Error(), "nil stdin") {
		t.Fatalf("nil stdin err = %v, want a nil-stdin misuse error", err)
	}
}

func TestRunStep_NilStdoutIsMisuseErrorNotPanic(t *testing.T) {
	// A nil stdout reader must return the documented misuse error, NOT panic in
	// DecodeHello via the eofSpy over a nil reader (finding #18).
	c := NewClient(nopWriteCloser{io.Discard}, nil, nil)
	out, err := c.RunStep(context.Background(), Step{Program: []byte("x\n")})
	if err == nil || !strings.Contains(err.Error(), "nil stdout") {
		t.Fatalf("nil stdout err = %v, want a nil-stdout misuse error", err)
	}
	if out.Exit != 0 || out.WireCut || out.ProtocolError {
		t.Errorf("misuse must return a zero outcome, got %+v", out)
	}
}

// nopWriteCloser adapts an io.Writer to io.WriteCloser for the nil-stdout test
// (Close is a no-op; RunStep closes stdin on the misuse path is not reached, but
// the field must be a WriteCloser).
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestProgressSummary_ReflectsLiveTrace(t *testing.T) {
	c, f := newFakeConn()
	release := make(chan struct{})
	finished := make(chan struct{})

	go f.serve(t, func(run runnerproto.RunFrame) {
		f.emit(t, traceStart(1, "apt-get", "update"))
		f.emit(t, ioFrame(1, "Reading package lists...\n"))
		<-release // hold the step open so the test can sample live progress
		f.emit(t, traceEnd(1, 0, 12_000_000_000))
		f.emit(t, resultFrame(0, 12_500_000_000, ""))
	})

	var out StepOutcome
	go func() {
		out, _ = c.RunStep(context.Background(), Step{
			Program: []byte("apt-get update\n"), Name: "deploy", Host: "host-b",
		})
		close(finished)
	}()

	// Poll until live state reflects the running command.
	var summary string
	deadline := time.After(2 * time.Second)
	for {
		summary = c.ProgressSummary(7)
		if strings.Contains(summary, "apt-get update") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("ProgressSummary never reflected the running command; got:\n%s", summary)
		case <-time.After(2 * time.Millisecond):
		}
	}

	if !strings.Contains(summary, "phase:run") {
		t.Errorf("summary missing phase:run:\n%s", summary)
	}
	if !strings.Contains(summary, "step:deploy") || !strings.Contains(summary, "host:host-b") {
		t.Errorf("summary missing step/host:\n%s", summary)
	}
	if !strings.Contains(summary, "> apt-get update") {
		t.Errorf("summary missing current-command line:\n%s", summary)
	}
	if !strings.Contains(summary, "Reading package lists...") {
		t.Errorf("summary missing recent stdout line:\n%s", summary)
	}

	close(release)
	<-finished
	if out.Exit != 0 {
		t.Errorf("Exit = %d, want 0", out.Exit)
	}
}

func TestProgressSummary_BootstrapPhaseBeforeRun(t *testing.T) {
	c, _ := newFakeConn()
	// Before any RunStep, the client sits in the bootstrap phase (a cold-host
	// connect reads as progress, not a stall — U0 §3).
	s := c.ProgressSummary(1)
	if !strings.Contains(s, "phase:bootstrap") {
		t.Errorf("fresh client summary = %q, want phase:bootstrap", s)
	}
}

func TestFormatDur(t *testing.T) {
	cases := []struct {
		ns   int64
		want string
	}{
		{500, "500ns"},
		{1_500, "1.5µs"},
		{2_500_000, "2.5ms"},
		{842_100_000, "842.1ms"},
		{1_500_000_000, "1.500s"},
	}
	for _, tc := range cases {
		if got := formatDur(tc.ns); got != tc.want {
			t.Errorf("formatDur(%d) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}
