package dashboard

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

func TestGoldenUnifiedView(t *testing.T) {
	stateDir := withTempStateDir(t)
	evDir := filepath.Join(stateDir, "events")
	if err := os.MkdirAll(evDir, 0755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}

	// testdata/golden lives at the repo root (shared fixtures), two levels up
	// from this package directory.
	goldenDir := filepath.Join("..", "..", "testdata", "golden")
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}

	for _, de := range entries {
		if !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(de.Name(), ".jsonl")
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(goldenDir, de.Name())
			dst := filepath.Join(evDir, name+".jsonl")
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if err := os.WriteFile(dst, data, 0644); err != nil {
				t.Fatalf("write fixture to state dir: %v", err)
			}

			got := renderGoldenFixture(t, src, name)

			expectedPath := filepath.Join(goldenDir, name+".expected")
			if os.Getenv("GOLDEN_UPDATE") == "1" {
				if err := os.WriteFile(expectedPath, []byte(got), 0644); err != nil {
					t.Fatalf("write expected: %v", err)
				}
				t.Logf("updated %s", expectedPath)
				return
			}

			want, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("read expected (run with GOLDEN_UPDATE=1 to generate): %v", err)
			}
			if got != string(want) {
				diff := lineDiff(string(want), got)
				t.Errorf("golden mismatch for %s:\n%s", name, diff)
			}
		})
	}
}

// renderGoldenFixture replays a JSONL event file through the unified view model
// at a fixed 160x40 terminal size, evicts the completed call from active state,
// and returns the stripped-ANSI View() output.
func renderGoldenFixture(t *testing.T, fixturePath, callID string) string {
	t.Helper()

	events, err := readEventsFile(fixturePath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	m := ldInitialModel()
	m.width = 160
	m.height = 40
	m.view = ldViewUnified

	mi, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = mi.(ldModel)

	// Feed all events.
	var callStart mcp.LiveEvent
	var callEnd mcp.LiveEvent
	for _, ev := range events {
		if ev.Kind == "call-start" {
			callStart = ev
		}
		if ev.Kind == "call-end" {
			callEnd = ev
		}
		mi, _ = m.Update(ldLiveEventMsg{CallID: callID, Event: ev})
		m = mi.(ldModel)
	}

	// call-end auto-transitions the call from active to entries, producing
	// deterministic output (no LIVE badge, no time.Now()-based elapsed).
	_ = callStart
	_ = callEnd

	return ui.StripANSI(m.View())
}

// lineDiff produces a simple line-by-line diff showing first divergence.
func lineDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	var b bytes.Buffer
	maxLines := len(wantLines)
	if len(gotLines) > maxLines {
		maxLines = len(gotLines)
	}
	shown := 0
	for i := 0; i < maxLines && shown < 10; i++ {
		w := ""
		g := ""
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			fmt.Fprintf(&b, "  line %d:\n", i+1)
			b.WriteString("    want: " + w + "\n")
			b.WriteString("     got: " + g + "\n")
			shown++
		}
	}
	if shown == 0 {
		b.WriteString("  (no differences found in line-by-line comparison)\n")
	}
	return b.String()
}
