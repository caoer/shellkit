package dashboard

import (
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	tea "github.com/charmbracelet/bubbletea"
)

// TestListViewportScrollPreservation verifies that the viewport's YOffset is
// preserved when new entries arrive (content refreshes) while the user has
// scrolled to a mid-list position. When the viewport is at the bottom, new
// content should auto-scroll to keep the bottom visible.
func TestListViewportScrollPreservation(t *testing.T) {
	m := ldInitialModel()
	m.width = 120
	m.height = 40
	m.view = ldViewList

	// Apply window size so viewport dimensions are correct.
	mi, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mi.(ldModel)

	// Create enough entries to overflow the viewport.
	now := time.Now()
	var entries []mcp.CallEntry
	for i := 0; i < 30; i++ {
		entries = append(entries, mcp.CallEntry{
			ID:         idForIndex(i),
			Timestamp:  now.Add(-time.Duration(i) * time.Minute),
			SessionID:  "test-session",
			Input:      "### step\n{}\necho hello",
			DurationMs: 100,
			Steps:      []mcp.StepBrief{{Name: "step1", Action: "exec", Hosts: []string{"host1"}}},
			Results:    []mcp.ResultBrief{{Name: "step1", Host: "host1", ExitCode: 0, Stdout: "hello\n"}},
		})
	}

	m.entries = entries
	m.rebuildFiltered()
	m.refreshListView()

	if m.listVP.TotalLineCount() <= m.listVP.Height {
		t.Fatalf("expected content taller than viewport; got %d lines, viewport height %d",
			m.listVP.TotalLineCount(), m.listVP.Height)
	}

	// Scroll to roughly halfway.
	midOffset := m.listVP.TotalLineCount() / 2
	m.listVP.SetYOffset(midOffset)
	savedOffset := m.listVP.YOffset

	if savedOffset == 0 {
		t.Fatal("expected non-zero YOffset after scrolling to middle")
	}

	// Simulate new entries arriving (content refresh) — same entries, which
	// triggers a refreshListView via the ldEntriesLoaded handler.
	m.entries = entries
	m.rebuildFiltered()
	m.refreshListView()

	// YOffset should be preserved (not at bottom, so it restores oldY).
	tolerance := 3 // SetYOffset clamps to maxYOffset which may shift slightly
	diff := m.listVP.YOffset - savedOffset
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Errorf("YOffset not preserved: had %d, now %d (tolerance %d)",
			savedOffset, m.listVP.YOffset, tolerance)
	}

	// Now test auto-follow when at bottom.
	m.listVP.GotoBottom()
	if !m.listVP.AtBottom() {
		t.Fatal("expected viewport at bottom after GotoBottom")
	}

	// Add more entries — should auto-scroll to new bottom.
	moreEntries := make([]mcp.CallEntry, len(entries)+5)
	copy(moreEntries, entries)
	for i := len(entries); i < len(moreEntries); i++ {
		moreEntries[i] = mcp.CallEntry{
			ID:         idForIndex(i),
			Timestamp:  now.Add(-time.Duration(i) * time.Minute),
			SessionID:  "test-session",
			Input:      "### extra\n{}\necho more",
			DurationMs: 50,
			Steps:      []mcp.StepBrief{{Name: "extra", Action: "exec", Hosts: []string{"host1"}}},
			Results:    []mcp.ResultBrief{{Name: "extra", Host: "host1", ExitCode: 0}},
		}
	}

	m.entries = moreEntries
	m.rebuildFiltered()
	m.refreshListView()

	if !m.listVP.AtBottom() {
		t.Errorf("expected viewport to follow bottom after new entries; YOffset=%d, maxY would be %d",
			m.listVP.YOffset, m.listVP.TotalLineCount()-m.listVP.Height)
	}
}

// idForIndex produces a deterministic call ID for test entries.
func idForIndex(i int) string {
	return "test-call-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
}
