package dashboard

import (
	"bufio"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	"github.com/fsnotify/fsnotify"
)

// ─────────────────────────────────────────────────────────────────────────────
// Live event watcher
//
// The dashboard fsnotify-watches <mcp.StateDir>/events/ for per-call JSONL files
// produced by the MCP daemon. Each new line in those files becomes a LiveEvent
// and is delivered to the bubbletea program as a ldLiveEventMsg.
//
// The watcher tracks a byte offset per file so partial reads are cheap and
// idempotent: any "modified" notification triggers a re-read from offset to
// EOF; lines that span multiple writes get glued together by bufio.Scanner.
// ─────────────────────────────────────────────────────────────────────────────

// activeCall is the in-memory live view of one in-flight call. On call-end,
// the dashboard auto-transitions these into static CallEntry records.
type activeCall struct {
	ID        string
	StartedAt time.Time
	SessionID string
	Input     string
	Steps     []mcp.StepBrief
	Pid       int // daemon pid from call-start; 0 if not present

	CurrentStep  int // -1 before any step-start arrives
	StepStatuses []stepLiveStatus

	Done       bool
	DoneStatus string

	// Most recent executing trace marker, if any.
	ExecutingCmd     string
	ExecutingHost    string
	ExecutingElapsed int

	// Rolling tail of lines (stdout, stderr, executing). Caller bounds size.
	Tail          []tailLine
	unlimitedTail bool // skip maxTail cap (used by static replay)

	// Cached DSL parse result — Input never changes after call-start, so
	// parseStepBodies only needs to run once per call instead of every render.
	parsedBodies [][]wfSourceLine

	// Incremental waterfall projection — maintained by Apply(), consumed
	// by buildLiveWaterfall(). Replaces per-render tail-replay.
	waterfall  []waterfallStep
	wfLineIdx  []int       // per-step current line index; -1 = not started
	firstAnyTs []time.Time // per-step; first event of ANY stream
	lastTs     []time.Time // per-step; fallback when DurationMs==0
	version    atomic.Uint64
}

type stepLiveStatus struct {
	Name       string
	Action     string
	Hosts      []string
	Params     map[string]string
	Started    bool
	Ended      bool
	ExitCode   int
	DurationMs int64
}

type tailLine struct {
	Stream string // "stdout" | "stderr" | "executing"
	Step   int
	Host   string
	Text   string
	Ts     time.Time
	LineNo int // source LINENO from DEBUG trap; only set on "executing" lines
}

// maxTail caps the per-call rolling tail so memory stays bounded for chatty
// calls. The dashboard only ever renders a viewport-worth anyway.
const maxTail = 500

// ldLiveEventMsg is sent to the tea.Program for each parsed event.
type ldLiveEventMsg struct {
	CallID string
	Event  mcp.LiveEvent
}

// ldLiveDoneMsg signals the watcher goroutine has exited (e.g. on dashboard
// shutdown). Currently informational only.
type ldLiveDoneMsg struct{}

// isControlEvent reports whether the event kind is a state-transition event
// that MUST reach the bubbletea Update loop. Dropping these corrupts the
// active-call projection (step never finalises, call never ends).
func isControlEvent(kind string) bool {
	switch kind {
	case "call-start", "step-start", "step-end", "call-end", "executing":
		return true
	}
	return false
}

// liveWatcher reads events/<id>.jsonl files and pushes parsed events onto a
// channel. The dashboard's bubbletea Cmd reads from that channel.
type liveWatcher struct {
	dir     string
	watcher *fsnotify.Watcher
	out     chan<- ldLiveEventMsg

	mu      sync.Mutex
	offsets map[string]int64
	sealed  map[string]bool // completed calls — skip in pollActive

	// Backpressure counters — shared with ldModel for lag indicator.
	// Incremented in scanFile when a data event is dropped (non-blocking send).
	droppedEvents *atomic.Uint64
	lastDropAt    *atomic.Int64 // unix nano
}

// startLiveWatcher creates and starts the watcher in a goroutine. Events flow
// onto out. Returns immediately; the goroutine runs until ctx-equivalent
// (we use the watcher.Close() signal — caller closes by closing the watcher).
func startLiveWatcher(out chan<- ldLiveEventMsg) (*liveWatcher, error) {
	dir := mcp.EventsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}

	lw := &liveWatcher{
		dir:     dir,
		watcher: w,
		out:     out,
		offsets: make(map[string]int64),
		sealed:  make(map[string]bool),
	}

	// Seed offsets from existing files so the live loop doesn't re-emit
	// old events for COMPLETED calls. For IN-PROGRESS calls (no call-end
	// line yet), seed offset to 0 so the live loop replays from the start
	// — that way we get the call-start event and the active record
	// reconstructs properly, instead of showing "(no steps)" until eviction.
	//
	// Cost: small. In-progress calls are typically <10; replaying their
	// events through the 1024-buffer channel is fast. The 27k-events-for-
	// completed-calls case from commit 049d372 stays optimized because
	// we still seed those to EOF.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			callID := callIDFromPath(path)
			if callID == "" {
				continue
			}
			if eventsFileIsCompleted(path, info.Size()) {
				// Completed: store offset at EOF so scanFile won't re-read,
				// and seal so pollActive won't stat this file every 500ms.
				lw.offsets[callID] = info.Size()
				lw.sealed[callID] = true
			} else {
				// In-progress: offset 0 so pollActive scans from the start.
				lw.offsets[callID] = 0
			}
		}
	}

	// Trigger an initial scan of in-progress files so we don't have to wait
	// for the first poll tick (500ms) to see them.
	go lw.pollActive()

	go lw.loop()
	return lw, nil
}

func (lw *liveWatcher) Close() error {
	if lw == nil || lw.watcher == nil {
		return nil
	}
	return lw.watcher.Close()
}

func (lw *liveWatcher) loop() {
	// macOS kqueue doesn't reliably fire WRITE events for an open file
	// being appended to — it fires on CREATE and close but not on each
	// write() syscall. Poll active files as fallback.
	// 150ms keeps output feeling responsive rather than stepwise; cost is
	// negligible (stat() on active files only — 0 when idle).
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case ev, ok := <-lw.watcher.Events:
			if !ok {
				return
			}
			if !strings.HasSuffix(ev.Name, ".jsonl") {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			lw.scanFile(ev.Name)
		case err, ok := <-lw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("live watcher error: %v", err)
		case <-poll.C:
			lw.pollActive()
		}
	}
}

// pollActive re-scans any event file that has grown since the last read.
// Only stats unsealed (in-progress) files — completed calls are skipped
// entirely, keeping poll cost proportional to active call count, not history.
func (lw *liveWatcher) pollActive() {
	lw.mu.Lock()
	type activeFile struct {
		id  string
		off int64
	}
	active := make([]activeFile, 0, len(lw.offsets))
	for id, off := range lw.offsets {
		if !lw.sealed[id] {
			active = append(active, activeFile{id, off})
		}
	}
	lw.mu.Unlock()

	for _, a := range active {
		path := mcp.CallEventsPath(a.id)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() > a.off {
			lw.scanFile(path)
		}
	}
}

// scanFile reads from the saved offset to EOF, parses each new line, and
// pushes events. If the file shrank (shouldn't happen for our append-only
// usage, but be defensive) we reset to 0 and re-read.
func (lw *liveWatcher) scanFile(path string) {
	callID := callIDFromPath(path)
	if callID == "" {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		// Race: file may have been deleted. Drop offset.
		lw.mu.Lock()
		delete(lw.offsets, callID)
		lw.mu.Unlock()
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return
	}

	lw.mu.Lock()
	off := lw.offsets[callID]
	if off > st.Size() {
		off = 0 // truncated/rotated
	}
	lw.mu.Unlock()

	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	bytesRead := int64(0)
	sawCallEnd := false
	for sc.Scan() {
		line := sc.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline
		ev, err := mcp.ParseLiveEvent(line)
		if err != nil {
			continue
		}
		if ev.Kind == "call-end" {
			sawCallEnd = true
		}
		msg := ldLiveEventMsg{CallID: callID, Event: ev}
		if isControlEvent(ev.Kind) {
			// Control events: blocking send. Missing these corrupts projection.
			lw.out <- msg
		} else {
			// Data events (stdout/stderr): non-blocking. Tolerate drops.
			select {
			case lw.out <- msg:
			default:
				if lw.droppedEvents != nil {
					lw.droppedEvents.Add(1)
					lw.lastDropAt.Store(time.Now().UnixNano())
				}
			}
		}
	}

	lw.mu.Lock()
	lw.offsets[callID] = off + bytesRead
	if sawCallEnd {
		lw.sealed[callID] = true
	}
	lw.mu.Unlock()

}

// eventsFileIsCompleted reports whether the per-call JSONL file ends with a
// "call-end" event. Used at startup to decide whether to backfill events for
// in-progress calls. Reads only the last ~512 bytes — fast and bounded.
func eventsFileIsCompleted(path string, size int64) bool {
	if size == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		// On error, assume completed so we don't replay broken files.
		return true
	}
	defer f.Close()
	tail := int64(512)
	if size < tail {
		tail = size
	}
	if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
		return true
	}
	buf := make([]byte, tail)
	n, _ := f.Read(buf)
	if n <= 0 {
		return false
	}
	// Check whether call-end appears anywhere in the tail. Straggler data
	// events (stdout/stderr) can land after call-end due to flush ordering,
	// so checking only the last line is insufficient.
	return strings.Contains(string(buf[:n]), `"kind":"call-end"`)
}

func callIDFromPath(path string) string {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(base, ".jsonl")
}

// ─────────────────────────────────────────────────────────────────────────────
// activeCall reducers — apply LiveEvents to evolve state.
// ─────────────────────────────────────────────────────────────────────────────

func (a *activeCall) Apply(ev mcp.LiveEvent) {
	defer a.version.Add(1)

	switch ev.Kind {
	case "call-start":
		a.SessionID = ev.SessionID
		a.Input = ev.Input
		a.Steps = ev.Steps
		a.Pid = ev.Pid
		a.StepStatuses = make([]stepLiveStatus, len(ev.Steps))
		for i, s := range ev.Steps {
			a.StepStatuses[i] = stepLiveStatus{
				Name:   s.Name,
				Action: s.Action,
				Hosts:  s.Hosts,
				Params: s.Params,
			}
		}
		a.CurrentStep = -1
		if !ev.Ts.IsZero() {
			a.StartedAt = ev.Ts
		} else {
			a.StartedAt = time.Now()
		}
		a.wfInitWaterfall(ev)

	case "step-start":
		a.CurrentStep = ev.Step
		if ev.Step >= 0 && ev.Step < len(a.StepStatuses) {
			s := &a.StepStatuses[ev.Step]
			s.Started = true
			if ev.Action != "" {
				s.Action = ev.Action
			}
			if len(ev.Hosts) > 0 {
				s.Hosts = ev.Hosts
			}
			if ev.Name != "" {
				s.Name = ev.Name
			}
		}
		// Reset most-recent-executing on step boundary.
		a.ExecutingCmd = ""
		a.ExecutingHost = ""
		a.wfStepStart(ev)

	case "step-end":
		if ev.Step >= 0 && ev.Step < len(a.StepStatuses) {
			s := &a.StepStatuses[ev.Step]
			s.Ended = true
			s.ExitCode = ev.ExitCode
			s.DurationMs = ev.DurationMs
		}
		a.wfStepEnd(ev)

	case "call-end":
		a.Done = true
		a.DoneStatus = ev.Status

	case "executing":
		a.ExecutingCmd = ev.Cmd
		a.ExecutingHost = ev.Host
		a.ExecutingElapsed = ev.ElapsedSec
		a.appendTail(tailLine{
			Stream: "executing",
			Step:   ev.Step,
			Host:   ev.Host,
			Text:   ev.Cmd,
			Ts:     ev.Ts,
			LineNo: ev.LineNo,
		})
		a.wfExecuting(ev)

	case "stdout":
		a.appendTail(tailLine{
			Stream: "stdout",
			Step:   ev.Step,
			Host:   ev.Host,
			Text:   ev.Line,
			Ts:     ev.Ts,
		})
		a.wfOutput(ev.Step, ev.Host, ev.Ts, "stdout", ev.Line)

	case "stderr":
		a.appendTail(tailLine{
			Stream: "stderr",
			Step:   ev.Step,
			Host:   ev.Host,
			Text:   ev.Line,
			Ts:     ev.Ts,
		})
		a.wfOutput(ev.Step, ev.Host, ev.Ts, "stderr", ev.Line)
	}
}

// ── Incremental waterfall helpers ────────────────────────────────────────

// wfInitWaterfall sets up the incremental waterfall on call-start.
func (a *activeCall) wfInitWaterfall(ev mcp.LiveEvent) {
	a.parsedBodies = parseStepBodies(a.Input)
	n := len(ev.Steps)
	a.waterfall = make([]waterfallStep, n)
	for i, s := range ev.Steps {
		a.waterfall[i] = waterfallStep{
			Name:   s.Name,
			Action: s.Action,
			Hosts:  s.Hosts,
			Params: s.Params,
		}
		if i < len(a.parsedBodies) {
			// Own backing array so incremental mutations don't alias parsedBodies.
			lines := make([]wfSourceLine, len(a.parsedBodies[i]))
			copy(lines, a.parsedBodies[i])
			a.waterfall[i].Lines = lines
		}
	}
	a.wfLineIdx = make([]int, n)
	for i := range a.wfLineIdx {
		a.wfLineIdx[i] = -1
	}
	a.firstAnyTs = make([]time.Time, n)
	a.lastTs = make([]time.Time, n)
}

// wfStepStart marks step started in the waterfall projection.
func (a *activeCall) wfStepStart(ev mcp.LiveEvent) {
	si := ev.Step
	if si < 0 || si >= len(a.waterfall) {
		return
	}
	ws := &a.waterfall[si]
	ws.Started = true
	if ev.Name != "" {
		ws.Name = ev.Name
	}
	if ev.Action != "" {
		ws.Action = ev.Action
	}
	if len(ev.Hosts) > 0 {
		ws.Hosts = ev.Hosts
	}
	// Defensive: finalize previous step's last line if already ended.
	if si > 0 && si-1 < len(a.waterfall) && a.waterfall[si-1].Ended {
		prevIdx := a.wfLineIdx[si-1]
		if prevIdx >= 0 && prevIdx < len(a.waterfall[si-1].Lines) {
			a.waterfall[si-1].Lines[prevIdx].Done = true
		}
	}
}

// wfStepEnd finalizes the step in the waterfall (Done, EndTs on last line).
func (a *activeCall) wfStepEnd(ev mcp.LiveEvent) {
	si := ev.Step
	if si < 0 || si >= len(a.waterfall) {
		return
	}
	ws := &a.waterfall[si]
	ws.Ended = true
	ws.ExitCode = ev.ExitCode
	ws.DurationMs = ev.DurationMs

	idx := a.wfLineIdx[si]
	if idx < 0 || idx >= len(ws.Lines) {
		return
	}
	line := &ws.Lines[idx]
	line.Done = true

	var end time.Time
	switch {
	case !a.firstAnyTs[si].IsZero() && ws.DurationMs > 0:
		end = a.firstAnyTs[si].Add(time.Duration(ws.DurationMs) * time.Millisecond)
	case !a.lastTs[si].IsZero():
		end = a.lastTs[si]
	default:
		end = line.StartTs
	}
	line.EndTs = end
	if len(line.Subs) > 0 && line.Subs[len(line.Subs)-1].EndTs.IsZero() {
		line.Subs[len(line.Subs)-1].EndTs = end
	}
}

// wfExecuting handles an "executing" event for the incremental waterfall.
func (a *activeCall) wfExecuting(ev mcp.LiveEvent) {
	si := ev.Step
	if si < 0 || si >= len(a.waterfall) {
		if si >= 0 {
			log.Printf("waterfall: executing event step %d out of bounds (have %d steps)", si, len(a.waterfall))
		}
		return
	}
	a.wfTrackTs(si, ev.Ts)

	step := &a.waterfall[si]
	lineIdx := findLineByLineNo(step.Lines, ev.LineNo)
	if lineIdx < 0 {
		lineIdx = a.wfLineIdx[si]
		if lineIdx < 0 {
			lineIdx = firstNonSkipped(step.Lines)
		}
		if lineIdx < 0 {
			return
		}
	}

	// Transition: finalize previous line if we moved.
	prev := a.wfLineIdx[si]
	if prev >= 0 && prev != lineIdx {
		step.Lines[prev].Done = true
		step.Lines[prev].EndTs = ev.Ts
		if len(step.Lines[prev].Subs) > 0 {
			step.Lines[prev].Subs[len(step.Lines[prev].Subs)-1].EndTs = ev.Ts
		}
	}

	line := &step.Lines[lineIdx]
	if !line.Started {
		line.Started = true
		line.StartTs = ev.Ts
	}
	if len(line.Subs) > 0 {
		line.Subs[len(line.Subs)-1].EndTs = ev.Ts
	}
	subOpIdx := matchSubToOperand(line.Operands, ev.Cmd)
	line.Subs = append(line.Subs, wfSub{
		Cmd:        ev.Cmd,
		StartTs:    ev.Ts,
		Host:       ev.Host,
		OperandIdx: subOpIdx,
	})
	a.wfLineIdx[si] = lineIdx
}

// wfOutput handles stdout/stderr for the incremental waterfall.
func (a *activeCall) wfOutput(si int, host string, ts time.Time, stream, text string) {
	if si < 0 || si >= len(a.waterfall) {
		return
	}
	a.wfTrackTs(si, ts)
	lineIdx := a.wfLineIdx[si]
	if lineIdx < 0 {
		lineIdx = firstNonSkipped(a.waterfall[si].Lines)
	}
	if lineIdx < 0 {
		return
	}
	line := &a.waterfall[si].Lines[lineIdx]
	// Non-traced entrypoints (sh, python, etc.) emit no "executing"
	// events, so lines never get Started. Mark on first output so
	// the renderer shows content instead of empty ○.
	if !line.Started {
		line.Started = true
		if line.StartTs.IsZero() {
			line.StartTs = ts
		}
	}
	if len(line.Output) < maxOutputPerLine {
		line.Output = append(line.Output, wfOut{Stream: stream, Text: text, Host: host, Ts: ts})
	}
}

// wfTrackTs updates firstAnyTs and lastTs for a step.
func (a *activeCall) wfTrackTs(si int, ts time.Time) {
	if ts.IsZero() || si < 0 || si >= len(a.firstAnyTs) {
		return
	}
	if a.firstAnyTs[si].IsZero() {
		a.firstAnyTs[si] = ts
	}
	if ts.After(a.lastTs[si]) {
		a.lastTs[si] = ts
	}
}

func (a *activeCall) appendTail(t tailLine) {
	a.Tail = append(a.Tail, t)
	if !a.unlimitedTail && len(a.Tail) > maxTail {
		a.Tail = a.Tail[len(a.Tail)-maxTail:]
	}
}

// asCallEntry projects an activeCall onto a CallEntry shape so the list view
// can render it through the same renderer. Results stay empty for in-flight
// calls; completed calls get Results populated during transition.
func (a *activeCall) asCallEntry() mcp.CallEntry {
	return mcp.CallEntry{
		ID:         a.ID,
		Timestamp:  a.StartedAt,
		SessionID:  a.SessionID,
		Input:      a.Input,
		Steps:      a.Steps,
		DurationMs: time.Since(a.StartedAt).Milliseconds(),
	}
}
