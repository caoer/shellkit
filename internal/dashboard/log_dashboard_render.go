package dashboard

// Headless renderer for the log dashboard.
//
// Renders ldModel to stdout without a TTY, by replaying an event stream and
// invoking the same Update/View functions bubbletea would. Used for development
// and debugging — `shellkit mcp render-dashboard <call-id>` prints what the live
// dashboard would show for that call.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

// renderOpts configures the headless render.
type renderOpts struct {
	callIDs   []string // specific call IDs; empty + all=true means every call
	all       bool     // render every call in events/
	limit     int      // cap on call IDs when --all (default 25; 0 = no cap)
	view      string   // "list" | "detail" | "unified" — empty defaults to list
	width     int
	height    int
	frame     int  // -1 = final state; >=0 = state after applying frame events
	frames    bool // emit one snapshot per event applied
	stripAnsi bool
	cursor    int  // simulate cursor position (0-indexed)
	zoom      bool // simulate z keypress (detail zoom)
	out       io.Writer
}

// RenderDashboard runs a headless render per opts.
func RenderDashboard(opts renderOpts) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.width == 0 {
		opts.width = 160
	}
	if opts.height == 0 {
		opts.height = 50
	}

	ids := opts.callIDs
	if opts.all {
		discovered, err := discoverCallIDs()
		if err != nil {
			return err
		}
		limit := opts.limit
		if limit == 0 {
			limit = 25
		}
		if len(discovered) > limit {
			discovered = discovered[:limit]
		}
		ids = discovered
	}
	if len(ids) == 0 {
		return fmt.Errorf("no call IDs specified (use --all or pass call IDs as args)")
	}

	// --all: feed every call's events into one model so the left column
	// shows multiple cells. Otherwise, render each call in its own model.
	if opts.all {
		fmt.Fprintf(opts.out, "▎ all calls (%d)   view: %s   %dx%d\n",
			len(ids), viewLabel(opts.view), opts.width, opts.height)
		fmt.Fprintln(opts.out, "──────────────────────────────────────────────────────────")
		return renderMerged(ids, opts)
	}

	for i, id := range ids {
		if i > 0 {
			fmt.Fprintln(opts.out, "\n══════════════════════════════════════════════════════════")
		}
		fmt.Fprintf(opts.out, "▎ call: %s   view: %s   %dx%d\n",
			id, viewLabel(opts.view), opts.width, opts.height)
		fmt.Fprintln(opts.out, "──────────────────────────────────────────────────────────")
		if err := renderOne(id, opts); err != nil {
			fmt.Fprintf(opts.out, "ERROR: %v\n", err)
		}
	}
	return nil
}

// renderMerged feeds events from all listed calls into a single ldModel and
// renders once. Useful for previewing the list/unified view with multiple
// entries.
func renderMerged(ids []string, opts renderOpts) error {
	m := ldInitialModel()
	m.width = opts.width
	m.height = opts.height
	switch opts.view {
	case "detail":
		m.view = ldViewDetail
	case "unified":
		m.view = ldViewUnified
	default:
		m.view = ldViewList
	}
	mi, _ := m.Update(tea.WindowSizeMsg{Width: opts.width, Height: opts.height})
	m = mi.(ldModel)

	for _, id := range ids {
		events, err := readEventsFile(mcp.CallEventsPath(id))
		if err != nil {
			fmt.Fprintf(opts.out, "skip %s: %v\n", id, err)
			continue
		}
		for _, ev := range events {
			mi, _ := m.Update(ldLiveEventMsg{CallID: id, Event: ev})
			m = mi.(ldModel)
		}
	}
	// Simulate cursor presses to navigate to a specific entry.
	for k := 0; k < opts.cursor; k++ {
		mi, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mi.(ldModel)
	}
	if opts.zoom {
		mi, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
		m = mi.(ldModel)
	}
	emit(opts, m.View())
	return nil
}

func renderOne(callID string, opts renderOpts) error {
	events, err := readEventsFile(mcp.CallEventsPath(callID))
	if err != nil {
		return err
	}

	m := ldInitialModel()
	m.width = opts.width
	m.height = opts.height

	// Set view mode
	switch opts.view {
	case "detail":
		m.view = ldViewDetail
	case "unified":
		m.view = ldViewUnified
	default:
		m.view = ldViewList
	}

	// Initial size + entries (synth calls.jsonl entry for completed calls)
	mi, _ := m.Update(tea.WindowSizeMsg{Width: opts.width, Height: opts.height})
	m = mi.(ldModel)

	// Decide which frames to emit
	last := len(events) - 1
	if opts.frame >= 0 && opts.frame <= last {
		last = opts.frame
	}

	for i, ev := range events {
		mi, _ := m.Update(ldLiveEventMsg{CallID: callID, Event: ev})
		m = mi.(ldModel)

		if i > last {
			break
		}
		if opts.frames {
			fmt.Fprintf(opts.out, "\n--- frame %d/%d  kind=%s  step=%d ---\n",
				i+1, len(events), ev.Kind, ev.Step)
			emit(opts, m.View())
		}
		if i == last && !opts.frames {
			if opts.zoom {
				mi, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
				m = mi.(ldModel)
			}
			emit(opts, m.View())
		}
	}

	return nil
}

func emit(opts renderOpts, s string) {
	if opts.stripAnsi {
		s = ui.StripANSI(s)
	}
	fmt.Fprintln(opts.out, s)
}

// readEventsFile reads a per-call JSONL events file into LiveEvent slice.
func readEventsFile(path string) ([]mcp.LiveEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open events: %w", err)
	}
	defer f.Close()

	var out []mcp.LiveEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		ev, err := mcp.ParseLiveEvent(sc.Bytes())
		if err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// discoverCallIDs lists every call ID with a JSONL event file, sorted by mtime
// newest-first so --all shows recent calls first.
func discoverCallIDs() ([]string, error) {
	dir := mcp.EventsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type item struct {
		id    string
		mtime int64
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{
			id:    strings.TrimSuffix(e.Name(), ".jsonl"),
			mtime: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime > items[j].mtime })
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out, nil
}

func viewLabel(v string) string {
	if v == "" {
		return "list"
	}
	return v
}

// ── CLI plumbing ──────────────────────────────────────────────────────

// RunRenderDashboard parses argv tail and dispatches RenderDashboard.
// Called from the mcp render-dashboard subcommand.
func RunRenderDashboard(args []string) error {
	opts := renderOpts{
		frame: -1,
	}

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--all":
			opts.all = true
		case a == "--view":
			if i+1 >= len(args) {
				return fmt.Errorf("--view requires a value")
			}
			opts.view = args[i+1]
			i++
		case strings.HasPrefix(a, "--view="):
			opts.view = strings.TrimPrefix(a, "--view=")
		case a == "--width":
			if i+1 >= len(args) {
				return fmt.Errorf("--width requires a value")
			}
			fmt.Sscanf(args[i+1], "%d", &opts.width)
			i++
		case strings.HasPrefix(a, "--width="):
			fmt.Sscanf(strings.TrimPrefix(a, "--width="), "%d", &opts.width)
		case a == "--height":
			if i+1 >= len(args) {
				return fmt.Errorf("--height requires a value")
			}
			fmt.Sscanf(args[i+1], "%d", &opts.height)
			i++
		case strings.HasPrefix(a, "--height="):
			fmt.Sscanf(strings.TrimPrefix(a, "--height="), "%d", &opts.height)
		case a == "--frame":
			if i+1 >= len(args) {
				return fmt.Errorf("--frame requires a value")
			}
			fmt.Sscanf(args[i+1], "%d", &opts.frame)
			i++
		case strings.HasPrefix(a, "--frame="):
			fmt.Sscanf(strings.TrimPrefix(a, "--frame="), "%d", &opts.frame)
		case a == "--frames":
			opts.frames = true
		case a == "--no-ansi":
			opts.stripAnsi = true
		case a == "--limit":
			if i+1 >= len(args) {
				return fmt.Errorf("--limit requires a value")
			}
			fmt.Sscanf(args[i+1], "%d", &opts.limit)
			i++
		case strings.HasPrefix(a, "--limit="):
			fmt.Sscanf(strings.TrimPrefix(a, "--limit="), "%d", &opts.limit)
		case a == "--cursor":
			if i+1 >= len(args) {
				return fmt.Errorf("--cursor requires a value")
			}
			fmt.Sscanf(args[i+1], "%d", &opts.cursor)
			i++
		case strings.HasPrefix(a, "--cursor="):
			fmt.Sscanf(strings.TrimPrefix(a, "--cursor="), "%d", &opts.cursor)
		case a == "--zoom":
			opts.zoom = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag: %s", a)
		default:
			// Treat as call ID. Allow path → strip dir + .jsonl.
			id := a
			if strings.HasSuffix(id, ".jsonl") {
				id = strings.TrimSuffix(filepath.Base(id), ".jsonl")
			}
			opts.callIDs = append(opts.callIDs, id)
		}
		i++
	}

	if !opts.all && len(opts.callIDs) == 0 {
		return fmt.Errorf("usage: shellkit mcp render-dashboard [--all|<call-id>...] [--view=list|detail|unified] [--width=N] [--height=N] [--frame=N|--frames] [--no-ansi]")
	}

	return RenderDashboard(opts)
}
