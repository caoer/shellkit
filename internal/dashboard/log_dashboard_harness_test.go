package dashboard

import (
	"fmt"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	tea "github.com/charmbracelet/bubbletea"
)

// ── Headless interaction harness ──────────────────────────────────────
//
// Drives ldModel directly — no tmux, no PTY. Synthesises tea.KeyMsg and
// ldLiveEventMsg via m.Update(), measures Update+View latency, and
// captures string snapshots of View() output.

type harness struct {
	t     *testing.T
	m     ldModel
	lastU time.Duration // last Update() wall time
	lastV time.Duration // last View() wall time
}

// newHarness creates a harness with the given terminal dimensions and an
// ldModel ready to receive events. The model is sized via WindowSizeMsg
// so viewport dimensions match.
func newHarness(t *testing.T, w, h int) *harness {
	t.Helper()
	m := ldInitialModel()
	m.width = w
	m.height = h
	mi, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mi.(ldModel)
	return &harness{t: t, m: m}
}

// withTempStateDir redirects mcp.StateDir() to a temp dir for the duration of
// the test. StateDir() reads HOME, so we override HOME and return the resulting
// state dir.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return mcp.StateDir()
}

// feedEvent applies a live event and returns Update wall time.
func (h *harness) feedEvent(callID string, ev mcp.LiveEvent) time.Duration {
	msg := ldLiveEventMsg{CallID: callID, Event: ev}
	start := time.Now()
	mi, _ := h.m.Update(msg)
	h.lastU = time.Since(start)
	h.m = mi.(ldModel)
	return h.lastU
}

// feedKey synthesises a tea.KeyMsg and applies it. Returns Update wall time.
func (h *harness) feedKey(key string) time.Duration {
	msg := keyMsg(key)
	start := time.Now()
	mi, _ := h.m.Update(msg)
	h.lastU = time.Since(start)
	h.m = mi.(ldModel)
	return h.lastU
}

// snapshot returns View() output and records view timing.
func (h *harness) snapshot() string {
	start := time.Now()
	s := h.m.View()
	h.lastV = time.Since(start)
	return s
}

// viewTime returns the most recent View() duration.
func (h *harness) viewTime() time.Duration { return h.lastV }

// ── Synthetic event generators ────────────────────────────────────────

// synthCallStart creates a call-start LiveEvent.
func synthCallStart(sessionID, input string, steps []mcp.StepBrief) mcp.LiveEvent {
	return mcp.LiveEvent{
		Kind:      "call-start",
		Ts:        time.Now(),
		SessionID: sessionID,
		Input:     input,
		Steps:     steps,
	}
}

// synthStepStart creates a step-start event.
func synthStepStart(step int, name string) mcp.LiveEvent {
	return mcp.LiveEvent{
		Kind: "step-start",
		Ts:   time.Now(),
		Step: step,
		Name: name,
	}
}

// synthStepEnd creates a step-end event.
func synthStepEnd(step int, exitCode int, durMs int64) mcp.LiveEvent {
	return mcp.LiveEvent{
		Kind:       "step-end",
		Ts:         time.Now(),
		Step:       step,
		ExitCode:   exitCode,
		DurationMs: durMs,
	}
}

// synthStdout creates a stdout event.
func synthStdout(step int, line string) mcp.LiveEvent {
	return mcp.LiveEvent{
		Kind: "stdout",
		Ts:   time.Now(),
		Step: step,
		Line: line,
	}
}

// synthCallEnd creates a call-end event.
func synthCallEnd(status string) mcp.LiveEvent {
	return mcp.LiveEvent{
		Kind:   "call-end",
		Ts:     time.Now(),
		Status: status,
	}
}

// keyMsg converts a key name string to a tea.KeyMsg. Handles common keys.
func keyMsg(key string) tea.KeyMsg {
	switch key {
	case "j":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}
	case "k":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "/":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "q":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	case "d":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}
	case "r":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}
	case "g":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}
	case "G":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	default:
		// Single rune fallback.
		if len(key) == 1 {
			return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
}

// seedCalls populates the harness with n completed calls via live events.
// Each call has 1 step with a few stdout lines. Returns the call IDs.
func seedCalls(h *harness, n int) []string {
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call-%04d", i)
		ids[i] = id
		h.feedEvent(id, synthCallStart("sess", "### build\necho hello", []mcp.StepBrief{
			{Name: "build", Action: "local"},
		}))
		h.feedEvent(id, synthStepStart(0, "build"))
		for j := 0; j < 3; j++ {
			h.feedEvent(id, synthStdout(0, fmt.Sprintf("output-%d-%d", i, j)))
		}
		h.feedEvent(id, synthStepEnd(0, 0, 100))
		h.feedEvent(id, synthCallEnd("ok"))
	}
	return ids
}
