package dashboard

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/ui"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ──────────────────────────────────────────────────────────
// Tokyo-Night-inspired palette, adaptive to the terminal background.
// Every color is an AdaptiveColor{Light, Dark} pair: the Dark side keeps the
// original palette tuned for dark terminals; the Light side swaps in darker
// counterparts readable on white. lipgloss detects the background once at
// first render (non-tty output defaults to the dark palette).
// Color reference (ANSI 256), dark / light:
//   text   primary 252/235  dim 244/243  faint 240/248
//   accent blue   111/26    cyan 117/31  purple 141/55
//   status ok 114/28  warn 215/130  fail 203/160
//   tags   step 213/162 (pink)  host 215/130 (orange)
//   bg     statusbar 237/254  selection 24/153 (blue, visible but not harsh)

// ac builds an adaptive color: light-terminal value first, dark second.
func ac(light, dark string) lipgloss.AdaptiveColor {
	return lipgloss.AdaptiveColor{Light: light, Dark: dark}
}

var (
	ldTitle  = lipgloss.NewStyle().Bold(true).Foreground(ac("26", "111"))
	ldDim    = lipgloss.NewStyle().Foreground(ac("243", "244"))
	ldFaint  = lipgloss.NewStyle().Foreground(ac("248", "240"))
	ldOk     = lipgloss.NewStyle().Foreground(ac("28", "114")).Bold(true)
	ldFail   = lipgloss.NewStyle().Foreground(ac("160", "203")).Bold(true)
	ldWarn   = lipgloss.NewStyle().Foreground(ac("130", "215"))
	ldAccent = lipgloss.NewStyle().Foreground(ac("31", "117"))

	ldBar     = lipgloss.NewStyle().Background(ac("254", "237")).Foreground(ac("235", "252"))
	ldSection = lipgloss.NewStyle().Bold(true).Foreground(ac("55", "141"))
	ldBorder  = lipgloss.NewStyle().Foreground(ac("250", "238"))
	ldColSep  = lipgloss.NewStyle().Foreground(ac("250", "238"))

	ldStepName = lipgloss.NewStyle().Bold(true).Foreground(ac("162", "213"))
	ldHostTag  = lipgloss.NewStyle().Foreground(ac("130", "215"))
	ldErrTag   = lipgloss.NewStyle().Bold(true).Foreground(ac("160", "203"))
	ldStdout   = lipgloss.NewStyle().Foreground(ac("238", "250")) // body text vs terminal bg
	ldArrow    = lipgloss.NewStyle().Foreground(ac("55", "141"))

	// DSL syntax: ### header (pink bold), {json} (cyan), body (off-white)
	ldDSLHead = lipgloss.NewStyle().Bold(true).Foreground(ac("162", "213"))
	ldDSLConf = lipgloss.NewStyle().Foreground(ac("31", "117"))
	ldDSLBody = lipgloss.NewStyle().Foreground(ac("235", "252"))

	// selection: bright cyan gutter
	ldSelGutter = lipgloss.NewStyle().Foreground(ac("31", "117")).Bold(true)

	// live indicators
	ldLive       = lipgloss.NewStyle().Bold(true).Background(ac("160", "203")).Foreground(lipgloss.Color("231")) // white-on-red both ways
	ldLiveDot    = lipgloss.NewStyle().Bold(true).Foreground(ac("160", "203"))
	ldLiveExec   = lipgloss.NewStyle().Bold(true).Foreground(ac("130", "215"))
	ldLiveStdout = lipgloss.NewStyle().Foreground(ac("238", "250"))
	ldLiveStderr = lipgloss.NewStyle().Foreground(ac("130", "215"))
	ldLiveStep   = lipgloss.NewStyle().Bold(true).Foreground(ac("31", "117"))
)

// ── Layout modes ────────────────────────────────────────────────────

type ldLayout int

const (
	ldLayoutFatRight ldLayout = iota // 30/70 — default: compact cells, wide waterfall
	ldLayoutBalanced                 // 50/50
	ldLayoutFatLeft                  // 65/35
	ldLayoutCount                    // sentinel for cycling
)

func (l ldLayout) String() string {
	switch l {
	case ldLayoutBalanced:
		return "balanced"
	case ldLayoutFatLeft:
		return "fat-left"
	case ldLayoutFatRight:
		return "fat-right"
	}
	return "?"
}

func (l ldLayout) ratio() (int, int) {
	switch l {
	case ldLayoutFatLeft:
		return 65, 35
	case ldLayoutFatRight:
		return 30, 70
	default:
		return 50, 50
	}
}

// ── View mode ───────────────────────────────────────────────────────

type ldViewMode int

// ldViewUnified is the zero value: `shellkit logs` opens straight into it.
const (
	ldViewUnified ldViewMode = iota
	ldViewDetail
)

// ── Render cache ────────────────────────────────────────────────────
//
// renderedCell caches pre-styled string slices for one call-log entry
// across both view modes. Version-gated: the cell is valid as long as
// the cached version >= the activeCall's current version.

type renderedCell struct {
	version  uint64
	left     []string // pre-styled left-column lines (unified view)
	right    []string // pre-styled right-column lines (unified waterfall)
	hasRight bool     // true once right has been computed (nil right = no steps)
}

const cellCacheCap = 200 // max cached cells before LRU eviction

// ── Model ───────────────────────────────────────────────────────────

type ldModel struct {
	// entries holds completed calls (newest first), populated by startup scan
	// and auto-transition when call-end arrives from live watcher.
	entries []mcp.CallEntry
	// merged is entries + active-call projections used for rendering. The
	// filtered slice holds indices into this.
	merged   []mcp.CallEntry
	filtered []int
	cursor   int
	height   int
	width    int

	// view
	view   ldViewMode
	layout ldLayout

	// detail view — viewport replaces detailLines/detailScroll
	detailVP viewport.Model
	zoomed   bool // z toggle: hide left column, full-width right column

	// search
	filter    textinput.Model
	filtering bool

	lastEntryCount int

	// Live state — populated by the fsnotify watcher goroutine.
	// active maps call-id → in-progress state. On call-end, we auto-transition
	// to entries so the renderer takes the canonical static entry.
	active    map[string]*activeCall
	activeIDs []string // insertion-ordered for stable list ordering

	// Unified view — viewports replace flat scroll buffers
	unifiedLeftVP     viewport.Model
	unifiedRightVP    viewport.Model
	unifiedCellStarts []int // line offset per cell for cursor-following

	// Channel backpressure — per-program channel + drop counters shared
	// with the watcher goroutine for the lag indicator.
	liveEventCh   chan ldLiveEventMsg
	droppedEvents *atomic.Uint64
	lastDropAt    *atomic.Int64 // unix nano

	// Per-call render cache — version-gated by activeCall.version.
	cellCache     map[string]*renderedCell
	cacheLRU      []string // FIFO order for eviction at cellCacheCap
	cacheLRUDirty bool     // marks needing end-of-frame eviction pass

	// Unified left column separator cache.
	unifiedSepLine  string
	unifiedSepWidth int
}

type ldTickMsg struct{}

func ldInitialModel() ldModel {
	fi := textinput.New()
	fi.Prompt = "/"
	fi.CharLimit = 128

	disabledKM := viewport.KeyMap{} // shellkit owns all navigation keys

	uLeftVP := viewport.New(40, 37)
	uLeftVP.KeyMap = disabledKM

	uRightVP := viewport.New(80, 37)
	uRightVP.KeyMap = disabledKM

	detVP := viewport.New(120, 36) // height-4 for detail chrome
	detVP.KeyMap = disabledKM

	return ldModel{
		height:         40,
		width:          120,
		unifiedLeftVP:  uLeftVP,
		unifiedRightVP: uRightVP,
		detailVP:       detVP,
		filter:         fi,
		layout:         ldLayoutFatRight, // the unified cell column is intrinsically compact
		active:         make(map[string]*activeCall),
		activeIDs:      nil,
		liveEventCh:    make(chan ldLiveEventMsg, 1024),
		droppedEvents:  &atomic.Uint64{},
		lastDropAt:     &atomic.Int64{},
		cellCache:      make(map[string]*renderedCell),
	}
}

func (m ldModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		tickEvery(2*time.Second),
		waitLiveEvent(m.liveEventCh),
	)
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return ldTickMsg{} })
}

// waitLiveEvent returns a tea.Cmd that blocks on ch and returns the next live
// event as a tea.Msg. The Update handler re-issues this cmd after handling
// each event so the pump runs continuously.
func waitLiveEvent(ch chan ldLiveEventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return ldLiveDoneMsg{}
		}
		return msg
	}
}

// ── Update ──────────────────────────────────────────────────────────

func (m ldModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.cacheInvalidateAll() // cells are width-dependent
		m.buildDetail()
		m.refreshUnifiedView()
		return m, nil
	case ldTickMsg:
		m.buildDetail()
		m.refreshUnifiedView()
		tick := 2 * time.Second
		if m.view == ldViewUnified && len(m.active) > 0 {
			tick = 1 * time.Second
		}
		return m, tickEvery(tick)
	case ldLiveEventMsg:
		m.applyLiveEvent(msg)
		// Batch-drain: consume all buffered events before rebuilding views.
		// The channel is FIFO so event ordering is preserved. This collapses
		// N per-event rebuilds into 1 — critical when poll pushes a burst.
		for {
			select {
			case more := <-m.liveEventCh:
				m.applyLiveEvent(more)
			default:
				goto drained
			}
		}
	drained:
		m.rebuildFiltered()
		m.buildDetail()
		m.refreshUnifiedView()
		return m, waitLiveEvent(m.liveEventCh)
	case ldLiveDoneMsg:
		// Watcher exited — stop pumping but stay alive on tick refresh.
		return m, nil
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		switch m.view {
		case ldViewDetail:
			return m.handleDetailKey(msg)
		case ldViewUnified:
			return m.handleUnifiedKey(msg)
		}
	}
	return m, nil
}

// handleMouse dispatches scroll-wheel events to the appropriate viewport.
func (m ldModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonWheelUp && msg.Button != tea.MouseButtonWheelDown {
		return m, nil
	}
	const scrollLines = 3
	switch m.view {
	case ldViewDetail:
		if msg.Button == tea.MouseButtonWheelUp {
			m.detailVP.ScrollUp(scrollLines)
		} else {
			m.detailVP.ScrollDown(scrollLines)
		}
	case ldViewUnified:
		// In non-zoomed mode, scroll left column when mouse is over it.
		leftW, _ := m.colWidths()
		if !m.zoomed && msg.X <= leftW {
			if msg.Button == tea.MouseButtonWheelUp {
				m.unifiedLeftVP.ScrollUp(scrollLines)
			} else {
				m.unifiedLeftVP.ScrollDown(scrollLines)
			}
		} else {
			if msg.Button == tea.MouseButtonWheelUp {
				m.unifiedRightVP.ScrollUp(scrollLines)
			} else {
				m.unifiedRightVP.ScrollDown(scrollLines)
			}
		}
	}
	return m, nil
}

// applyLiveEvent updates the active-call map for one event from the watcher.
// On call-end, auto-transitions the call from active to entries.
func (m *ldModel) applyLiveEvent(msg ldLiveEventMsg) {
	a, ok := m.active[msg.CallID]
	if !ok {
		a = &activeCall{ID: msg.CallID, CurrentStep: -1}
		m.active[msg.CallID] = a
		m.activeIDs = append(m.activeIDs, msg.CallID)
	}
	a.Apply(msg.Event)

	// Auto-transition: call-end moves the call from active to entries.
	if msg.Event.Kind == "call-end" {
		m.transitionToEntry(msg.CallID, msg.Event)
	}
}

// transitionToEntry projects an active call into a static CallEntry and
// moves it from m.active to m.entries. This replaces the old dual-store
// reconciliation that relied on calls.jsonl.
func (m *ldModel) transitionToEntry(callID string, endEvent mcp.LiveEvent) {
	a, ok := m.active[callID]
	if !ok {
		return
	}
	entry := a.asCallEntry()
	if endEvent.DurationMs > 0 {
		entry.DurationMs = endEvent.DurationMs
	}
	if endEvent.Error != "" {
		entry.Error = endEvent.Error
	}
	if endEvent.Status != "" && endEvent.Status != "ok" && entry.Error == "" {
		entry.Error = endEvent.Status
	}
	// Build Results from StepStatuses so CallStatus()/statusBadge() work.
	for _, ss := range a.StepStatuses {
		entry.Results = append(entry.Results, mcp.ResultBrief{
			Name:     ss.Name,
			ExitCode: ss.ExitCode,
		})
	}
	// Prepend to entries (newest first).
	m.entries = append([]mcp.CallEntry{entry}, m.entries...)
	delete(m.active, callID)
	delete(m.cellCache, callID) // stale: was live, now static
	m.compactActiveIDs()
}

// compactActiveIDs removes evicted IDs from the order list.
func (m *ldModel) compactActiveIDs() {
	out := m.activeIDs[:0]
	for _, id := range m.activeIDs {
		if _, ok := m.active[id]; ok {
			out = append(out, id)
		}
	}
	m.activeIDs = out
}

// ── Detail keys ─────────────────────────────────────────────────────

func (m ldModel) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		m.view = ldViewUnified
		m.refreshUnifiedView()
		return m, nil
	case "up", "k":
		m.detailVP.ScrollUp(1)
	case "down", "j":
		m.detailVP.ScrollDown(1)
	case "home", "g":
		m.detailVP.GotoTop()
	case "end", "G":
		m.detailVP.GotoBottom()
	case "ctrl+u", "pgup":
		m.detailVP.HalfPageUp()
	case "ctrl+d", "pgdown":
		m.detailVP.HalfPageDown()
	case "left", "h":
		if m.cursor > 0 {
			m.cursor--
			m.detailVP.GotoTop()
			m.buildDetail()
			m.refreshUnifiedView()
		}
	case "right", "l":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			m.detailVP.GotoTop()
			m.buildDetail()
			m.refreshUnifiedView()
		}
	case "D":
		m.view = ldViewUnified
		m.unifiedRightVP.GotoTop()
		m.refreshUnifiedView()
		return m, nil
	case "tab":
		m.layout = (m.layout + 1) % ldLayoutCount
		m.cacheInvalidateAll()
		m.buildDetail()
	case "z":
		m.zoomed = !m.zoomed
		m.buildDetail()
	}
	return m, nil
}

// ── View ────────────────────────────────────────────────────────────

func (m ldModel) View() string {
	if m.view == ldViewDetail {
		return m.viewDetail()
	}
	return m.viewUnified()
}

func (m ldModel) viewDetail() string {
	var b strings.Builder

	if len(m.filtered) == 0 {
		return "no entries"
	}
	idx := m.filtered[m.cursor]
	e := &m.merged[idx]

	// header
	badge := statusBadge(e)
	ts := e.Timestamp.Local().Format("2006-01-02 15:04:05")
	dur := formatDuration(e.DurationMs)
	sid := ui.TruncOrDash(e.SessionID, 12)

	layoutTag := m.layout.String()
	if m.zoomed {
		layoutTag = "zoom"
	}
	fmt.Fprintf(&b, " %s  %s  %s  session:%s  dur:%s  id:%s  [%s]\n\n",
		ldTitle.Render("detail"),
		badge, ts, ldAccent.Render(sid), dur,
		ldDim.Render(e.ID), ldDim.Render(layoutTag))

	// scrollable body — viewport handles scroll window + padding
	b.WriteString(m.detailVP.View())
	b.WriteString("\n")

	// status bar
	pos := ""
	if m.detailVP.TotalLineCount() > m.detailVP.Height {
		pct := int(m.detailVP.ScrollPercent() * 100)
		pos = fmt.Sprintf("  %d%%", pct)
	}
	nav := ""
	if m.cursor > 0 {
		nav += " [h]prev"
	}
	if m.cursor < len(m.filtered)-1 {
		nav += " [l]next"
	}
	zoom := ""
	if m.zoomed {
		zoom = "  " + ldAccent.Render("[ZOOM]")
	}
	lag := m.lagIndicator()
	b.WriteString(ldBar.Render(fmt.Sprintf(
		" [j/k]scroll  [esc/D]unified%s  [tab]layout  [z]zoom  [q]uit  %d/%d%s ",
		nav, m.cursor+1, len(m.filtered), pos)) + zoom + lag)

	return b.String()
}

func (m ldModel) viewportHeight() int {
	h := m.height - 3 // title + filter + statusbar
	if h < 1 {
		return 1
	}
	return h
}

// cachedUnifiedSepLine returns the separator for unified left column.
func (m *ldModel) cachedUnifiedSepLine(w int) string {
	if m.unifiedSepWidth != w {
		m.unifiedSepLine = ldBorder.Render(strings.Repeat("─", w))
		m.unifiedSepWidth = w
	}
	return m.unifiedSepLine
}

// ── Detail rendering (no cap) ───────────────────────────────────────

func (m *ldModel) buildDetail() {
	m.detailVP.Width = m.width
	m.detailVP.Height = max(1, m.height-4)

	if len(m.filtered) == 0 {
		m.detailVP.SetContent("")
		return
	}
	idx := m.filtered[m.cursor]
	e := &m.merged[idx]

	// Right column: output-focused view extracted from the waterfall.
	// Unlike the unified view's execution timeline, the detail view shows
	// clean stdout/stderr blocks per step — a log reader, not a debugger.
	rightLines := m.renderDetailOutput(e)

	if m.zoomed {
		var lines []string
		for _, r := range rightLines {
			lines = append(lines, "  "+r)
		}
		m.detailVP.SetContent(strings.Join(lines, "\n"))
	} else {
		leftW, rightW := m.colWidths()
		leftLines := renderInputLines(e, leftW)

		rows := max(len(leftLines), len(rightLines))
		sep := ldColSep.Render(" │ ")

		var lines []string
		for i := 0; i < rows; i++ {
			l := ""
			if i < len(leftLines) {
				l = leftLines[i]
			}
			r := ""
			if i < len(rightLines) {
				r = rightLines[i]
			}
			lines = append(lines, "  "+ui.PadTo(l, leftW)+sep+ui.PadTo(r, rightW))
		}

		m.detailVP.SetContent(strings.Join(lines, "\n"))
	}
}

// renderDetailOutput produces a clean output-focused right column for the
// detail view. Shows per-step headers with exit status, then raw stdout/stderr
// blocks — a log reader view, distinct from the unified view's execution
// waterfall. For active calls, falls back to renderLiveLines.
func (m *ldModel) renderDetailOutput(e *mcp.CallEntry) []string {
	// Active calls: use the live view.
	if a, ok := m.active[e.ID]; ok {
		w := m.rightColWidth()
		return renderLiveLines(a, w)
	}

	w := m.rightColWidth()

	// Load waterfall steps to extract output.
	_, steps, err := loadStaticWaterfall(e.ID)
	if err != nil || len(steps) == 0 {
		// No waterfall data — show what we can from ResultBrief.
		return renderResultLines(e, w, 0, true)
	}

	var lines []string
	for si, step := range steps {
		if si > 0 {
			lines = append(lines, "")
		}

		// Step header: name [action] → hosts  exit:N  duration
		hdr := ldStepName.Render(step.Name) + " " + ldFaint.Render("["+step.Action+"]")
		if len(step.Hosts) > 0 {
			hdr += " " + ldArrow.Render("→") + " " + ldHostTag.Render(strings.Join(step.Hosts, ", "))
		}
		if step.Ended {
			if step.ExitCode == 0 {
				hdr += "  " + ldOk.Render("✓")
			} else {
				hdr += "  " + ldFail.Render(fmt.Sprintf("✗ exit:%d", step.ExitCode))
			}
			if step.DurationMs > 0 {
				hdr += "  " + ldDim.Render(formatDuration(step.DurationMs))
			}
		}
		lines = append(lines, hdr)

		// Config line
		if step.ConfigLine != "" {
			lines = append(lines, ldDSLConf.Render(step.ConfigLine))
		}
		lines = append(lines, "")

		// Collect all output across all source lines for this step.
		hasOutput := false
		for _, sl := range step.Lines {
			for _, o := range sl.Output {
				hasOutput = true
				text := strings.ReplaceAll(o.Text, "\r", "")
				style := ldStdout
				if o.Stream == "stderr" {
					style = ldWarn
				}
				for _, wl := range wrapToWidth(text, w-2) {
					lines = append(lines, style.Render(wl))
				}
			}
		}
		if !hasOutput {
			lines = append(lines, ldDim.Render("(no output)"))
		}
	}
	return lines
}

// renderLiveLines builds the right-column content for an in-flight call.
//
// Layout:
//   - status header: STATUS line with current step + elapsed
//   - step plan: bullet list, current step highlighted
//   - executing line: most recent trace marker, when present
//   - tail: rolling stdout/stderr buffer (last N lines)
func renderLiveLines(a *activeCall, w int) []string {
	if a == nil {
		return []string{ldDim.Render("(no live data)")}
	}

	var lines []string

	// Header
	statusBadge := ldLiveDot.Render("●") + " " + ldLive.Render("LIVE")
	if a.Done {
		statusBadge = ldDim.Render("DONE")
		if a.DoneStatus != "" && a.DoneStatus != "ok" {
			statusBadge = ldErrTag.Render("ERROR")
		}
	}
	elapsed := time.Since(a.StartedAt).Truncate(time.Second)
	lines = append(lines, fmt.Sprintf("%s  %s elapsed", statusBadge, ldAccent.Render(elapsed.String())))
	lines = append(lines, "")

	// Step plan
	if len(a.StepStatuses) > 0 {
		lines = append(lines, ldSection.Render("STEPS"))
		for i, s := range a.StepStatuses {
			marker := "  "
			nameStyled := ldDim.Render(s.Name)
			switch {
			case s.Ended:
				marker = ldOk.Render("✓ ")
				if s.ExitCode != 0 {
					marker = ldFail.Render("✗ ")
				}
			case s.Started:
				marker = ldLiveDot.Render("▶ ")
				nameStyled = ldLiveStep.Render(s.Name)
			}
			hosts := ""
			if len(s.Hosts) > 0 {
				hosts = " " + ldHostTag.Render("→ "+strings.Join(s.Hosts, ", "))
			}
			meta := ""
			if s.Ended {
				meta = " " + ldDim.Render(fmt.Sprintf("(%dms exit:%d)", s.DurationMs, s.ExitCode))
			} else if i == a.CurrentStep && a.ExecutingCmd != "" {
				meta = " " + ldDim.Render(fmt.Sprintf("(+%ds)", a.ExecutingElapsed))
			}
			ptags := formatParamTags(s.Params)
			lines = append(lines, marker+nameStyled+ldFaint.Render(" ["+s.Action+"]")+hosts+ptags+meta)
		}
		lines = append(lines, "")
	}

	// Currently executing
	if a.ExecutingCmd != "" {
		lines = append(lines, ldSection.Render("EXECUTING"))
		host := a.ExecutingHost
		if host == "" {
			host = "local"
		}
		hdr := fmt.Sprintf("%s %s",
			ldHostTag.Render("["+host+"]"),
			ldDim.Render(fmt.Sprintf("+%ds", a.ExecutingElapsed)))
		lines = append(lines, hdr)
		for _, wrapped := range wrapToWidth(a.ExecutingCmd, w-3) {
			lines = append(lines, "  "+ldLiveExec.Render(wrapped))
		}
		lines = append(lines, "")
	}

	// Live tail of stdout/stderr (last lines that fit; renderer cuts further)
	if len(a.Tail) > 0 {
		lines = append(lines, ldSection.Render("OUTPUT (live)"))
		// Only show stdout + stderr (executing already rendered above).
		// Limit to the most recent ~60 lines so the tail is bounded; the
		// rolling buffer caps at maxTail anyway.
		showFrom := 0
		const tailRender = 60
		shown := 0
		// Walk backwards to count tail-render-eligible lines
		for i := len(a.Tail) - 1; i >= 0 && shown < tailRender; i-- {
			if a.Tail[i].Stream == "executing" {
				continue
			}
			shown++
			showFrom = i
		}
		for i := showFrom; i < len(a.Tail); i++ {
			t := a.Tail[i]
			if t.Stream == "executing" {
				continue
			}
			prefix := ""
			if t.Host != "" {
				prefix = ldHostTag.Render("[" + t.Host + "] ")
			}
			contentStyle := ldLiveStdout
			if t.Stream == "stderr" {
				contentStyle = ldLiveStderr
			}
			for wi, wrapped := range wrapToWidth(t.Text, w-len(t.Host)-5) {
				if wi == 0 {
					lines = append(lines, prefix+contentStyle.Render(wrapped))
				} else {
					lines = append(lines, ldFaint.Render("↪ ")+contentStyle.Render(wrapped))
				}
			}
		}
	}

	return lines
}

// selBgSeq is the raw ANSI sequence for the selection background color.
// We need this directly because lipgloss.Render() doesn't re-apply background
// after the inner content emits \x1b[0m resets (lipgloss issue #520).
// Raw sequences bypass lipgloss, so background adaptation is done by hand:
// deep blue (24) on dark terminals, pale blue (153) on light. Detection is
// cached by termenv, so calling per render line is cheap.
func selBgSeq() string {
	if lipgloss.HasDarkBackground() {
		return "\x1b[48;5;24m"
	}
	return "\x1b[48;5;153m"
}

const ansiResetCS = "\x1b[0m"

// ── Column widths ───────────────────────────────────────────────────

func (m ldModel) colWidths() (int, int) {
	usable := m.width - 6 // gutter + indent + separator + margin
	if usable < 20 {
		usable = 20
	}
	lp, _ := m.layout.ratio()
	left := usable * lp / 100
	right := usable - left
	return left, right
}

// rightColWidth returns the effective right-column width, accounting for zoom.
// When zoomed, the right column spans the full terminal minus a small margin.
func (m ldModel) rightColWidth() int {
	if m.zoomed {
		w := m.width - 4
		if w < 20 {
			return 20
		}
		return w
	}
	_, rw := m.colWidths()
	return rw
}

// ── Input column (shared between list + detail) ─────────────────────

func renderInputLines(e *mcp.CallEntry, w int) []string {
	var lines []string
	for _, raw := range strings.Split(e.Input, "\n") {
		if len(raw) > w && w > 0 {
			for len(raw) > w {
				lines = append(lines, colorizeDSL(raw[:w]))
				raw = raw[w:]
			}
			if raw != "" {
				lines = append(lines, colorizeDSL(raw))
			}
		} else {
			lines = append(lines, colorizeDSL(raw))
		}
	}
	return lines
}

// ── Result column (shared between list + detail) ────────────────────

// renderResultLines renders the right column.
// detailed=true adds clear section labels (OUTPUT/STDOUT/STDERR) for the detail view.
// detailed=false produces compact output for the list view.
func renderResultLines(e *mcp.CallEntry, w int, stdoutCap int, detailed bool) []string {
	if len(e.Results) == 0 && e.Error == "" {
		return []string{ldDim.Render("(no results)")}
	}

	// Filter out fan-out merged results.
	hasPerHostResult := make(map[string]bool)
	for _, r := range e.Results {
		if r.Host != "" {
			hasPerHostResult[r.Name] = true
		}
	}
	displayResults := e.Results[:0:0]
	for _, r := range e.Results {
		if r.Host == "" && hasPerHostResult[r.Name] {
			continue
		}
		displayResults = append(displayResults, r)
	}

	var lines []string
	indent := ldDim.Render("│") + " "

	for i, r := range displayResults {
		host := r.Host
		if host == "" {
			host = "local"
		}

		exitStyle := ldOk
		if r.ExitCode != 0 || r.TimedOut {
			exitStyle = ldFail
		}

		isLast := i == len(displayResults)-1 && e.Error == ""
		connector := "├"
		if isLast {
			connector = "└"
		}

		line := fmt.Sprintf("%s %s %s exit:%s",
			ldDim.Render(connector),
			ldStepName.Render(r.Name),
			ldHostTag.Render("["+host+"]"),
			exitStyle.Render(fmt.Sprintf("%d", r.ExitCode)))
		if r.TimedOut {
			line += " " + ldWarn.Render("TIMEOUT")
		}
		lines = append(lines, line)

		ci := indent
		if isLast {
			ci = "  "
		}

		// $OUTPUT key=value pairs — labeled OUTPUT section in detail mode
		if len(r.Outputs) > 0 {
			if detailed {
				lines = append(lines, ci+ldSection.Render("OUTPUT"))
				// align values: find longest key
				maxK := 0
				for k := range r.Outputs {
					if len(k) > maxK {
						maxK = len(k)
					}
				}
				for k, v := range r.Outputs {
					padK := k + strings.Repeat(" ", maxK-len(k))
					vw := w - maxK - 6
					lines = append(lines, ci+"  "+ldAccent.Render(padK)+ldDim.Render(" = ")+ui.TruncLine(v, vw))
				}
			} else {
				for k, v := range r.Outputs {
					lines = append(lines, ci+ldAccent.Render(k+"=")+ui.TruncLine(v, w-len(k)-3))
				}
			}
		}

		// error
		if r.Error != "" {
			lines = append(lines, ci+ldFail.Render("→ "+ui.TruncLine(r.Error, w-4)))
		}

		// stdout — labeled section in detail mode
		if r.Stdout != "" {
			if detailed {
				lines = append(lines, ci+ldSection.Render("STDOUT"))
			}
			stdoutLines := strings.Split(strings.TrimRight(r.Stdout, "\n"), "\n")
			showMax := len(stdoutLines)
			if stdoutCap > 0 && showMax > stdoutCap {
				showMax = stdoutCap
			}
			contentW := w - 3
			for si := 0; si < showMax; si++ {
				for wi, wl := range wrapToWidth(stdoutLines[si], contentW) {
					if wi == 0 {
						lines = append(lines, ci+ldStdout.Render(wl))
					} else {
						lines = append(lines, ci+ldFaint.Render("↪ ")+ldStdout.Render(wl))
					}
				}
			}
			if showMax < len(stdoutLines) {
				lines = append(lines, ci+ldFaint.Render(fmt.Sprintf("… +%d lines", len(stdoutLines)-showMax)))
			}
		}

		// stderr — labeled section in detail mode (full lines, no cap)
		if r.Stderr != "" && r.Stderr != r.Error {
			stderrLines := strings.Split(strings.TrimRight(r.Stderr, "\n"), "\n")
			if detailed {
				lines = append(lines, ci+ldSection.Render("STDERR"))
				for _, sl := range stderrLines {
					for wi, wl := range wrapToWidth(sl, w-3) {
						if wi == 0 {
							lines = append(lines, ci+ldWarn.Render(wl))
						} else {
							lines = append(lines, ci+ldFaint.Render("↪ ")+ldWarn.Render(wl))
						}
					}
				}
			} else {
				lines = append(lines, ci+ldWarn.Render("stderr:"))
				showMax := 4
				if showMax > len(stderrLines) {
					showMax = len(stderrLines)
				}
				for si := 0; si < showMax; si++ {
					lines = append(lines, ci+ldWarn.Render(ui.TruncLine(stderrLines[si], w-3)))
				}
				if len(stderrLines) > showMax {
					lines = append(lines, ci+ldFaint.Render(fmt.Sprintf("… +%d lines", len(stderrLines)-showMax)))
				}
			}
		}
	}

	if e.Error != "" {
		lines = append(lines, ldDim.Render("└ ")+ldErrTag.Render(ui.TruncLine(e.Error, w-4)))
	}

	return lines
}

// ── Helpers ─────────────────────────────────────────────────────────

func statusBadge(e *mcp.CallEntry) string {
	if e.Error == "interrupted" {
		return ldWarn.Render("✗ interrupted")
	}
	switch e.CallStatus() {
	case "ok":
		return ldOk.Render(" OK ")
	case "fail":
		return ldFail.Render("FAIL")
	case "error":
		return ldErrTag.Render("ERR ")
	}
	return " ?  "
}

// formatParamTags renders step config params as compact styled tags.
// Returns empty string when params is nil/empty.
func formatParamTags(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	// Stable order for display.
	order := []string{"entrypoint", "timeout", "trace", "filter", "continue_on_error"}
	var tags []string
	for _, k := range order {
		if v, ok := params[k]; ok {
			tags = append(tags, k+":"+v)
		}
	}
	if len(tags) == 0 {
		return ""
	}
	return ldDim.Render(" " + strings.Join(tags, " "))
}

func colorizeDSL(line string) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "###"):
		return ldDSLHead.Render(line)
	case strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}"):
		return ldDSLConf.Render(line)
	case trimmed == "":
		return ""
	default:
		return ldDSLBody.Render(line)
	}
}

// wrapToWidth wraps a string to lines of at most w runes.
// Prefers breaking at whitespace within the last 25% of the line.
// Falls back to hard mid-rune break for unbroken strings (long IDs, paths).
func wrapToWidth(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	r := []rune(s)
	if len(r) <= w {
		return []string{s}
	}

	var out []string
	for len(r) > w {
		// look for a space in the last 25% to break cleanly
		breakAt := w
		searchFrom := w * 3 / 4
		if searchFrom < 1 {
			searchFrom = 1
		}
		for i := w - 1; i >= searchFrom; i-- {
			if r[i] == ' ' || r[i] == '\t' {
				breakAt = i
				break
			}
		}
		out = append(out, string(r[:breakAt]))
		// skip the breaking whitespace itself
		if breakAt < len(r) && (r[breakAt] == ' ' || r[breakAt] == '\t') {
			r = r[breakAt+1:]
		} else {
			r = r[breakAt:]
		}
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

// rebuildFiltered recomputes m.merged (entries + active projections) and
// m.filtered (indices into m.merged after filter applied).
//
// Active calls are prepended (newest activity first), then completed
// entries (already newest-first). Active projection IDs that also appear
// in m.entries are skipped — completed calls always render from the
// canonical static entry.
func (m *ldModel) rebuildFiltered() {
	// Remember what the cursor pointed to so we can restore it after the
	// merged/filtered arrays are rebuilt. cursor==0 means "follow newest" —
	// new entries that get prepended keep the cursor on the latest.
	var prevID string
	followNewest := m.cursor == 0
	if !followNewest && m.cursor < len(m.filtered) {
		idx := m.filtered[m.cursor]
		if idx < len(m.merged) {
			prevID = m.merged[idx].ID
		}
	}

	staticIDs := make(map[string]bool, len(m.entries))
	for _, e := range m.entries {
		staticIDs[e.ID] = true
	}

	merged := make([]mcp.CallEntry, 0, len(m.active)+len(m.entries))
	for _, id := range m.activeIDs {
		a := m.active[id]
		if a == nil || staticIDs[id] {
			continue
		}
		merged = append(merged, a.asCallEntry())
	}
	merged = append(merged, m.entries...)
	m.merged = merged

	query := strings.ToLower(m.filter.Value())
	m.filtered = m.filtered[:0]
	for i, e := range m.merged {
		if query == "" {
			m.filtered = append(m.filtered, i)
			continue
		}
		searchable := strings.ToLower(strings.Join([]string{
			e.SessionID, e.Input, e.Error, e.ID,
		}, " "))
		for _, s := range e.Steps {
			searchable += " " + strings.ToLower(s.Name+" "+strings.Join(s.Hosts, " "))
		}
		for _, r := range e.Results {
			searchable += " " + strings.ToLower(r.Name+" "+r.Host+" "+r.Error)
		}
		if strings.Contains(searchable, query) {
			m.filtered = append(m.filtered, i)
		}
	}

	// Restore cursor: follow-newest stays at 0; otherwise find the
	// previously-selected entry by ID so new arrivals don't hijack focus.
	if followNewest || prevID == "" {
		m.cursor = 0
	} else {
		found := false
		for fi, mi := range m.filtered {
			if m.merged[mi].ID == prevID {
				m.cursor = fi
				found = true
				break
			}
		}
		if !found {
			m.cursor = 0
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

// isActiveID reports whether the given entry id corresponds to a live call.
func (m *ldModel) isActiveID(id string) bool {
	if m == nil || m.active == nil {
		return false
	}
	_, ok := m.active[id]
	return ok
}

func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%ds", ms/60000, (ms%60000)/1000)
}

// lagIndicator returns a styled "lag:N" string when data events have been
// dropped within the last 10 seconds, or empty string otherwise.
func (m ldModel) lagIndicator() string {
	if m.droppedEvents == nil {
		return ""
	}
	dropped := m.droppedEvents.Load()
	if dropped == 0 {
		return ""
	}
	lastDrop := time.Unix(0, m.lastDropAt.Load())
	if time.Since(lastDrop) >= 10*time.Second {
		return ""
	}
	return "  " + ldWarn.Render(fmt.Sprintf("lag:%d", dropped))
}

// ── Render cache methods ────────────────────────────────────────────

// renderCell returns a cached renderedCell for the given call ID. On miss
// or stale version, it calls buildCell to re-render and stores the result.
//
// Cache hit condition: c.version >= a.version.Load()
// This IS correct — not flipped. Explanation:
//   - activeCall.version is a monotonically increasing counter bumped on every
//     Apply() mutation. c.version records what version the cell was rendered at.
//   - c.version >= a.version means the cached snapshot reflects the current (or
//     a later) state — HIT. "Later" can't actually happen since version only
//     increases, so in practice it's ==, but >= is the safe comparison.
//   - Completed calls have no active entry (a == nil) → permanent HIT. Their
//     data never changes, so the first render is valid forever (until LRU eviction).
func (m *ldModel) renderCell(id string) *renderedCell {
	if c, ok := m.cellCache[id]; ok {
		a := m.active[id]
		// HIT: completed call (a==nil → immutable) or cached version at/past current.
		if a == nil || c.version >= a.version.Load() {
			return c
		}
	}
	// MISS or stale: re-render.
	cell := m.buildCell(id)
	m.cellCache[id] = cell
	m.cacheLRUMark(id)
	return cell
}

// buildCell renders the unified left column for one call. The right column
// (waterfall) is deferred — it involves disk I/O for static calls and is
// only needed when the entry is selected in unified/detail view.
func (m *ldModel) buildCell(id string) *renderedCell {
	cell := &renderedCell{}

	// Find the entry in merged.
	var e *mcp.CallEntry
	for i := range m.merged {
		if m.merged[i].ID == id {
			e = &m.merged[i]
			break
		}
	}
	if e == nil {
		return cell
	}

	a, isActive := m.active[id]

	// Snapshot version at render time.
	if isActive {
		cell.version = a.version.Load()
	}

	leftW, _ := m.colWidths()
	rightWEff := m.rightColWidth() // zoom-aware width for right column

	// Unified left column.
	cell.left = renderUnifiedCell(e, leftW, isActive)

	// Right column: only eagerly compute for active calls (no disk I/O).
	// Static calls defer right-column build to ensureCellRight().
	if isActive && len(a.StepStatuses) > 0 {
		steps := buildLiveWaterfall(a)
		cell.right = renderWaterfall(steps, rightWEff, a.StartedAt, true)
		cell.hasRight = true
	}

	return cell
}

// ensureCellRight lazily computes the right column (waterfall) for the given
// call. For static calls this reads from disk — only called for the selected
// entry in unified/detail view, never in bulk.
func (m *ldModel) ensureCellRight(id string) []string {
	cell := m.renderCell(id)
	if cell.hasRight {
		return cell.right
	}

	// Find entry.
	var e *mcp.CallEntry
	for i := range m.merged {
		if m.merged[i].ID == id {
			e = &m.merged[i]
			break
		}
	}
	if e == nil {
		cell.hasRight = true
		return nil
	}

	rightW := m.rightColWidth()
	callStart, steps, err := loadStaticWaterfall(e.ID)
	if err != nil || len(steps) == 0 {
		cell.right = renderResultLines(e, rightW, 0, true)
	} else {
		cell.right = renderWaterfall(steps, rightW, callStart, false)
	}
	cell.hasRight = true
	return cell.right
}

// cacheLRUMark records id in the LRU list. Deduplicates by removing prior
// occurrence (if any) and appending to tail. Marks dirty for end-of-frame
// eviction rather than evicting inline during render.
func (m *ldModel) cacheLRUMark(id string) {
	// Remove prior occurrence (linear but list is capped at cellCacheCap).
	for i, v := range m.cacheLRU {
		if v == id {
			m.cacheLRU = append(m.cacheLRU[:i], m.cacheLRU[i+1:]...)
			break
		}
	}
	m.cacheLRU = append(m.cacheLRU, id)
	if len(m.cacheLRU) > cellCacheCap {
		m.cacheLRUDirty = true
	}
}

// cacheEvict runs LRU eviction. Called at end-of-frame (after all render
// walks complete) — never during a render walk, which would re-evict cells
// just rendered in the same frame.
func (m *ldModel) cacheEvict() {
	for len(m.cellCache) > cellCacheCap && len(m.cacheLRU) > 0 {
		victim := m.cacheLRU[0]
		m.cacheLRU = m.cacheLRU[1:]
		delete(m.cellCache, victim)
	}
	m.cacheLRUDirty = false
}

// cacheInvalidateAll clears the entire render cache. Used on layout change
// (tab key) or terminal resize — cells are width-dependent.
func (m *ldModel) cacheInvalidateAll() {
	m.cellCache = make(map[string]*renderedCell)
	m.cacheLRU = m.cacheLRU[:0]
	m.cacheLRUDirty = false
}

// scanCompletedEvents reads all completed event files in events/ and replays
// control events to build CallEntry records. In-progress files are skipped —
// the live watcher handles those. This is the startup replacement for the
// deleted calls.jsonl reader.
func scanCompletedEvents() []mcp.CallEntry {
	dir := mcp.EventsDir()
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var entries []mcp.CallEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		info, err := f.Info()
		if err != nil {
			continue
		}
		callID := callIDFromPath(path)
		if callID == "" {
			continue
		}
		if !eventsFileIsCompleted(path, info.Size()) {
			continue // in-progress — handled by live watcher
		}
		entry, ok := replayToCallEntry(callID, path)
		if ok {
			entries = append(entries, entry)
		}
	}

	// Newest first.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})
	return entries
}

// replayToCallEntry scans a completed event file for control events only
// (call-start, step-start, step-end, call-end) and builds a CallEntry.
// Skips data events (stdout/stderr) for speed.
func replayToCallEntry(callID, path string) (mcp.CallEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return mcp.CallEntry{}, false
	}
	defer f.Close()

	var entry mcp.CallEntry
	entry.ID = callID

	type stepInfo struct {
		name     string
		exitCode int
	}
	var steps []stepInfo

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		// Fast filter: skip data events (vast majority of lines).
		if !bytes.Contains(line, []byte(`"call-`)) &&
			!bytes.Contains(line, []byte(`"step-`)) {
			continue
		}
		ev, err := mcp.ParseLiveEvent(line)
		if err != nil {
			continue
		}
		switch ev.Kind {
		case "call-start":
			entry.Timestamp = ev.Ts
			entry.SessionID = ev.SessionID
			entry.Input = ev.Input
			entry.Steps = ev.Steps
			steps = make([]stepInfo, len(ev.Steps))
			for i, s := range ev.Steps {
				steps[i].name = s.Name
			}
		case "step-end":
			if ev.Step >= 0 && ev.Step < len(steps) {
				steps[ev.Step].exitCode = ev.ExitCode
			}
		case "call-end":
			entry.DurationMs = ev.DurationMs
			if ev.Error != "" {
				entry.Error = ev.Error
			}
			if ev.Status != "" && ev.Status != "ok" && entry.Error == "" {
				entry.Error = ev.Status
			}
		}
	}

	// Build Results from step info.
	for _, si := range steps {
		entry.Results = append(entry.Results, mcp.ResultBrief{
			Name:     si.name,
			ExitCode: si.exitCode,
		})
	}

	return entry, entry.Timestamp != (time.Time{})
}

// pidAlive reports whether the given process is still running.
// Uses signal(0) which checks existence without actually sending a signal.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// classifyInterrupted checks in-progress calls (in m.active) for dead pids
// and transitions them to entries with status "interrupted".
func (m *ldModel) classifyInterrupted() {
	var dead []string
	for id, a := range m.active {
		if a.Done {
			continue
		}
		if a.Pid > 0 && !pidAlive(a.Pid) {
			dead = append(dead, id)
		} else if a.Pid == 0 {
			// No pid recorded — assume interrupted (pre-pid event files).
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		a := m.active[id]
		// Skip calls that never received a call-start event — these are
		// garbage from partial/corrupt event files with no useful data.
		if a.StartedAt.IsZero() {
			delete(m.active, id)
			delete(m.cellCache, id)
			continue
		}
		entry := a.asCallEntry()
		entry.Error = "interrupted"
		// asCallEntry uses time.Since(StartedAt) which is wrong for old/dead
		// calls replayed from disk. Use last tail event as end time instead.
		entry.DurationMs = 0
		if !a.StartedAt.IsZero() {
			for i := len(a.Tail) - 1; i >= 0; i-- {
				if t := a.Tail[i].Ts; !t.IsZero() {
					entry.DurationMs = t.Sub(a.StartedAt).Milliseconds()
					break
				}
			}
		}
		// Build Results from whatever step state we have.
		for _, ss := range a.StepStatuses {
			entry.Results = append(entry.Results, mcp.ResultBrief{
				Name:     ss.Name,
				ExitCode: ss.ExitCode,
			})
		}
		m.entries = append([]mcp.CallEntry{entry}, m.entries...)
		delete(m.active, id)
		delete(m.cellCache, id)
	}
	if len(dead) > 0 {
		m.compactActiveIDs()
	}
}

// dashboardCrashLog appends a structured crash report to disk so panics during
// TUI runs are recoverable. Without this, panic stack traces go to stderr and
// disappear when the tmux pane exits.
func dashboardCrashLog(panicVal interface{}, stack []byte) {
	path := filepath.Join(mcp.StateDir(), "dashboard-crash.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "=== CRASH %s ===\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "panic: %v\n\n%s\n\n", panicVal, stack)
}

// RunLogDashboard launches the log dashboard TUI.
func RunLogDashboard() (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			dashboardCrashLog(r, debug.Stack())
			retErr = fmt.Errorf("dashboard panic: %v (see %s/dashboard-crash.log)", r, mcp.StateDir())
		}
	}()

	m := ldInitialModel()

	// Scan completed events from events/ dir — replaces the deleted calls.jsonl.
	m.entries = scanCompletedEvents()
	m.lastEntryCount = len(m.entries)
	m.rebuildFiltered()

	// Start the live watcher BEFORE the bubbletea program. If it fails we
	// still show the static log — degraded but not broken.
	lw, err := startLiveWatcher(m.liveEventCh)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: live watcher unavailable: %v\n", err)
	} else {
		// Share drop counters so the watcher increments and the view reads.
		lw.droppedEvents = m.droppedEvents
		lw.lastDropAt = m.lastDropAt
	}
	defer lw.Close()

	// Drain initial backfill events from the live watcher so in-progress calls
	// are populated before we classify interrupted ones.
	drainTimeout := time.After(500 * time.Millisecond)
drainLoop:
	for {
		select {
		case msg := <-m.liveEventCh:
			m.applyLiveEvent(msg)
		case <-drainTimeout:
			break drainLoop
		}
	}
	m.classifyInterrupted()
	m.rebuildFiltered()

	// WithFPS(30) caps View() calls to 30Hz — bounds string assembly + ANSI cost.
	// Does NOT throttle Update() calls or message processing.
	// Combined with view cache: Update path is cheap, so unbounded Update rate is OK.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFPS(30))
	_, err = p.Run()
	return err
}
