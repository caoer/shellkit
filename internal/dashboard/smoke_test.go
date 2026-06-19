// E2E smoke test for the live-events pipeline using ActionLocal so it has no
// SSH dependency. Verifies that emitting envelope + per-step events plus a
// live tee on stdout produces a well-formed JSONL file with executing,
// stdout, and step-boundary events.

package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
)

func TestSmoke_E2E_LocalExecution_ProducesEventsFile(t *testing.T) {
	withTempStateDir(t)

	stream, err := mcp.NewEventStream("smoke1")
	if err != nil {
		t.Fatal(err)
	}
	stream.Emit("call-start", map[string]any{"steps": []mcp.StepBrief{{Name: "x", Action: "local"}}})

	ctx := mcp.ContextWithEventStream(context.Background(), stream)
	store, _ := mcp.NewOutputStore(nil)
	exec := mcp.NewExecutor(store, nil)

	step := mcp.Step{Name: "x", Action: mcp.ActionLocal, Body: "echo hello\necho world\nfor i in 1 2 3; do echo $i; done"}
	results, err := exec.Execute(ctx, []mcp.Step{step})
	if err != nil {
		t.Fatal(err)
	}
	stream.Emit("call-end", map[string]any{"status": "ok", "duration_ms": int64(0)})
	stream.Close()

	if results[0].ExitCode != 0 {
		t.Fatalf("exit: %d, stderr: %s", results[0].ExitCode, results[0].Stderr)
	}

	path := filepath.Join(mcp.EventsDir(), "smoke1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	checks := []string{
		`"kind":"call-start"`,
		`"kind":"step-start"`,
		`"kind":"step-end"`,
		`"kind":"call-end"`,
		`"line":"hello"`,
		`"line":"world"`,
		`"line":"1"`,
		`"line":"2"`,
		`"line":"3"`,
		// trace markers should be transformed into executing events
		`"kind":"executing"`,
	}
	for _, want := range checks {
		if !strings.Contains(content, want) {
			t.Errorf("events file missing %q\n--- file ---\n%s", want, content)
		}
	}
	// Sanity: we wrote events while the process was running, not all at the end.
	// Approximate by checking the file's mtime is recent.
	if st, _ := os.Stat(path); time.Since(st.ModTime()) > 5*time.Second {
		t.Errorf("file mtime suspiciously old")
	}
}

// TestSmoke_E2E_LiveWatcherSeesEventsBeforeCallEnds verifies the full pipeline:
// Executor → EventStream → JSONL file → liveWatcher → channel. Events from a
// long-running local command must arrive on the watcher channel BEFORE the
// command finishes. This catches the macOS kqueue WRITE-event gap: if polling
// is broken, events only arrive after the file is closed at call-end.
func TestSmoke_E2E_LiveWatcherSeesEventsBeforeCallEnds(t *testing.T) {
	withTempStateDir(t)

	out := make(chan ldLiveEventMsg, 256)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatal(err)
	}
	defer lw.Close()

	// Create EventStream AFTER watcher starts so CREATE event is live.
	stream, err := mcp.NewEventStream("live-e2e")
	if err != nil {
		t.Fatal(err)
	}
	stream.Emit("call-start", map[string]any{
		"steps": []mcp.StepBrief{{Name: "ticker", Action: "local"}},
	})

	// Drain the call-start from the CREATE/WRITE fsnotify event.
	got := drain(out, 1, 2*time.Second)
	if len(got) != 1 || got[0].Event.Kind != "call-start" {
		t.Fatalf("expected call-start from live CREATE, got %d events", len(got))
	}

	ctx := mcp.ContextWithEventStream(context.Background(), stream)
	store, _ := mcp.NewOutputStore(nil)
	executor := mcp.NewExecutor(store, nil)

	// Command prints 6 lines with 0.5s gaps → runs ~3 seconds total.
	step := mcp.Step{
		Name:   "ticker",
		Action: mcp.ActionLocal,
		Body:   `for i in 1 2 3 4 5 6; do echo "tick-$i"; sleep 0.5; done`,
	}

	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		executor.Execute(ctx, []mcp.Step{step})
		stream.Emit("call-end", map[string]any{"status": "ok"})
		stream.Close()
	}()

	// We must see stdout events BEFORE the command finishes (~3s).
	// Wait at most 2s for the first stdout "tick-1" event.
	deadline := time.After(2 * time.Second)
	var firstStdout *ldLiveEventMsg
	for firstStdout == nil {
		select {
		case ev := <-out:
			if ev.Event.Kind == "stdout" {
				firstStdout = &ev
			}
		case <-deadline:
			t.Fatal("no stdout event within 2s — pollActive not delivering live events")
		}
	}

	// Verify the command is still running when we got the first event.
	select {
	case <-execDone:
		t.Fatal("executor finished before we received the first live stdout event — events are batched, not live")
	default:
		// Good — command still running, events arriving live.
	}

	if firstStdout.Event.Line != "tick-1" {
		t.Errorf("first stdout line: %q, want tick-1", firstStdout.Event.Line)
	}

	// Wait for completion.
	<-execDone

	// Drain remaining events and verify we got all 6 ticks.
	remaining := drain(out, 100, 2*time.Second)
	allStdout := []string{firstStdout.Event.Line}
	for _, ev := range remaining {
		if ev.Event.Kind == "stdout" {
			allStdout = append(allStdout, ev.Event.Line)
		}
	}
	if len(allStdout) < 6 {
		t.Errorf("got %d stdout lines, want 6: %v", len(allStdout), allStdout)
	}
}
