package dashboard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
)

func TestActiveCall_ApplyCallStartStepBoundariesEnd(t *testing.T) {
	a := &activeCall{ID: "c1", CurrentStep: -1}

	a.Apply(mcp.LiveEvent{
		Kind:      "call-start",
		SessionID: "sess",
		Input:     "### x\nfoo",
		Steps: []mcp.StepBrief{
			{Name: "s1", Action: "ssh", Hosts: []string{"h1"}},
			{Name: "s2", Action: "local"},
		},
	})
	if a.SessionID != "sess" {
		t.Errorf("session: %q", a.SessionID)
	}
	if len(a.StepStatuses) != 2 {
		t.Fatalf("steps: %d", len(a.StepStatuses))
	}
	if a.CurrentStep != -1 {
		t.Errorf("current: %d", a.CurrentStep)
	}

	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 0, Name: "s1", Hosts: []string{"h1"}})
	if !a.StepStatuses[0].Started || a.CurrentStep != 0 {
		t.Errorf("step 0 not marked started: %+v", a.StepStatuses[0])
	}

	a.Apply(mcp.LiveEvent{Kind: "executing", Step: 0, Host: "h1", ElapsedSec: 3, Cmd: "df -h /"})
	if a.ExecutingCmd != "df -h /" || a.ExecutingHost != "h1" || a.ExecutingElapsed != 3 {
		t.Errorf("executing not captured: %+v", a)
	}
	if len(a.Tail) != 1 || a.Tail[0].Stream != "executing" || a.Tail[0].Text != "df -h /" {
		t.Errorf("executing not appended to tail: %+v", a.Tail)
	}

	a.Apply(mcp.LiveEvent{Kind: "stdout", Step: 0, Host: "h1", Line: "Filesystem"})
	if len(a.Tail) != 2 || a.Tail[1].Stream != "stdout" {
		t.Errorf("stdout tail: %+v", a.Tail)
	}

	a.Apply(mcp.LiveEvent{Kind: "step-end", Step: 0, Name: "s1", ExitCode: 0, DurationMs: 1234})
	if !a.StepStatuses[0].Ended || a.StepStatuses[0].DurationMs != 1234 {
		t.Errorf("step 0 not ended properly: %+v", a.StepStatuses[0])
	}

	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 1, Name: "s2"})
	// Cross-step transitions should clear the executing snapshot so the UI
	// doesn't show step-1's status with step-0's command.
	if a.ExecutingCmd != "" {
		t.Errorf("executing should reset on step-start, got %q", a.ExecutingCmd)
	}

	a.Apply(mcp.LiveEvent{Kind: "call-end", Status: "ok", DurationMs: 5000})
	if !a.Done || a.DoneStatus != "ok" {
		t.Errorf("not marked done: %+v", a)
	}
}

func TestActiveCall_TailRingBuffer(t *testing.T) {
	a := &activeCall{ID: "c"}
	for i := 0; i < maxTail+50; i++ {
		a.Apply(mcp.LiveEvent{Kind: "stdout", Line: fmt.Sprintf("line-%d", i)})
	}
	if len(a.Tail) != maxTail {
		t.Errorf("tail length = %d, want %d", len(a.Tail), maxTail)
	}
	// First retained line should be line-50 (i.e., the buffer dropped 0..49).
	if a.Tail[0].Text != fmt.Sprintf("line-%d", 50) {
		t.Errorf("oldest retained = %q, want line-50", a.Tail[0].Text)
	}
}

func TestActiveCall_AsCallEntryDuration(t *testing.T) {
	a := &activeCall{
		ID:        "x",
		StartedAt: time.Now().Add(-2 * time.Second),
	}
	e := a.asCallEntry()
	if e.ID != "x" {
		t.Errorf("id: %q", e.ID)
	}
	if e.DurationMs < 1500 {
		t.Errorf("duration too small: %d ms", e.DurationMs)
	}
}

// Completed files (ending in call-end) should not replay at startup. Their
// offsets are seeded to EOF so only future writes (which shouldn't happen)
// would emit. This is the optimization from commit 049d372 — without it,
// 27k historical events would starve the live loop.
func TestLiveWatcher_CompletedFilesNotBackfilled(t *testing.T) {
	withTempStateDir(t)

	dir := mcp.EventsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	completed := filepath.Join(dir, "done.jsonl")
	completedData := `{"ts":"2026-05-09T08:00:00Z","kind":"call-start","steps":[{"name":"s","action":"ssh"}]}` + "\n" +
		`{"ts":"2026-05-09T08:00:01Z","kind":"step-start","step":0,"name":"s"}` + "\n" +
		`{"ts":"2026-05-09T08:00:02Z","kind":"step-end","step":0,"exit_code":0}` + "\n" +
		`{"ts":"2026-05-09T08:00:02Z","kind":"call-end","status":"ok"}` + "\n"
	if err := os.WriteFile(completed, []byte(completedData), 0644); err != nil {
		t.Fatal(err)
	}

	out := make(chan ldLiveEventMsg, 16)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatalf("startLiveWatcher: %v", err)
	}
	defer lw.Close()

	// Completed file should not emit anything.
	got := drain(out, 1, 500*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("completed file should not backfill, got %d events", len(got))
	}

	lw.mu.Lock()
	off := lw.offsets["done"]
	lw.mu.Unlock()
	if off != int64(len(completedData)) {
		t.Errorf("offset: got %d, want %d", off, len(completedData))
	}
}

// In-progress files (no call-end yet) must replay at startup so the
// dashboard reconstructs active-call state — otherwise selecting that call
// shows "(no steps)" until eviction. This is the flash-bug fix.
func TestLiveWatcher_InProgressFilesBackfilled(t *testing.T) {
	withTempStateDir(t)

	dir := mcp.EventsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	inprog := filepath.Join(dir, "running.jsonl")
	inprogData := `{"ts":"2026-05-09T08:00:00Z","kind":"call-start","steps":[{"name":"s","action":"ssh"}]}` + "\n" +
		`{"ts":"2026-05-09T08:00:01Z","kind":"step-start","step":0,"name":"s"}` + "\n"
	if err := os.WriteFile(inprog, []byte(inprogData), 0644); err != nil {
		t.Fatal(err)
	}

	out := make(chan ldLiveEventMsg, 16)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatalf("startLiveWatcher: %v", err)
	}
	defer lw.Close()

	// In-progress file should replay both events so active state reconstructs.
	got := drain(out, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("in-progress file should backfill, got %d events", len(got))
	}
	if got[0].Event.Kind != "call-start" {
		t.Errorf("event[0] kind = %q, want call-start", got[0].Event.Kind)
	}
	if got[1].Event.Kind != "step-start" {
		t.Errorf("event[1] kind = %q, want step-start", got[1].Event.Kind)
	}

	// After backfill, offset should be at EOF so subsequent appends don't dup.
	// (Poll runs at 500ms intervals; give it time to drain.)
	time.Sleep(700 * time.Millisecond)
	lw.mu.Lock()
	off := lw.offsets["running"]
	lw.mu.Unlock()
	if off != int64(len(inprogData)) {
		t.Errorf("offset after backfill: got %d, want %d", off, len(inprogData))
	}

	// Append a new event — should arrive exactly once (no duplicate).
	f, err := os.OpenFile(inprog, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"ts":"2026-05-09T08:00:02Z","kind":"stdout","line":"hello"}` + "\n")
	f.Close()

	more := drain(out, 1, 2*time.Second)
	if len(more) != 1 {
		t.Fatalf("after append: got %d events, want 1", len(more))
	}
	if more[0].Event.Kind != "stdout" || more[0].Event.Line != "hello" {
		t.Errorf("appended event: %+v", more[0].Event)
	}
}

// Brand-new file (Create event) — still works after the in-progress backfill
// change. Tests that the fsnotify Create path is unaffected.
func TestLiveWatcher_BrandNewFile(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	out := make(chan ldLiveEventMsg, 16)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatalf("startLiveWatcher: %v", err)
	}
	defer lw.Close()

	newPath := filepath.Join(dir, "newcall.jsonl")
	os.WriteFile(newPath, []byte(`{"ts":"2026-05-09T08:01:00Z","kind":"call-start"}`+"\n"), 0644)
	created := drain(out, 1, 2*time.Second)
	if len(created) != 1 {
		t.Fatalf("after create: got %d events, want 1", len(created))
	}
	if created[0].CallID != "newcall" || created[0].Event.Kind != "call-start" {
		t.Errorf("created event: %+v", created[0])
	}
}

// drain reads up to n events from ch, waiting at most total. Returns whatever
// it collected (may be fewer than n).
func drain(ch <-chan ldLiveEventMsg, n int, total time.Duration) []ldLiveEventMsg {
	deadline := time.Now().Add(total)
	var out []ldLiveEventMsg
	for len(out) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-time.After(remaining):
			return out
		}
	}
	return out
}

func TestLiveWatcher_OffsetIsPerCall(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	out := make(chan ldLiveEventMsg, 32)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatal(err)
	}
	defer lw.Close()

	// Two parallel files. Offsets must not bleed.
	go func() {
		f, _ := os.Create(filepath.Join(dir, "alpha.jsonl"))
		f.WriteString(`{"kind":"call-start"}` + "\n")
		f.WriteString(`{"kind":"stdout","line":"a1"}` + "\n")
		f.Close()
	}()
	go func() {
		f, _ := os.Create(filepath.Join(dir, "beta.jsonl"))
		f.WriteString(`{"kind":"call-start"}` + "\n")
		f.WriteString(`{"kind":"stdout","line":"b1"}` + "\n")
		f.Close()
	}()

	got := drain(out, 4, 2*time.Second)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4", len(got))
	}

	// Verify per-call separation.
	byCall := make(map[string][]string)
	for _, ev := range got {
		if ev.Event.Kind == "stdout" {
			byCall[ev.CallID] = append(byCall[ev.CallID], ev.Event.Line)
		}
	}
	if got1 := byCall["alpha"]; len(got1) != 1 || got1[0] != "a1" {
		t.Errorf("alpha: %v", got1)
	}
	if got2 := byCall["beta"]; len(got2) != 1 || got2[0] != "b1" {
		t.Errorf("beta: %v", got2)
	}
}

// TestLiveWatcher_PollDetectsOpenFileAppend simulates the real MCP daemon
// scenario: a file is created and kept open while events are written
// incrementally. On macOS, kqueue doesn't fire WRITE events for an open file
// being appended to, so the watcher must detect growth via pollActive().
func TestLiveWatcher_PollDetectsOpenFileAppend(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	out := make(chan ldLiveEventMsg, 64)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatal(err)
	}
	defer lw.Close()

	// Simulate EventStream: open file, keep it open for the duration.
	path := filepath.Join(dir, "opencall.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Write initial event (call-start). The CREATE fsnotify event should
	// trigger the first scanFile.
	f.WriteString(`{"kind":"call-start","steps":[{"name":"s","action":"local"}]}` + "\n")

	got := drain(out, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("initial event: got %d, want 1", len(got))
	}
	if got[0].Event.Kind != "call-start" {
		t.Errorf("initial event kind: %q", got[0].Event.Kind)
	}
	if got[0].CallID != "opencall" {
		t.Errorf("call id: %q", got[0].CallID)
	}

	// Now write more events to the STILL-OPEN file. On macOS, fsnotify won't
	// fire WRITE events for this. The 500ms poll ticker must detect the growth.
	f.WriteString(`{"kind":"stdout","line":"line1"}` + "\n")

	got2 := drain(out, 1, 2*time.Second)
	if len(got2) != 1 {
		t.Fatalf("poll-detected event: got %d, want 1 (pollActive not working)", len(got2))
	}
	if got2[0].Event.Kind != "stdout" || got2[0].Event.Line != "line1" {
		t.Errorf("poll event: %+v", got2[0].Event)
	}

	// Write two more events rapidly. Poll should batch them in one scan.
	f.WriteString(`{"kind":"stdout","line":"line2"}` + "\n")
	f.WriteString(`{"kind":"stdout","line":"line3"}` + "\n")

	got3 := drain(out, 2, 2*time.Second)
	if len(got3) != 2 {
		t.Fatalf("batch poll events: got %d, want 2", len(got3))
	}
}

// TestLiveWatcher_CrossProcessPollDetection simulates the real production
// scenario: a SEPARATE PROCESS writes to the JSONL file (like the MCP daemon),
// while the liveWatcher in THIS process detects the writes via polling. This
// catches any cross-process visibility issues with os.Stat on macOS.
func TestLiveWatcher_CrossProcessPollDetection(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	out := make(chan ldLiveEventMsg, 64)
	lw, err := startLiveWatcher(out)
	if err != nil {
		t.Fatal(err)
	}
	defer lw.Close()

	jsonlPath := filepath.Join(dir, "cross-proc.jsonl")

	// Spawn a child process that writes events with sleeps between them.
	// This simulates the MCP daemon writing to the JSONL file.
	script := fmt.Sprintf(`
import json, time, sys
path = %q
f = open(path, 'w')
f.write(json.dumps({"kind": "call-start", "steps": [{"name": "s", "action": "local"}]}) + '\n')
f.flush()
for i in range(1, 6):
    time.sleep(0.4)
    f.write(json.dumps({"kind": "stdout", "line": "tick-%%d" %% i}) + '\n')
    f.flush()
time.sleep(0.2)
f.write(json.dumps({"kind": "call-end", "status": "ok"}) + '\n')
f.close()
`, jsonlPath)

	cmd := exec.Command("python3", "-c", script)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()

	// Must see call-start + at least 2 stdout events within 4 seconds.
	// The child takes ~2.2s to write 5 ticks, so 4s is generous.
	deadline := time.After(4 * time.Second)
	var kinds []string
	stdoutCount := 0
	for stdoutCount < 2 {
		select {
		case ev := <-out:
			kinds = append(kinds, ev.Event.Kind)
			if ev.Event.Kind == "stdout" {
				stdoutCount++
			}
		case <-deadline:
			t.Fatalf("only got %d stdout events in 4s (kinds: %v) — poll not detecting cross-process writes", stdoutCount, kinds)
		}
	}

	// Verify the command is still running (events arrived live, not batched).
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		t.Error("child exited before we collected live events — writes were batched")
	}

	cmd.Wait()
}

// TestChannelDropDataOnly saturates a tiny channel with stdout events and
// verifies that: (a) control events still arrive in order, (b) the dropped
// counter is positive (some data events were shed).
func TestChannelDropDataOnly(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	// Tiny channel — will fill up fast under data flood.
	ch := make(chan ldLiveEventMsg, 4)
	dropped := &atomic.Uint64{}
	lastDrop := &atomic.Int64{}

	lw := &liveWatcher{
		dir:           dir,
		offsets:       make(map[string]int64),
		sealed:        make(map[string]bool),
		out:           ch,
		droppedEvents: dropped,
		lastDropAt:    lastDrop,
	}

	// Build a JSONL file: call-start, 10000 stdout events, step-end, call-end.
	var lines []string
	lines = append(lines, `{"kind":"call-start","steps":[{"name":"s","action":"local"}]}`)
	lines = append(lines, `{"kind":"step-start","step":0,"name":"s"}`)
	for i := 0; i < 10000; i++ {
		lines = append(lines, fmt.Sprintf(`{"kind":"stdout","step":0,"line":"line-%d"}`, i))
	}
	lines = append(lines, `{"kind":"step-end","step":0,"exit_code":0}`)
	lines = append(lines, `{"kind":"call-end","status":"ok"}`)

	path := filepath.Join(dir, "flood.jsonl")
	data := strings.Join(lines, "\n") + "\n"
	os.WriteFile(path, []byte(data), 0644)

	// Drain channel concurrently while scanFile runs so control events
	// can make progress (they block on send).
	var received []ldLiveEventMsg
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range ch {
			mu.Lock()
			received = append(received, msg)
			mu.Unlock()
		}
	}()

	lw.scanFile(path)
	close(ch)
	<-done

	// Verify control events all arrived in order.
	mu.Lock()
	defer mu.Unlock()

	controlKinds := []string{}
	for _, msg := range received {
		if isControlEvent(msg.Event.Kind) {
			controlKinds = append(controlKinds, msg.Event.Kind)
		}
	}
	expected := []string{"call-start", "step-start", "step-end", "call-end"}
	if len(controlKinds) != len(expected) {
		t.Fatalf("control events: got %v, want %v", controlKinds, expected)
	}
	for i, k := range controlKinds {
		if k != expected[i] {
			t.Errorf("control[%d]: got %q, want %q", i, k, expected[i])
		}
	}

	// Some data events should have been dropped.
	if dropped.Load() == 0 {
		t.Errorf("expected dropped > 0, got 0 (channel never saturated?)")
	}
	if lastDrop.Load() == 0 {
		t.Errorf("lastDropAt should be set")
	}
	t.Logf("total events received: %d, dropped: %d", len(received), dropped.Load())
}

// TestControlEventsNeverDrop floods data events while a single step-end is
// queued; verifies step-end still applies to the active call.
func TestControlEventsNeverDrop(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	// Tiny channel.
	ch := make(chan ldLiveEventMsg, 2)
	dropped := &atomic.Uint64{}
	lastDrop := &atomic.Int64{}

	lw := &liveWatcher{
		dir:           dir,
		offsets:       make(map[string]int64),
		sealed:        make(map[string]bool),
		out:           ch,
		droppedEvents: dropped,
		lastDropAt:    lastDrop,
	}

	// File: call-start, step-start, 5000 stdout, step-end, call-end.
	var lines []string
	lines = append(lines, `{"kind":"call-start","steps":[{"name":"build","action":"local"}]}`)
	lines = append(lines, `{"kind":"step-start","step":0,"name":"build"}`)
	for i := 0; i < 5000; i++ {
		lines = append(lines, fmt.Sprintf(`{"kind":"stdout","step":0,"line":"out-%d"}`, i))
	}
	lines = append(lines, `{"kind":"step-end","step":0,"exit_code":42,"duration_ms":1234}`)
	lines = append(lines, `{"kind":"call-end","status":"fail"}`)

	path := filepath.Join(dir, "ctrl-test.jsonl")
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	// Apply received events to an activeCall — if step-end arrives, the step
	// should be marked Ended.
	a := &activeCall{ID: "ctrl-test", CurrentStep: -1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range ch {
			a.Apply(msg.Event)
		}
	}()

	lw.scanFile(path)
	close(ch)
	<-done

	if !a.Done {
		t.Error("call-end not applied — control event was dropped")
	}
	if a.DoneStatus != "fail" {
		t.Errorf("done status: got %q, want %q", a.DoneStatus, "fail")
	}
	if len(a.StepStatuses) == 0 {
		t.Fatal("no step statuses — call-start dropped")
	}
	if !a.StepStatuses[0].Ended {
		t.Error("step-end not applied — control event was dropped")
	}
	if a.StepStatuses[0].ExitCode != 42 {
		t.Errorf("exit code: got %d, want 42", a.StepStatuses[0].ExitCode)
	}

	if dropped.Load() == 0 {
		t.Error("expected some data drops with channel size 2")
	}
	t.Logf("dropped %d data events, all control events survived", dropped.Load())
}

// Sanity: the watcher's offsets map is goroutine-safe (we mutate from loop +
// scanFile which can be called concurrently across files).
func TestLiveWatcher_ConcurrentScans(t *testing.T) {
	withTempStateDir(t)
	dir := mcp.EventsDir()
	os.MkdirAll(dir, 0755)

	lw := &liveWatcher{dir: dir, offsets: make(map[string]int64), out: make(chan ldLiveEventMsg, 1024)}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			path := filepath.Join(dir, fmt.Sprintf("c%d.jsonl", i))
			os.WriteFile(path, []byte(`{"kind":"stdout","line":"x"}`+"\n"), 0644)
			lw.scanFile(path)
		}()
	}
	wg.Wait()

	if len(lw.offsets) != 20 {
		t.Errorf("offsets: %d", len(lw.offsets))
	}
}
