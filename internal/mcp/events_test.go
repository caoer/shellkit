package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempStateDir redirects mcpStateDir() to a temp dir for the duration of
// the test. mcpStateDir() reads HOME, so we override HOME.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return filepath.Join(tmp, ".local", "state", "shellkit")
}

func readEvents(t *testing.T, path string) []LiveEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open events file: %v", err)
	}
	defer f.Close()

	var out []LiveEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ev, err := ParseLiveEvent(sc.Bytes())
		if err != nil {
			t.Fatalf("parse line %q: %v", sc.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestEventStream_StdoutAndExecuting(t *testing.T) {
	withTempStateDir(t)

	callID := "test-call-1"
	stream, err := NewEventStream(callID)
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}

	nonce := "abc123"
	w := stream.StdoutWriter(0, "build", "host-1", nonce)

	traceMarker := traceMarkerFor(nonce)
	outMarker := outputMarkerFor(nonce)

	// Simulate streaming bytes — multi-write, partial line at end.
	chunks := []string{
		"hello\n",
		traceMarker + " 0 echo hello\n",
		"world\n",
		traceMarker + " 1 echo world\n",
		outMarker + "\n", // boundary — anything after should be suppressed
		"key=value\n",
		"more=stuff\n",
	}
	for _, c := range chunks {
		if _, err := fmt.Fprint(w, c); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if e, ok := w.(*eventEmitter); ok {
		e.Flush()
	}

	stream.Close()

	events := readEvents(t, CallEventsPath(callID))
	if len(events) != 4 {
		t.Fatalf("expected 4 events (2 stdout + 2 executing), got %d: %+v", len(events), events)
	}

	// Order: stdout("hello"), executing(0, "echo hello"), stdout("world"), executing(1, "echo world")
	expectStdout := func(idx int, line string) {
		t.Helper()
		ev := events[idx]
		if ev.Kind != "stdout" || ev.Line != line {
			t.Errorf("event[%d]: want stdout %q, got kind=%s line=%q", idx, line, ev.Kind, ev.Line)
		}
		if ev.Host != "host-1" {
			t.Errorf("event[%d]: want host=host-1, got %q", idx, ev.Host)
		}
	}
	expectExec := func(idx int, elapsed int, cmd string) {
		t.Helper()
		ev := events[idx]
		if ev.Kind != "executing" || ev.ElapsedSec != elapsed || ev.Cmd != cmd {
			t.Errorf("event[%d]: want executing(%d, %q), got kind=%s elapsed=%d cmd=%q",
				idx, elapsed, cmd, ev.Kind, ev.ElapsedSec, ev.Cmd)
		}
	}
	expectStdout(0, "hello")
	expectExec(1, 0, "echo hello")
	expectStdout(2, "world")
	expectExec(3, 1, "echo world")
}

func TestEventStream_Stderr(t *testing.T) {
	withTempStateDir(t)

	stream, err := NewEventStream("c2")
	if err != nil {
		t.Fatal(err)
	}
	w := stream.StderrWriter(1, "remote-step", "h2")

	fmt.Fprint(w, "warning: permanently added\nbad host key\n")
	if e, ok := w.(*eventEmitter); ok {
		e.Flush()
	}
	stream.Close()

	events := readEvents(t, CallEventsPath("c2"))
	if len(events) != 2 {
		t.Fatalf("want 2 stderr events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Kind != "stderr" || ev.Step != 1 || ev.Name != "remote-step" || ev.Host != "h2" {
			t.Errorf("event[%d]: unexpected metadata: %+v", i, ev)
		}
	}
	if events[0].Line != "warning: permanently added" {
		t.Errorf("line[0] = %q", events[0].Line)
	}
}

func TestEventStream_Emit_EnvelopeEvents(t *testing.T) {
	withTempStateDir(t)

	stream, _ := NewEventStream("c3")
	stream.Emit("call-start", map[string]any{
		"steps": []StepBrief{
			{Name: "s1", Action: "ssh", Hosts: []string{"h1"}},
		},
		"session_id": "sess-1",
	})
	stream.Emit("step-start", map[string]any{"step": 0, "name": "s1", "hosts": []string{"h1"}})
	stream.Emit("step-end", map[string]any{"step": 0, "name": "s1", "exit_code": 0, "duration_ms": int64(123)})
	stream.Emit("call-end", map[string]any{"status": "ok", "duration_ms": int64(456)})
	stream.Close()

	events := readEvents(t, CallEventsPath("c3"))
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}
	want := []string{"call-start", "step-start", "step-end", "call-end"}
	for i, k := range want {
		if events[i].Kind != k {
			t.Errorf("event[%d].kind = %q, want %q", i, events[i].Kind, k)
		}
	}
	if events[0].SessionID != "sess-1" {
		t.Errorf("call-start session_id = %q", events[0].SessionID)
	}
	if len(events[0].Steps) != 1 || events[0].Steps[0].Name != "s1" {
		t.Errorf("call-start steps malformed: %+v", events[0].Steps)
	}
	if events[2].ExitCode != 0 || events[2].DurationMs != 123 {
		t.Errorf("step-end fields wrong: %+v", events[2])
	}
}

func TestEventEmitter_PartialLineBuffering(t *testing.T) {
	withTempStateDir(t)
	stream, _ := NewEventStream("c4")
	w := stream.StdoutWriter(0, "step", "host", "")

	// Write a line in 3 chunks, no terminating newline until the end.
	fmt.Fprint(w, "hel")
	fmt.Fprint(w, "lo wo")
	fmt.Fprint(w, "rld\n")

	stream.Close()

	events := readEvents(t, CallEventsPath("c4"))
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Line != "hello world" {
		t.Errorf("line = %q", events[0].Line)
	}
}

func TestEventEmitter_FlushUnterminated(t *testing.T) {
	withTempStateDir(t)
	stream, _ := NewEventStream("c5")
	w := stream.StdoutWriter(0, "step", "host", "")

	fmt.Fprint(w, "no-newline-here")
	w.(*eventEmitter).Flush()
	stream.Close()

	events := readEvents(t, CallEventsPath("c5"))
	if len(events) != 1 || events[0].Line != "no-newline-here" {
		t.Errorf("expected flushed line, got %+v", events)
	}
}

func TestParseLiveEvent_RawTimestamp(t *testing.T) {
	raw := `{"ts":"2026-05-09T08:00:00Z","kind":"stdout","line":"hi"}`
	ev, err := ParseLiveEvent([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "stdout" || ev.Line != "hi" {
		t.Errorf("parse failed: %+v", ev)
	}
	if ev.Ts.IsZero() {
		t.Errorf("expected parsed timestamp from %q", ev.TsRaw)
	}
}

func TestEventStream_NilSafe(t *testing.T) {
	var stream *EventStream
	stream.Emit("anything", nil) // should not panic
	if err := stream.Close(); err != nil {
		t.Errorf("nil close: %v", err)
	}
}

// Sanity: confirm the marker constants this test uses haven't drifted from
// what the wrapper script in mcp_exec.go actually emits.
func TestMarkerConstants(t *testing.T) {
	got := outputMarkerFor("xy")
	if !strings.Contains(got, "OUTPUTS") || !strings.Contains(got, "xy") {
		t.Errorf("outputMarkerFor changed shape: %q", got)
	}
	got = traceMarkerFor("xy")
	if !strings.Contains(got, "CMD") || !strings.Contains(got, "xy") {
		t.Errorf("traceMarkerFor changed shape: %q", got)
	}
}

// Ensure a call with no events still produces a parseable empty file.
func TestEventStream_EmptyFile(t *testing.T) {
	withTempStateDir(t)
	s, err := NewEventStream("empty")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	data, err := os.ReadFile(CallEventsPath("empty"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

// Compile-time check: emitter satisfies io.Writer (caught at build, but
// guarded explicitly so future refactors don't accidentally break it).
var _ = func() bool {
	var _ = (&eventEmitter{}).Write
	return true
}()

// New trace-line format: <SECONDS> <LINENO> <BASH_COMMAND>
// Verifies the streaming emitter records LineNo when present.
func TestEventEmitter_TraceLineWithLineNo(t *testing.T) {
	withTempStateDir(t)

	stream, _ := NewEventStream("c-line")
	nonce := "lnonce"
	w := stream.StdoutWriter(0, "build", "h1", nonce)

	// Two trace fires from different source lines, plus a real stdout line.
	tm := traceMarkerFor(nonce)
	chunks := []string{
		tm + " 0 5 ps aux\n",
		tm + " 0 5 grep nixos\n",
		"some-output\n",
		tm + " 1 7 echo done\n",
	}
	for _, c := range chunks {
		fmt.Fprint(w, c)
	}
	w.(*eventEmitter).Flush()
	stream.Close()

	events := readEvents(t, CallEventsPath("c-line"))
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(events), events)
	}
	checkExec := func(idx, elapsed, lineNo int, cmd string) {
		t.Helper()
		ev := events[idx]
		if ev.Kind != "executing" || ev.ElapsedSec != elapsed || ev.LineNo != lineNo || ev.Cmd != cmd {
			t.Errorf("event[%d]: want executing(elapsed=%d lineno=%d cmd=%q), got kind=%s elapsed=%d lineno=%d cmd=%q",
				idx, elapsed, lineNo, cmd, ev.Kind, ev.ElapsedSec, ev.LineNo, ev.Cmd)
		}
	}
	checkExec(0, 0, 5, "ps aux")
	checkExec(1, 0, 5, "grep nixos")
	if events[2].Kind != "stdout" || events[2].Line != "some-output" {
		t.Errorf("event[2] not stdout: %+v", events[2])
	}
	checkExec(3, 1, 7, "echo done")
}

// Old trace-line format (no LINENO) should still parse, with LineNo=0.
func TestEventEmitter_TraceLineLegacy(t *testing.T) {
	withTempStateDir(t)

	stream, _ := NewEventStream("c-legacy")
	nonce := "lgnonce"
	w := stream.StdoutWriter(0, "s", "h", nonce)
	fmt.Fprint(w, traceMarkerFor(nonce)+" 3 ls -la\n")
	w.(*eventEmitter).Flush()
	stream.Close()

	events := readEvents(t, CallEventsPath("c-legacy"))
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Kind != "executing" || events[0].ElapsedSec != 3 || events[0].Cmd != "ls -la" || events[0].LineNo != 0 {
		t.Errorf("legacy parse wrong: %+v", events[0])
	}
}

func TestEventStream_ProgressSummary(t *testing.T) {
	withTempStateDir(t)

	stream, _ := NewEventStream("c-summary")
	stream.Emit("step-start", map[string]any{"step": 0, "name": "deploy"})
	stream.Emit("executing", map[string]any{"cmd": "apt-get update", "host": "h1", "elapsed_sec": 3})
	stream.Emit("stdout", map[string]any{"line": "Reading package lists..."})
	stream.Emit("stdout", map[string]any{"line": "Building dependency tree..."})
	stream.Close()

	summary := stream.ProgressSummary(10)
	if !strings.Contains(summary, "executing (10s)") {
		t.Errorf("missing elapsed header: %s", summary)
	}
	if !strings.Contains(summary, "step:deploy") {
		t.Errorf("missing step name: %s", summary)
	}
	if !strings.Contains(summary, "host:h1") {
		t.Errorf("missing host: %s", summary)
	}
	if !strings.Contains(summary, "> apt-get update") {
		t.Errorf("missing executing cmd: %s", summary)
	}
	if !strings.Contains(summary, "Reading package lists...") {
		t.Errorf("missing recent stdout: %s", summary)
	}
}

func TestEventStream_ProgressSummary_Nil(t *testing.T) {
	var stream *EventStream
	s := stream.ProgressSummary(42)
	if s != "executing (42s)" {
		t.Errorf("nil stream summary: %q", s)
	}
}

// Sanity that StepBrief JSON tags align with our wire format.
func TestStepBriefJSONShape(t *testing.T) {
	b := StepBrief{Name: "x", Action: "ssh", Hosts: []string{"h"}}
	data, _ := json.Marshal(b)
	if !strings.Contains(string(data), `"name":"x"`) {
		t.Errorf("StepBrief json: %s", data)
	}
}
