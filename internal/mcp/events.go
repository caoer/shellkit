package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// EventStream is the live event log for a single MCP call.
//
// It writes JSONL events to <mcpStateDir>/events/<call-id>.jsonl. The dashboard
// fsnotify-watches that directory for live updates.
//
// Concurrency: Tmux fan-out may run multiple goroutines per call, each holding
// its own emitter writers but sharing one EventStream. The internal mutex
// serialises writes to the file.
type EventStream struct {
	callID   string
	file     *os.File
	mu       sync.Mutex
	emitters []*eventEmitter

	// liveMu guards both the progress summary state and the MCP log buffer.
	// Lock ordering: mu → liveMu (never acquire mu while holding liveMu).
	liveMu      sync.Mutex
	execCmd     string // most recent "executing" command
	execHost    string
	recentLines []string // last N stdout/stderr lines (rendered)
	stepName    string   // current step name

	// progressDelegate, when set, supplies the ticker payload instead of the
	// legacy summary fields above. The runner path (U6b) points it at the active
	// rundaemon.Client's ProgressSummary so the ticker renders the live trace feed
	// + phase: token (U0 §3); it is nil on the legacy path, which is what keeps
	// the legacy ticker byte-identical.
	progressDelegate func(int) string

	// MCP server reference — used by sendMCPLog to forward output lines
	// as notifications/message events in real time.
	mcpSrv *server.MCPServer
	mcpCtx context.Context
}

// eventsDir returns the directory that holds per-call event files.
func EventsDir() string {
	return filepath.Join(StateDir(), "events")
}

// callEventsPath returns the JSONL path for one call.
func CallEventsPath(callID string) string {
	return filepath.Join(EventsDir(), callID+".jsonl")
}

// NewEventStream opens the event log file for a call.
// Returns nil and an error if the file cannot be created; callers SHOULD
// continue executing without live events on error.
func NewEventStream(callID string) (*EventStream, error) {
	if err := os.MkdirAll(EventsDir(), 0755); err != nil {
		return nil, fmt.Errorf("mkdir events: %w", err)
	}
	f, err := os.OpenFile(CallEventsPath(callID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("open events file: %w", err)
	}
	return &EventStream{callID: callID, file: f}, nil
}

// SetMCP attaches the MCP server and context for buffered log notifications.
func (s *EventStream) SetMCP(srv *server.MCPServer, ctx context.Context) {
	if s == nil {
		return
	}
	s.mcpSrv = srv
	s.mcpCtx = ctx
}

// Close finalises the stream. Flushes any buffered partial lines from
// emitters created via StdoutWriter / StderrWriter, then closes the file.
// Safe to call multiple times.
func (s *EventStream) Close() error {
	if s == nil {
		return nil
	}

	// Snapshot + clear emitters under lock; flush them outside the lock so
	// processLine -> Emit can acquire s.mu without deadlocking.
	s.mu.Lock()
	emitters := s.emitters
	s.emitters = nil
	s.mu.Unlock()

	for _, e := range emitters {
		e.Flush()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Emit writes one JSONL event to the stream. Safe to call concurrently.
// Best-effort: errors are written to stderr but don't propagate, since the
// caller is mid-execution and shouldn't fail the whole call over a log glitch.
func (s *EventStream) Emit(kind string, fields map[string]any) {
	if s == nil {
		return
	}
	ev := map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"kind": kind,
	}
	for k, v := range fields {
		ev[k] = v
	}
	data, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "events: marshal: %v\n", err)
		return
	}

	s.mu.Lock()
	if s.file == nil {
		s.mu.Unlock()
		return
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "events: write: %v\n", err)
	}
	s.mu.Unlock()

	// These run outside mu so an external send doesn't block file
	// writes or other emitters. Only liveMu is held for updateSummary.
	s.updateSummary(kind, fields)
	s.sendMCPLog(kind, fields)
}

const maxRecentLines = 8

func (s *EventStream) updateSummary(kind string, fields map[string]any) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	switch kind {
	case "step-start":
		if name, _ := fields["name"].(string); name != "" {
			s.stepName = name
		}
		// Reset on step boundary so stale output doesn't bleed across steps.
		s.execCmd = ""
		s.execHost = ""
		s.recentLines = s.recentLines[:0]
	case "executing":
		if cmd, _ := fields["cmd"].(string); cmd != "" {
			s.execCmd = cmd
		}
		if host, _ := fields["host"].(string); host != "" {
			s.execHost = host
		}
	case "stdout", "stderr":
		line, _ := fields["line"].(string)
		if line == "" {
			return
		}
		prefix := ""
		if kind == "stderr" {
			prefix = "[err] "
		}
		s.recentLines = append(s.recentLines, prefix+line)
		if len(s.recentLines) > maxRecentLines {
			// Copy tail into fresh slice to release the old backing array.
			tail := make([]string, maxRecentLines)
			copy(tail, s.recentLines[len(s.recentLines)-maxRecentLines:])
			s.recentLines = tail
		}
	}
}

// SetProgressDelegate installs (or, with nil, clears) a function that supplies
// the ticker payload for the currently-active runner step. The runner path sets
// it to the driving rundaemon.Client's ProgressSummary around a step and clears
// it after; fan-out is sequential (decision #10) so at most one delegate is ever
// live. Safe to call concurrently with the ticker goroutine.
func (s *EventStream) SetProgressDelegate(fn func(int) string) {
	if s == nil {
		return
	}
	s.liveMu.Lock()
	s.progressDelegate = fn
	s.liveMu.Unlock()
}

// ProgressSummary returns a compact multi-line string for MCP progress
// notifications. Includes current step, executing command, and recent output.
func (s *EventStream) ProgressSummary(elapsed int) string {
	if s == nil {
		return fmt.Sprintf("executing (%ds)", elapsed)
	}
	s.liveMu.Lock()
	delegate := s.progressDelegate
	step := s.stepName
	cmd := s.execCmd
	host := s.execHost
	lines := make([]string, len(s.recentLines))
	copy(lines, s.recentLines)
	s.liveMu.Unlock()

	// Runner path: defer entirely to the live Client renderer (U0 §3). Called
	// outside liveMu so the Client's own mutex never nests under ours.
	if delegate != nil {
		return delegate(elapsed)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "executing (%ds)", elapsed)
	if step != "" {
		fmt.Fprintf(&b, " step:%s", step)
	}
	if host != "" {
		fmt.Fprintf(&b, " host:%s", host)
	}
	if cmd != "" {
		truncCmd := truncRunes(cmd, 120)
		fmt.Fprintf(&b, "\n> %s", truncCmd)
	}
	if len(lines) > 0 {
		b.WriteString("\n---")
		for _, l := range lines {
			fmt.Fprintf(&b, "\n%s", truncRunes(l, 200))
		}
	}
	return b.String()
}

// sendMCPLog sends each output line immediately as an MCP notification.
// No buffering — live means live.
func (s *EventStream) sendMCPLog(kind string, fields map[string]any) {
	if s == nil || s.mcpSrv == nil || s.mcpCtx == nil {
		return
	}

	switch kind {
	case "stdout", "stderr":
		line, _ := fields["line"].(string)
		if host, _ := fields["host"].(string); host != "" {
			line = "[" + host + "] " + line
		}
		if kind == "stderr" {
			line = "[stderr] " + line
		}
		if err := s.mcpSrv.SendNotificationToClient(s.mcpCtx, "notifications/message", map[string]any{
			"level":  "info",
			"logger": "shellkit",
			"data":   line,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "events: log notification failed: %v\n", err)
		}
	}
}

// truncRunes truncates s to maxRunes, appending "..." if truncated.
// Unlike byte slicing, this never splits multi-byte UTF-8 sequences.
func truncRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-3]) + "..."
}

// StdoutWriter returns an io.Writer that emits user stdout events.
//
// nonce, when non-empty, enables wrapper-protocol awareness:
//   - Lines equal to outputMarkerFor(nonce) end the user-stdout region; further
//     lines are silently consumed (they belong to the $OUTPUT/stderr capture
//     postscript and are parsed elsewhere).
//   - Lines beginning with traceMarkerFor(nonce) are emitted as "executing"
//     events instead of "stdout" — they're DEBUG-trap markers, not real output.
//
// For local execution where there is no wrapper, pass nonce="" but still set
// traceNonce if trace is enabled, so trace lines get filtered.
func (s *EventStream) StdoutWriter(stepIdx int, stepName, host, nonce string) io.Writer {
	e := &eventEmitter{
		stream:   "stdout",
		stepIdx:  stepIdx,
		stepName: stepName,
		host:     host,
		nonce:    nonce,
		stop:     &outputMarkerLine{nonce: nonce}, // empty nonce → never matches
		out:      s,
	}
	s.registerEmitter(e)
	return e
}

// StderrWriter returns an io.Writer that emits stderr events. No marker
// awareness: stderr is the OUTER process stderr (ssh client, local exec) and
// doesn't carry wrapper-protocol markers.
func (s *EventStream) StderrWriter(stepIdx int, stepName, host string) io.Writer {
	e := &eventEmitter{
		stream:   "stderr",
		stepIdx:  stepIdx,
		stepName: stepName,
		host:     host,
		out:      s,
	}
	s.registerEmitter(e)
	return e
}

func (s *EventStream) registerEmitter(e *eventEmitter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitters = append(s.emitters, e)
}

// outputMarkerLine wraps the outputMarker for a specific nonce so we can
// compare lines without recomputing the string each time.
type outputMarkerLine struct {
	nonce string
}

func (m *outputMarkerLine) matches(line string) bool {
	if m.nonce == "" {
		return false
	}
	return line == outputMarkerFor(m.nonce)
}

// eventEmitter is an io.Writer that buffers, splits on newline, classifies
// each line, and emits an event via its parent EventStream.
type eventEmitter struct {
	stream   string // "stdout" or "stderr"
	stepIdx  int
	stepName string
	host     string
	nonce    string

	stop *outputMarkerLine // when this line is seen, suppress all further emission

	mu       sync.Mutex
	buf      []byte
	suppress bool
	out      *EventStream
}

func (e *eventEmitter) Write(p []byte) (int, error) {
	if e.out == nil {
		return len(p), nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	n := len(p)
	e.buf = append(e.buf, p...)
	for {
		i := bytes.IndexByte(e.buf, '\n')
		if i < 0 {
			break
		}
		line := string(e.buf[:i])
		e.buf = e.buf[i+1:]
		e.processLine(line)
	}
	return n, nil
}

// Flush emits any unterminated buffered bytes as a final line. Should be
// called after the upstream process has exited.
func (e *eventEmitter) Flush() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) > 0 {
		e.processLine(string(e.buf))
		e.buf = e.buf[:0]
	}
}

func (e *eventEmitter) processLine(line string) {
	if e.suppress {
		return
	}
	if e.stop != nil && e.stop.matches(line) {
		e.suppress = true
		return
	}
	if e.nonce != "" {
		prefix := traceMarkerFor(e.nonce) + " "
		bareMarker := traceMarkerFor(e.nonce)
		// Full-line marker at start — pure trace, no user output.
		if strings.HasPrefix(line, prefix) {
			if elapsed, lineNo, cmd, ok := parseTraceLine(line[len(prefix):]); ok {
				fields := map[string]any{
					"step":        e.stepIdx,
					"name":        e.stepName,
					"host":        e.host,
					"elapsed_sec": elapsed,
					"cmd":         cmd,
				}
				if lineNo > 0 {
					fields["line_no"] = lineNo
				}
				e.out.Emit("executing", fields)
			}
			return
		}
		// Mid-line marker: preceding command used echo -n (no trailing
		// newline), so the trap output got appended to the same line.
		// Emit the real output prefix, then the trace event.
		if idx := strings.Index(line, prefix); idx > 0 {
			e.out.Emit(e.stream, map[string]any{
				"step": e.stepIdx,
				"name": e.stepName,
				"host": e.host,
				"line": line[:idx],
			})
			if elapsed, lineNo, cmd, ok := parseTraceLine(line[idx+len(prefix):]); ok {
				fields := map[string]any{
					"step":        e.stepIdx,
					"name":        e.stepName,
					"host":        e.host,
					"elapsed_sec": elapsed,
					"cmd":         cmd,
				}
				if lineNo > 0 {
					fields["line_no"] = lineNo
				}
				e.out.Emit("executing", fields)
			}
			return
		}
		// Bare marker without variables — strip entirely, keep prefix.
		if idx := strings.Index(line, bareMarker); idx >= 0 {
			if idx > 0 {
				e.out.Emit(e.stream, map[string]any{
					"step": e.stepIdx,
					"name": e.stepName,
					"host": e.host,
					"line": line[:idx],
				})
			}
			return
		}
	}
	e.out.Emit(e.stream, map[string]any{
		"step": e.stepIdx,
		"name": e.stepName,
		"host": e.host,
		"line": line,
	})
}

// LiveEvent is the parsed shape of an event read back from disk.
// Fields are flat — match the JSON written by Emit.
type LiveEvent struct {
	Ts         time.Time   `json:"-"`
	Kind       string      `json:"kind"`
	Step       int         `json:"step,omitempty"`
	Name       string      `json:"name,omitempty"`
	Host       string      `json:"host,omitempty"`
	Hosts      []string    `json:"hosts,omitempty"`
	Action     string      `json:"action,omitempty"`
	Line       string      `json:"line,omitempty"`
	Cmd        string      `json:"cmd,omitempty"`
	ElapsedSec int         `json:"elapsed_sec,omitempty"`
	LineNo     int         `json:"line_no,omitempty"`
	ExitCode   int         `json:"exit_code,omitempty"`
	DurationMs int64       `json:"duration_ms,omitempty"`
	Status     string      `json:"status,omitempty"`
	Steps      []StepBrief `json:"steps,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	Input      string      `json:"input,omitempty"`
	Error      string      `json:"error,omitempty"`
	TimedOut   bool        `json:"timed_out,omitempty"`
	Pid        int         `json:"pid,omitempty"`

	TsRaw string `json:"ts,omitempty"` // raw RFC3339Nano string from file
}

// ParseLiveEvent parses one JSONL line into a LiveEvent.
func ParseLiveEvent(line []byte) (LiveEvent, error) {
	var ev LiveEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, err
	}
	if ev.TsRaw != "" {
		if t, err := time.Parse(time.RFC3339Nano, ev.TsRaw); err == nil {
			ev.Ts = t
		}
	}
	return ev, nil
}
