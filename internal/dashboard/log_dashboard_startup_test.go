package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
)

// TestStartupReplaysEventsDir verifies that scanCompletedEvents reads
// completed event files and classifies them correctly, and that in-progress
// files are left for the live watcher.
func TestStartupReplaysEventsDir(t *testing.T) {
	stateDir := withTempStateDir(t)
	dir := filepath.Join(stateDir, "events")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	// Completed call 1
	writeEventFile(t, filepath.Join(dir, "done1.jsonl"), []map[string]any{
		{"kind": "call-start", "ts": "2026-05-10T10:00:00Z", "session_id": "s1",
			"input": "### build\necho hello", "steps": []map[string]string{{"name": "build", "action": "local"}}},
		{"kind": "step-start", "ts": "2026-05-10T10:00:01Z", "step": 0, "name": "build"},
		{"kind": "step-end", "ts": "2026-05-10T10:00:02Z", "step": 0, "exit_code": 0, "duration_ms": 1000},
		{"kind": "call-end", "ts": "2026-05-10T10:00:02Z", "status": "ok", "duration_ms": 2000},
	})

	// Completed call 2 (with failure)
	writeEventFile(t, filepath.Join(dir, "done2.jsonl"), []map[string]any{
		{"kind": "call-start", "ts": "2026-05-10T11:00:00Z", "session_id": "s2",
			"input": "### deploy\necho go", "steps": []map[string]string{{"name": "deploy", "action": "ssh"}}},
		{"kind": "step-start", "ts": "2026-05-10T11:00:01Z", "step": 0, "name": "deploy"},
		{"kind": "step-end", "ts": "2026-05-10T11:00:03Z", "step": 0, "exit_code": 1, "duration_ms": 2000},
		{"kind": "call-end", "ts": "2026-05-10T11:00:03Z", "status": "ok", "duration_ms": 3000},
	})

	// In-progress call (no call-end)
	writeEventFile(t, filepath.Join(dir, "running.jsonl"), []map[string]any{
		{"kind": "call-start", "ts": "2026-05-10T12:00:00Z", "session_id": "s3",
			"input": "### check\nuptime", "steps": []map[string]string{{"name": "check", "action": "local"}}},
		{"kind": "step-start", "ts": "2026-05-10T12:00:01Z", "step": 0, "name": "check"},
	})

	entries := scanCompletedEvents()

	// Should only have 2 completed, not the in-progress one.
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	// Newest first: done2 (11:00) before done1 (10:00).
	if entries[0].ID != "done2" {
		t.Errorf("entries[0].ID = %q, want done2", entries[0].ID)
	}
	if entries[1].ID != "done1" {
		t.Errorf("entries[1].ID = %q, want done1", entries[1].ID)
	}

	// Verify done2 has fail status from exit code.
	if entries[0].CallStatus() != "fail" {
		t.Errorf("done2 status = %q, want fail", entries[0].CallStatus())
	}
	if entries[0].DurationMs != 3000 {
		t.Errorf("done2 duration = %d, want 3000", entries[0].DurationMs)
	}

	// Verify done1 is ok.
	if entries[1].CallStatus() != "ok" {
		t.Errorf("done1 status = %q, want ok", entries[1].CallStatus())
	}
}

// TestCrashRecovery verifies that in-progress calls with dead pids are
// classified as interrupted.
func TestCrashRecovery(t *testing.T) {
	stateDir := withTempStateDir(t)
	dir := filepath.Join(stateDir, "events")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	m := ldInitialModel()

	// Simulate an in-progress call with a dead pid (pid=999999 should not exist).
	a := &activeCall{ID: "crashed", CurrentStep: -1}
	a.Apply(mcp.LiveEvent{
		Kind:      "call-start",
		Ts:        time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		SessionID: "s-crash",
		Input:     "### test\necho boom",
		Steps:     []mcp.StepBrief{{Name: "test", Action: "local"}},
		Pid:       999999, // non-existent pid
	})
	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 0, Name: "test"})
	m.active["crashed"] = a
	m.activeIDs = append(m.activeIDs, "crashed")

	m.classifyInterrupted()

	// Should be moved from active to entries.
	if _, ok := m.active["crashed"]; ok {
		t.Error("crashed call still in active after classifyInterrupted")
	}
	if len(m.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.entries))
	}
	if m.entries[0].Error != "interrupted" {
		t.Errorf("entry error = %q, want interrupted", m.entries[0].Error)
	}
	if m.entries[0].ID != "crashed" {
		t.Errorf("entry ID = %q, want crashed", m.entries[0].ID)
	}
}

// TestInterruptedDuration verifies that interrupted calls use last-event
// timestamp for duration, not time.Since(StartedAt).
func TestInterruptedDuration(t *testing.T) {
	withTempStateDir(t)

	m := ldInitialModel()

	start := time.Date(2025, 12, 31, 18, 27, 0, 0, time.UTC)
	lastEvent := time.Date(2025, 12, 31, 18, 27, 5, 0, time.UTC) // 5s later

	a := &activeCall{ID: "old-call", CurrentStep: -1}
	a.Apply(mcp.LiveEvent{
		Kind:      "call-start",
		Ts:        start,
		SessionID: "s-old",
		Input:     "### test\necho hi",
		Steps:     []mcp.StepBrief{{Name: "test", Action: "local"}},
		Pid:       999999,
	})
	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 0, Name: "test", Ts: start})
	a.Apply(mcp.LiveEvent{Kind: "stdout", Step: 0, Line: "hi", Ts: lastEvent})
	m.active["old-call"] = a
	m.activeIDs = append(m.activeIDs, "old-call")

	m.classifyInterrupted()

	if len(m.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.entries))
	}
	got := m.entries[0].DurationMs
	want := int64(5000) // 5 seconds
	if got != want {
		t.Errorf("DurationMs = %d, want %d (should use last event, not time.Now())", got, want)
	}
}

// TestInterruptedDurationNoEvents verifies interrupted calls with no tail
// events fall back to zero duration instead of overflowing.
func TestInterruptedDurationNoEvents(t *testing.T) {
	withTempStateDir(t)

	m := ldInitialModel()

	// activeCall with no call-start → zero StartedAt, empty tail
	a := &activeCall{ID: "no-start", CurrentStep: -1}
	m.active["no-start"] = a
	m.activeIDs = append(m.activeIDs, "no-start")

	m.classifyInterrupted()

	if len(m.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.entries))
	}
	got := m.entries[0].DurationMs
	if got != 0 {
		t.Errorf("DurationMs = %d, want 0 (no events, should not overflow)", got)
	}
}

// TestCallStartIncludesPid verifies the daemon emits pid in call-start.
func TestCallStartIncludesPid(t *testing.T) {
	withTempStateDir(t)

	stream, err := mcp.NewEventStream("pid-test")
	if err != nil {
		t.Fatal(err)
	}
	stream.Emit("call-start", map[string]any{
		"session_id": "sess",
		"input":      "### x\necho",
		"steps":      []mcp.StepBrief{{Name: "x", Action: "local"}},
		"pid":        os.Getpid(),
	})
	stream.Close()

	events, err := readEventsFile(mcp.CallEventsPath("pid-test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", events[0].Pid, os.Getpid())
	}
}

func writeEventFile(t *testing.T, path string, events []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ev := range events {
		data, _ := json.Marshal(ev)
		f.Write(append(data, '\n'))
	}
}
