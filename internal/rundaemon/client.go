package rundaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// gracePeriod is how long the client waits after sending an in-band TERM before
// escalating to KILL when the surrounding context is cancelled. The runner
// (U4) owns its own TERM→grace→KILL escalation per external command; this is a
// daemon-side backstop for a runner that ignores the wire signal entirely.
const gracePeriod = 5 * time.Second

// maxRecentLines caps the live-progress ring buffer fed to [Client.ProgressSummary]
// (mirrors internal/mcp/events.go's maxRecentLines so the ticker payload looks
// identical to the legacy path).
const maxRecentLines = 8

// TraceLine is one completed command in a step's trace, assembled from a
// cmd_start + cmd_end trace-frame pair. It is rundaemon's OWN render type
// (decision: internal/rundaemon must never import internal/mcp — U6b adapts
// these to mcp's TraceLine per the U0 contract).
type TraceLine struct {
	// Seq is the runner's per-step command sequence number (pairs a cmd_start
	// with its cmd_end); used for ordering/dedup, not rendered.
	Seq int
	// ElapsedNS is nanoseconds since step start, measured daemon-side (single
	// monotonic clock) when the cmd_start frame arrived. Never a cross-clock
	// subtraction — the runner's own clock supplies DurationNS instead.
	ElapsedNS int64
	// DurationNS is how long the command took, measured runner-side (monotonic)
	// and carried on the cmd_end frame. 0 means unknown — the command was still
	// running when the step ended (truncated mid-run), or a builtin didn't report.
	DurationNS int64
	// Command is the argv joined by spaces (external) or the source text (builtin).
	Command string
	// LineNo is the command's 1-based source line in the step body (from the
	// cmd_start frame). 0 = unknown (older runner or no position at the seam).
	LineNo int
	// Exit is the command's own exit code, non-nil once its cmd_end arrived; nil
	// means unknown (no cmd_end seen — truncated mid-run).
	Exit *int
}

// StepOutcome is the structured result of driving one step through the runner:
// rundaemon's OWN outcome type (never internal/mcp's StepResult). A well-formed
// step ends with Exit set from the result frame; any transport or protocol
// failure ends with Exit == -1 and exactly one of WireCut / ProtocolError /
// ProtoMismatch set, so U6b can decide fallback with a single uniform check.
type StepOutcome struct {
	// Exit is the step's final exit code from the result frame, or -1 when the
	// step never produced one (wire cut, protocol error, proto mismatch).
	Exit int
	// WallNS is the step's total wall-clock duration in nanoseconds (result frame).
	WallNS int64
	// Outputs is the collected $OUTPUT key=value set (output frame); nil if none.
	Outputs map[string]string
	// Stdout is the child's accumulated fd-1 output (io frames, fd 1), decoded.
	Stdout string
	// Stderr is the child's accumulated fd-2 output (io frames, fd 2), decoded,
	// with any runner self-diagnostics (the runner's own stderr) appended.
	Stderr string
	// Trace is the ordered per-command trace assembled from trace frames.
	Trace []TraceLine
	// Error is a runner-level failure (result-frame Error, e.g. a recovered interp
	// panic), a wire-cut reason, or a protocol-error detail; empty on clean success.
	Error string
	// RunnerHello is the runner's handshake ack (os/arch/version/proto).
	RunnerHello runnerproto.HelloFrame
	// WireCut is true when the stream closed (EOF) before a result frame — the
	// "wire cut" signature the daemon reports as exit -1.
	WireCut bool
	// ProtocolError is true when an unparseable/oversized/mismatched frame was
	// read after the handshake — kill the connection and fall back.
	ProtocolError bool
	// ProtoMismatch is true when the handshake was rejected before the run frame:
	// the runner's hello advertised a different protocol version, a non-runner
	// role, or a runner-binary version other than the bootstrapped one. All three
	// mean the body NEVER ran, so mcp safely falls back to legacy (or re-pushes).
	ProtoMismatch bool
	// UnsentOversize is true when a run/file frame's encoded ndjson line would
	// exceed runnerproto.MaxLineBytes, so the daemon refused to send it: the
	// remote decoder would reject an oversized line as a protocol error, which
	// would look like a false exit -1 for a body that NEVER executed. Like
	// ProtoMismatch, this means the body never ran, so mcp falls back to legacy.
	UnsentOversize bool
}

// FileStage is a prior {{step.output}} to stage into the runner's scratch dir
// (a file frame) before the run frame.
type FileStage struct {
	Name string
	Data []byte
}

// Step is one unit of work handed to the runner over the wire. Env is a CLOSED
// {OUTPUT}-only allowlist by the plan's decision #17; this client forwards it
// verbatim and NEVER populates it from os.Environ() — the runner (U3a) enforces
// the allowlist, but the client must not smuggle secrets in either.
type Step struct {
	// Program is the step body as exact bytes (never re-parsed/re-printed).
	Program []byte
	// Env is the closed environment applied to the step (see the note above).
	Env map[string]string
	// Entrypoint names a non-bash interpreter (python3, node…); empty runs the
	// mvdan/sh interp engine.
	Entrypoint string
	// ExpectVersion is the runner-binary content-hash version bootstrap verified
	// on this host. When non-empty, the handshake rejects a runner ack that
	// advertises any other version (a stale endpoint / version skew), so the body
	// never runs against a wrong binary. Empty skips the version check (tests that
	// don't model versioning).
	ExpectVersion string
	// Timeout is the wall-clock step timeout; 0 means no timeout.
	Timeout time.Duration
	// Files are prior outputs to stage before the run frame.
	Files []FileStage
	// Name and Host label the live-progress payload only (not sent on the wire).
	Name string
	Host string
}

// Client drives one runner connection through the ndjson protocol: the hello
// handshake, the run frame (preceded by any file frames), then consuming
// trace / io / output frames until a result frame closes the step or the wire
// is cut. It is transport-agnostic — [NewClient] takes raw stdio, so tests
// drive it over in-process pipes and [SpawnSSH] drives it over an ssh exec.
type Client struct {
	enc    *runnerproto.Encoder
	dec    *runnerproto.Decoder
	stdin  io.WriteCloser
	stderr io.Reader
	eof    *eofSpy

	// live progress state, read by ProgressSummary from the daemon's 3s ticker
	// goroutine while the read loop mutates it.
	mu     sync.Mutex
	start  time.Time
	phase  string
	step   string
	host   string
	curCmd string
	recent []string

	// Live event hooks, optionally set by the caller BEFORE RunStep. All are
	// invoked from the read-loop goroutine as frames arrive, so they must be
	// fast and must not block — they exist to bridge the runner's live frame
	// stream into the daemon's event log (the legacy path gets the equivalent
	// for free by teeing the ssh stdout). Nil hooks are skipped.
	//
	// OnCmdStart fires once per cmd_start frame with the opened TraceLine
	// (Command, LineNo, ElapsedNS; Duration/Exit not yet known). OnStdout /
	// OnStderr fire with each io frame's raw bytes (fd 1 / fd 2).
	OnCmdStart func(TraceLine)
	OnStdout   func([]byte)
	OnStderr   func([]byte)
}

// NewClient wraps the runner's stdio in a protocol client. stdin carries the
// daemon→runner frames (hello, file, run, signal) and is closed when [Client.RunStep]
// returns so the runner's stdin-EOF watchdog can exit. stdout carries the
// runner→daemon frames. stderr, if non-nil, is the runner's free-form
// self-diagnostics (never frames) and is drained into the outcome; pass nil to
// ignore it.
func NewClient(stdin io.WriteCloser, stdout io.Reader, stderr io.Reader) *Client {
	// The decoder reads through an eofSpy so the handshake can tell a stream
	// close (wire cut) from a bad frame (protocol error): DecodeHello collapses
	// EOF into ErrProtocol, but the spy records the real io.EOF underneath. Only
	// the single read-loop goroutine touches it, so no lock is needed.
	spy := &eofSpy{r: stdout}
	return &Client{
		enc:    runnerproto.NewEncoder(stdin),
		dec:    runnerproto.NewDecoder(spy),
		stdin:  stdin,
		stderr: stderr,
		eof:    spy,
		phase:  "bootstrap",
	}
}

// eofSpy wraps a reader and records whether a read ever returned io.EOF, so the
// handshake can distinguish a stream close from a protocol violation even
// though DecodeHello reports both as ErrProtocol.
type eofSpy struct {
	r      io.Reader
	hitEOF bool
}

func (s *eofSpy) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if errors.Is(err, io.EOF) {
		s.hitEOF = true
	}
	return n, err
}

// RunStep drives one full step: handshake → (file frames) → run frame →
// consume the trace/io/output stream → result. It returns a populated
// [StepOutcome] and a nil error for every wire outcome, including failures —
// a wire cut, a protocol error, a proto mismatch, and a runner-level error all
// surface in the outcome (Exit -1 for the transport failures), so the caller
// has one uniform path. A non-nil error is reserved for a misuse the caller
// must fix (a nil stdin/stdout).
//
// stdin is closed on return (deferred) so the runner always receives EOF and
// its watchdog can tear down any surviving children.
func (c *Client) RunStep(ctx context.Context, step Step) (StepOutcome, error) {
	if c.stdin == nil {
		return StepOutcome{}, errors.New("rundaemon: nil stdin")
	}
	// A nil stdout reader would let the handshake's DecodeHello dereference a nil
	// reader through the eofSpy and panic; report it as the documented misuse
	// error (a non-nil error is reserved for caller-fixable misuse) before any
	// frame is written. c.eof wraps the stdout reader; c.eof.r is that reader.
	if c.eof == nil || c.eof.r == nil {
		return StepOutcome{}, errors.New("rundaemon: nil stdout")
	}
	defer c.stdin.Close()

	c.mu.Lock()
	c.step = step.Name
	c.host = step.Host
	c.phase = "bootstrap"
	c.mu.Unlock()

	// Drain the runner's self-diagnostic stderr concurrently so a chatty runner
	// never blocks on a full pipe; folded into the outcome on close.
	var stderrBuf strings.Builder
	var stderrWG sync.WaitGroup
	if c.stderr != nil {
		stderrWG.Add(1)
		go func() {
			defer stderrWG.Done()
			_, _ = io.Copy(&stderrBuf, c.stderr)
		}()
	}
	drainStderr := func(o *StepOutcome) {
		// Close stdin BEFORE waiting for the stderr drain. The runner keeps its
		// stderr open until it exits, and it exits only when its stdin hits EOF;
		// over a real transport (ssh) whose stderr closes at process exit, waiting
		// for the drain without first closing stdin deadlocks — the deferred
		// stdin.Close() below fires only after RunStep returns, i.e. after this
		// wait. Closing here lets the runner exit, its stderr reach EOF, and the
		// copy finish. (A double close via the defer is a harmless no-op; the
		// in-process pipe tests pass stderr=nil, so this path never blocked there.)
		_ = c.stdin.Close()
		stderrWG.Wait()
		if s := stderrBuf.String(); s != "" {
			o.Stderr += s
		}
	}

	// 1. Handshake. Send the daemon hello, read the runner's ack.
	if err := c.enc.Encode(runnerproto.Frame{
		Type:  runnerproto.FrameHello,
		Hello: &runnerproto.HelloFrame{Proto: runnerproto.ProtoVersion, Role: runnerproto.RoleDaemon},
	}); err != nil {
		o := wireCut("send hello: " + err.Error())
		drainStderr(&o)
		return o, nil
	}
	hello, err := c.dec.DecodeHello()
	if err != nil {
		// DecodeHello reports both a stream close and a bad frame as ErrProtocol;
		// the spy tells them apart — a closed stream during the handshake is a
		// wire cut, a garbage/oversized/non-hello frame is a protocol error.
		var o StepOutcome
		if c.eof.hitEOF {
			o = wireCut("stream closed during handshake")
		} else {
			o = protocolError(err.Error(), nil)
		}
		o.RunnerHello = hello
		drainStderr(&o)
		return o, nil
	}
	if hello.Proto != runnerproto.ProtoVersion {
		o := StepOutcome{
			Exit:          -1,
			RunnerHello:   hello,
			ProtoMismatch: true,
			Error: fmt.Sprintf("runner protocol mismatch (runner proto:%d, daemon proto:%d)",
				hello.Proto, runnerproto.ProtoVersion),
		}
		drainStderr(&o)
		return o, nil
	}
	// The ack must come from a RUNNER (not another daemon or a same-proto
	// non-runner endpoint), and — when bootstrap pinned an expected version — from
	// exactly the bootstrapped binary. Either mismatch means we would otherwise
	// send run/file frames to a wrong-role or stale endpoint, so reject at the
	// handshake (body never ran) and let mcp fall back / re-bootstrap.
	if hello.Role != runnerproto.RoleRunner {
		o := StepOutcome{
			Exit:          -1,
			RunnerHello:   hello,
			ProtoMismatch: true,
			Error:         fmt.Sprintf("runner handshake role mismatch (got role:%q, want %q)", hello.Role, runnerproto.RoleRunner),
		}
		drainStderr(&o)
		return o, nil
	}
	if step.ExpectVersion != "" && hello.Version != step.ExpectVersion {
		o := StepOutcome{
			Exit:          -1,
			RunnerHello:   hello,
			ProtoMismatch: true,
			Error:         fmt.Sprintf("runner version mismatch (runner version:%q, expected %q)", hello.Version, step.ExpectVersion),
		}
		drainStderr(&o)
		return o, nil
	}

	// 2. Stage files, then the run frame. step start = the moment the runner is
	// told to execute, so ElapsedNS is measured from here.
	//
	// Frame-size preflight (finding #1b): a file/run frame whose encoded ndjson
	// line would exceed runnerproto.MaxLineBytes is NOT sent — the remote decoder
	// rejects an oversized line as a protocol error, which would surface as a
	// false exit -1 for a body that never executed. Detecting it daemon-side lets
	// mcp fall back to legacy (body provably unsent) instead. This is checked
	// BEFORE the run frame is written, so nothing runs on an oversize step.
	for _, f := range step.Files {
		fr := runnerproto.Frame{
			Type: runnerproto.FrameFile,
			File: &runnerproto.FileFrame{Name: f.Name, Data: f.Data},
		}
		if over, err := frameOverLimit(fr); err != nil || over {
			o := unsentOversize(fmt.Sprintf("file frame %q too large to send (exceeds %d-byte wire limit)", f.Name, runnerproto.MaxLineBytes))
			o.RunnerHello = hello
			drainStderr(&o)
			return o, nil
		}
		if err := c.enc.Encode(fr); err != nil {
			o := wireCut("send file frame: " + err.Error())
			o.RunnerHello = hello
			drainStderr(&o)
			return o, nil
		}
	}
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run: &runnerproto.RunFrame{
			Program:    step.Program,
			Env:        step.Env,
			Entrypoint: step.Entrypoint,
			TimeoutNS:  step.Timeout.Nanoseconds(),
		},
	}
	if over, err := frameOverLimit(runFrame); err != nil || over {
		o := unsentOversize(fmt.Sprintf("run frame too large to send (body exceeds %d-byte wire limit)", runnerproto.MaxLineBytes))
		o.RunnerHello = hello
		drainStderr(&o)
		return o, nil
	}
	c.mu.Lock()
	c.start = time.Now()
	c.phase = "run"
	c.mu.Unlock()
	if err := c.enc.Encode(runFrame); err != nil {
		o := wireCut("send run frame: " + err.Error())
		o.RunnerHello = hello
		drainStderr(&o)
		return o, nil
	}

	// 3. Concurrent cancel watcher: on ctx cancel, send an in-band TERM (ssh
	// signal delivery is unreliable — cancellation rides the wire), escalating
	// to KILL after the grace period if the read loop has not finished.
	done := make(chan struct{})
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		c.watchCancel(ctx, done)
	}()

	// 4. Read loop consumes frames until a result closes the step (or the wire
	// dies / a garbage frame arrives).
	outcome := c.readLoop()
	outcome.RunnerHello = hello

	close(done)
	watchWG.Wait()
	drainStderr(&outcome)
	return outcome, nil
}

// readLoop consumes runner→daemon frames until a result frame, EOF (wire cut),
// or a protocol error. It assembles trace lines, accumulates io, and captures
// the $OUTPUT set, updating live-progress state as it goes.
func (c *Client) readLoop() StepOutcome {
	var out StepOutcome
	var stdout, stderr strings.Builder
	// seqIndex maps a cmd sequence number to its slot in out.Trace so a cmd_end
	// can back-fill the line its cmd_start opened.
	seqIndex := make(map[int]int)

	for {
		fr, err := c.dec.Decode()
		if err != nil {
			o := failFromDecode(err, "run", &out)
			o.Stdout = stdout.String()
			o.Stderr = stderr.String()
			return o
		}
		switch fr.Type {
		case runnerproto.FrameTrace:
			c.handleTrace(fr.Trace, &out.Trace, seqIndex)
		case runnerproto.FrameIO:
			b, berr := fr.IO.Bytes()
			if berr != nil {
				out.Stdout = stdout.String()
				out.Stderr = stderr.String()
				return protocolError("decode io payload: "+berr.Error(), &out)
			}
			if fr.IO.FD == 2 {
				stderr.Write(b)
				if c.OnStderr != nil {
					c.OnStderr(b)
				}
			} else {
				stdout.Write(b)
				if c.OnStdout != nil {
					c.OnStdout(b)
				}
			}
			c.pushRecent(b)
		case runnerproto.FrameOutput:
			// The runner sends the full $OUTPUT set in one frame, but merge
			// rather than overwrite so a future incremental sender can't silently
			// drop earlier keys.
			if out.Outputs == nil {
				out.Outputs = make(map[string]string, len(fr.Output.Values))
			}
			for k, v := range fr.Output.Values {
				out.Outputs[k] = v
			}
		case runnerproto.FrameResult:
			out.Exit = fr.Result.Exit
			out.WallNS = fr.Result.WallNS
			out.Error = fr.Result.Error
			out.Stdout = stdout.String()
			out.Stderr = stderr.String()
			return out
		case runnerproto.FrameHello, runnerproto.FrameRun, runnerproto.FrameFile, runnerproto.FrameSignal:
			// The runner must never send these on stdout; treat as a contract
			// violation, same class as a garbage line.
			out.Stdout = stdout.String()
			out.Stderr = stderr.String()
			return protocolError(fmt.Sprintf("unexpected %q frame from runner", fr.Type), &out)
		}
	}
}

// handleTrace folds one trace frame into the trace slice: a cmd_start opens a
// new line (Command, ElapsedNS, nil Exit), a cmd_end back-fills the line its
// Seq opened (DurationNS, Exit). A cmd_end with no prior cmd_start is recorded
// standalone so nothing is silently dropped.
func (c *Client) handleTrace(t *runnerproto.TraceFrame, trace *[]TraceLine, seqIndex map[int]int) {
	switch t.Event {
	case runnerproto.TraceCmdStart:
		cmd := strings.Join(t.Argv, " ")
		line := TraceLine{
			Seq:       t.Seq,
			ElapsedNS: c.elapsed(),
			Command:   cmd,
			LineNo:    t.Line,
		}
		*trace = append(*trace, line)
		seqIndex[t.Seq] = len(*trace) - 1
		c.setCurCmd(cmd)
		if c.OnCmdStart != nil {
			c.OnCmdStart(line)
		}
	case runnerproto.TraceCmdEnd:
		exit := t.Exit
		if i, ok := seqIndex[t.Seq]; ok {
			(*trace)[i].DurationNS = t.DurNS
			(*trace)[i].Exit = &exit
			c.pushTrace((*trace)[i])
		} else {
			// Unpaired cmd_end — keep it rather than lose the signal.
			line := TraceLine{Seq: t.Seq, ElapsedNS: c.elapsed(), DurationNS: t.DurNS, Exit: &exit}
			*trace = append(*trace, line)
			c.pushTrace(line)
		}
	}
}

// watchCancel sends an in-band TERM when ctx is cancelled, escalating to KILL
// after gracePeriod if the read loop (done) has not finished. It returns as
// soon as done closes so RunStep never leaks the goroutine.
func (c *Client) watchCancel(ctx context.Context, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	_ = c.enc.Encode(runnerproto.Frame{
		Type:   runnerproto.FrameSignal,
		Signal: &runnerproto.SignalFrame{Signal: runnerproto.SignalTERM},
	})
	select {
	case <-done:
	case <-time.After(gracePeriod):
		_ = c.enc.Encode(runnerproto.Frame{
			Type:   runnerproto.FrameSignal,
			Signal: &runnerproto.SignalFrame{Signal: runnerproto.SignalKILL},
		})
	}
}

// ProgressSummary renders the live ticker payload for the daemon's 3s MCP
// progress notification, fed from live trace state (decision #14, U0 §3). The
// prefix matches the legacy path's `executing (Ns) step:X host:Y` so existing
// consumers keep parsing, with an added `phase:bootstrap|run` token; the `>`
// line is the newest command and the `---` block is the recent trace/output ring.
func (c *Client) ProgressSummary(elapsed int) string {
	c.mu.Lock()
	phase := c.phase
	step := c.step
	host := c.host
	cmd := c.curCmd
	lines := make([]string, len(c.recent))
	copy(lines, c.recent)
	c.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "executing (%ds)", elapsed)
	if step != "" {
		fmt.Fprintf(&b, " step:%s", step)
	}
	if host != "" {
		fmt.Fprintf(&b, " host:%s", host)
	}
	if phase != "" {
		fmt.Fprintf(&b, " phase:%s", phase)
	}
	if cmd != "" {
		fmt.Fprintf(&b, "\n> %s", truncRunes(cmd, 120))
	}
	if len(lines) > 0 {
		b.WriteString("\n---")
		for _, l := range lines {
			fmt.Fprintf(&b, "\n%s", truncRunes(l, 200))
		}
	}
	return b.String()
}

func (c *Client) elapsed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start.IsZero() {
		return 0
	}
	return time.Since(c.start).Nanoseconds()
}

func (c *Client) setCurCmd(cmd string) {
	c.mu.Lock()
	c.curCmd = cmd
	c.mu.Unlock()
}

// pushTrace appends a `[trace] <cmd>  (<dur>)` line to the recent ring so a
// silent long-running command still produces visible progress from its own
// completion event (U0 §3).
func (c *Client) pushTrace(t TraceLine) {
	if t.Command == "" {
		return
	}
	c.pushLine(fmt.Sprintf("[trace] %s  (%s)", t.Command, formatDur(t.DurationNS)))
}

// pushRecent appends the non-empty text lines of a decoded io chunk to the
// recent ring (approximate line-splitting is fine for a 3s ticker).
func (c *Client) pushRecent(b []byte) {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			c.pushLine(line)
		}
	}
}

func (c *Client) pushLine(line string) {
	c.mu.Lock()
	c.recent = append(c.recent, line)
	if len(c.recent) > maxRecentLines {
		c.recent = c.recent[len(c.recent)-maxRecentLines:]
	}
	c.mu.Unlock()
}

// failFromDecode maps a decoder error to the right failure outcome: io.EOF
// before a result is a wire cut; an ErrProtocol is a protocol error; anything
// else is surfaced as a protocol error too (the connection is unusable). The
// partial outcome (already-collected trace/output) is preserved when provided.
func failFromDecode(err error, where string, partial *StepOutcome) StepOutcome {
	if errors.Is(err, io.EOF) {
		return wireCutFrom("stream closed before result frame ("+where+")", partial)
	}
	if errors.Is(err, runnerproto.ErrProtocol) {
		return protocolError(err.Error(), partial)
	}
	return protocolError(fmt.Sprintf("read %s: %v", where, err), partial)
}

func wireCut(reason string) StepOutcome { return wireCutFrom(reason, nil) }

func wireCutFrom(reason string, partial *StepOutcome) StepOutcome {
	o := StepOutcome{}
	if partial != nil {
		o = *partial
	}
	o.Exit = -1
	o.WireCut = true
	o.Error = "wire cut: " + reason
	return o
}

func protocolError(reason string, partial *StepOutcome) StepOutcome {
	o := StepOutcome{}
	if partial != nil {
		o = *partial
	}
	o.Exit = -1
	o.ProtocolError = true
	o.Error = "protocol error: " + reason
	return o
}

// unsentOversize marks a frame the daemon refused to send because its encoded
// ndjson line would exceed runnerproto.MaxLineBytes. The body never ran, so the
// caller falls back to legacy (ranBody=false), NOT a false exit -1.
func unsentOversize(reason string) StepOutcome {
	return StepOutcome{Exit: -1, UnsentOversize: true, Error: "frame too large: " + reason}
}

// frameOverLimit reports whether f's encoded ndjson line (the JSON plus the
// trailing '\n' the encoder appends) would exceed runnerproto.MaxLineBytes — the
// same ceiling the remote decoder enforces. It returns an error only if the frame
// fails to marshal, which the caller treats as unsendable as well.
func frameOverLimit(f runnerproto.Frame) (bool, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return false, err
	}
	return len(data)+1 > runnerproto.MaxLineBytes, nil
}

// formatDur renders a nanosecond duration self-scaling (ns/µs/ms/s), matching
// the U0 trace-render contract (§1.1).
func formatDur(ns int64) string {
	switch d := time.Duration(ns); {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", ns)
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(ns)/1e3)
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(ns)/1e6)
	default:
		return fmt.Sprintf("%.3fs", float64(ns)/1e9)
	}
}

// truncRunes truncates s to at most n runes, appending an ellipsis when cut
// (rune-safe, matching internal/mcp/events.go's truncation).
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
