package dashboard

import (
	"fmt"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	tea "github.com/charmbracelet/bubbletea"
)

// TestUnifiedStickyBottom verifies the right viewport's sticky-bottom behavior:
//   - Active call with overflow content → viewport at bottom
//   - New output appended → remains at bottom (sticky)
//   - User scrolls up → new output does NOT snap back to bottom
func TestUnifiedStickyBottom(t *testing.T) {
	m := ldInitialModel()
	m.width = 120
	m.height = 15 // small height so waterfall easily overflows
	m.view = ldViewUnified

	// Apply window size so viewports get correct dimensions.
	mi, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 15})
	m = mi.(ldModel)

	callID := "sticky-test"
	a := &activeCall{ID: callID, CurrentStep: -1}
	a.Apply(mcp.LiveEvent{
		Kind:      "call-start",
		Ts:        time.Now(),
		SessionID: "sess",
		Input:     "### deploy\necho deploying",
		Steps:     []mcp.StepBrief{{Name: "deploy", Action: "remote", Hosts: []string{"web1"}}},
	})
	a.Apply(mcp.LiveEvent{Kind: "step-start", Step: 0, Name: "deploy"})

	// Generate enough waterfall content to overflow viewport.
	// Each "executing" event creates a source line in the waterfall.
	for i := 0; i < 40; i++ {
		a.Apply(mcp.LiveEvent{
			Kind:   "executing",
			Step:   0,
			LineNo: i + 1,
			Line:   fmt.Sprintf("command_%d --flag=%d", i, i),
			Host:   "web1",
		})
		a.Apply(mcp.LiveEvent{
			Kind: "stdout",
			Step: 0,
			Line: fmt.Sprintf("output line %d", i),
			Host: "web1",
		})
	}

	m.active[callID] = a
	m.activeIDs = []string{callID}
	m.rebuildFiltered()
	m.refreshUnifiedView()

	// 1. Active call should be at bottom (sticky).
	if !m.unifiedRightVP.AtBottom() {
		t.Errorf("expected right viewport at bottom for active call, YOffset=%d total=%d height=%d",
			m.unifiedRightVP.YOffset, m.unifiedRightVP.TotalLineCount(), m.unifiedRightVP.Height)
	}

	// 2. Append more output → should remain at bottom.
	for i := 0; i < 5; i++ {
		a.Apply(mcp.LiveEvent{Kind: "stdout", Step: 0, Line: fmt.Sprintf("appended %d", i), Host: "web1"})
	}
	m.refreshUnifiedView()
	if !m.unifiedRightVP.AtBottom() {
		t.Error("should remain at bottom after appending output (sticky)")
	}

	// 3. User scrolls up to middle → should NOT snap back.
	total := m.unifiedRightVP.TotalLineCount()
	vh := m.unifiedRightVP.Height
	t.Logf("right VP: total=%d height=%d YOffset=%d", total, vh, m.unifiedRightVP.YOffset)
	if total <= vh {
		t.Fatalf("precondition: need content (%d lines) > viewport height (%d) to test scroll", total, vh)
	}
	m.unifiedRightVP.ScrollUp(10)
	if m.unifiedRightVP.AtBottom() {
		t.Fatalf("precondition: should not be at bottom after ScrollUp(10), YOffset=%d maxY=%d",
			m.unifiedRightVP.YOffset, total-vh)
	}

	a.Apply(mcp.LiveEvent{Kind: "stdout", Step: 0, Line: "line after scroll", Host: "web1"})
	m.refreshUnifiedView()

	if m.unifiedRightVP.AtBottom() {
		t.Error("should NOT snap to bottom after user scrolled away")
	}
}
