package dashboard

// Unified detail view: iPad-Mail-style two-column layout.
//
//   Left column: fixed-height cells, one per call-log entry. Selected entry
//   is highlighted; cursor moves with j/k.
//
//   Right column: execution waterfall for the selected entry. Source lines
//   from the script body are shown as the primary rows; sub-command ticks
//   from the DEBUG trap roll up under each source line; stdout/stderr is
//   indented under the line that produced it.

import (
	"fmt"
	"strings"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/ui"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ─────────────────────────────────────────────────────────────

var (
	wfPending = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))            // grey
	wfDoneCmd = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))            // dim white
	wfExecCmd = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")) // bright bold
	wfSubLine = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))            // dimmer for sub-cmd detail
	wfTimer   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))            // static timer
	wfTimerOn = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))            // live timer (cyan)
	// Output block: near-black inset (#121212) with ▐ gutter bar.
	// Alternative blue tint: Background(lipgloss.Color("#161622"))
	wfOutput    = lipgloss.NewStyle().Foreground(lipgloss.Color("#c0caf5")).Background(lipgloss.Color("#1a2040"))
	wfOutErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e")).Background(lipgloss.Color("#1a2040"))
	wfOutGutter = lipgloss.NewStyle().Foreground(lipgloss.Color("60"))
	wfErrGutter = lipgloss.NewStyle().Foreground(lipgloss.Color("131"))
	wfStepHdr   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213")) // pink
	wfRail      = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))            // ┌ │ └
	wfSepLine   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))            // ╌╌╌ separator
	wfSepTime   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))            // cumulative time on separator
	wfOpBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))            // operator label
	wfRepeat    = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))            // ↻ for repeated subs (loops)
	wfRepCount  = lipgloss.NewStyle().Foreground(lipgloss.Color("215"))            // [×N] count badge
)

// ── Constants ──────────────────────────────────────────────────────────

const unifiedCellHeight = 3 // content lines per left-column cell (excl. separator)

// ── Cell renderer (left column) ────────────────────────────────────────

func renderUnifiedCell(e *mcp.CallEntry, w int, isActive bool) []string {
	if w < 10 {
		w = 10
	}

	// Line 1: status + timestamp + duration
	badge := statusBadge(e)
	if isActive {
		badge = ldLive.Render("LIVE")
	}
	ts := e.Timestamp.Local().Format("01-02 15:04")
	dur := formatDuration(e.DurationMs)
	line1 := fmt.Sprintf("%s %s %s", badge, ldDim.Render(ts), ldDim.Render(dur))

	// Line 2: first step name + action + hosts (+N more)
	line2 := ""
	if len(e.Steps) > 0 {
		s := e.Steps[0]
		nameMax := w / 2
		if nameMax < 8 {
			nameMax = 8
		}
		nm := ui.Truncate(s.Name, nameMax)
		line2 = ldStepName.Render(nm) + " " + ldFaint.Render("["+s.Action+"]")
		if len(s.Hosts) > 0 {
			hosts := ui.Truncate(strings.Join(s.Hosts, ","), w/3+1)
			line2 += " " + ldArrow.Render("→") + " " + ldHostTag.Render(hosts)
		}
		if len(e.Steps) > 1 {
			line2 += " " + ldDim.Render(fmt.Sprintf("+%d", len(e.Steps)-1))
		}
	}

	// Line 3: first script-line body preview
	preview := ""
	if len(e.Steps) > 0 && e.Steps[0].BodyPreview != "" {
		preview = e.Steps[0].BodyPreview
	} else {
		preview = e.InputPreview(w)
	}
	line3 := ldFaint.Render(ui.Truncate(preview, w))

	return []string{line1, line2, line3}
}

// ── Waterfall renderer (right column) ──────────────────────────────────

// renderWaterfall produces the right-column lines for a built waterfall.
// callStart, now, isActive: live timing context.
func renderWaterfall(steps []waterfallStep, w int, callStart time.Time, isActive bool) []string {
	if len(steps) == 0 {
		return []string{ldDim.Render("  (no steps)")}
	}
	now := time.Now()
	if !isActive {
		// For static rendering, "now" doesn't matter — but use callStart so
		// in-flight lines (shouldn't happen for static) don't blow up.
		now = callStart
	}

	var lines []string
	for si, step := range steps {
		if si > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, renderStepHeader(step, w))
		lines = append(lines, renderStepBody(step, w, callStart, now, isActive)...)
		lines = append(lines, renderStepFooter(step, w))
	}
	return lines
}

func renderStepHeader(step waterfallStep, w int) string {
	hosts := ""
	if len(step.Hosts) > 0 {
		hosts = " " + ldArrow.Render("→") + " " + ldHostTag.Render(strings.Join(step.Hosts, ", "))
	}
	stat := ""
	switch {
	case step.Ended && step.ExitCode == 0:
		stat = " " + ldOk.Render("✓")
	case step.Ended:
		stat = " " + ldFail.Render(fmt.Sprintf("✗ exit:%d", step.ExitCode))
	case step.Started:
		stat = " " + ldLiveDot.Render("●")
	default:
		stat = " " + ldDim.Render("○")
	}
	if step.Ended && step.DurationMs > 0 {
		stat += " " + ldDim.Render(formatDuration(step.DurationMs))
	}
	return wfRail.Render("┌─ ") + wfStepHdr.Render(step.Name) + " " +
		ldFaint.Render("["+step.Action+"]") + hosts + stat
}

func renderStepFooter(step waterfallStep, w int) string {
	if step.Ended {
		if step.ExitCode == 0 {
			return wfRail.Render("└─ ") + ldOk.Render("✓")
		}
		return wfRail.Render("└─ ") +
			ldFail.Render(fmt.Sprintf("✗ exit:%d", step.ExitCode)) +
			"  " + ldDim.Render(formatDuration(step.DurationMs))
	}
	if step.Started {
		return wfRail.Render("│  ") + ldLiveDot.Render("●") + " " + ldDim.Render("running…")
	}
	return wfRail.Render("│  ") + ldDim.Render("(pending)")
}

// renderStepBody renders all source lines for one step. Source lines that
// haven't started executing render as grey "pending" rows. Started lines show
// marker + text + timer, plus sub-command detail and output beneath.
func renderStepBody(step waterfallStep, w int, callStart, now time.Time, isActive bool) []string {
	if !step.Started && len(step.Lines) == 0 {
		return []string{wfRail.Render("│  ") + ldDim.Render("(no script body)")}
	}

	rail := wfRail.Render("│ ")
	railW := 2 // visible width of "│ "

	// Find which line is currently executing (last started, not done).
	curIdx := -1
	for i := len(step.Lines) - 1; i >= 0; i-- {
		if step.Lines[i].Started && !step.Lines[i].Done {
			curIdx = i
			break
		}
	}

	var lines []string
	for i := range step.Lines {
		line := &step.Lines[i]
		if line.Skipped {
			continue
		}

		hasContent := len(line.Output) > 0 || len(line.Subs) > 0

		switch {
		case line.Done:
			lines = append(lines, renderDoneLine(line, rail, railW, w, callStart)...)
			lines = append(lines, renderSubsAndOutput(line, rail, railW, w, false)...)
			if hasContent {
				lines = append(lines, renderCumulativeSep(line, rail, railW, w, callStart))
			}
		case line.Started:
			lines = append(lines, renderExecLine(line, rail, railW, w, callStart, now, isActive)...)
			lines = append(lines, renderSubsAndOutput(line, rail, railW, w, true)...)
			if hasContent {
				lines = append(lines, renderCumulativeSep(line, rail, railW, w, callStart))
			}
		default:
			lines = append(lines, renderPendingLine(line, rail, railW, w)...)
		}
		_ = curIdx
	}

	return lines
}

func renderDoneLine(line *wfSourceLine, rail string, railW, w int, callStart time.Time) []string {
	marker := ldOk.Render("✓")
	return composeLine(rail, marker, wfDoneCmd, line.Text, wfTimer, "", w, railW)
}

func renderExecLine(line *wfSourceLine, rail string, railW, w int, callStart, now time.Time, isActive bool) []string {
	marker := ldLiveDot.Render("▶")
	return composeLine(rail, marker, wfExecCmd, line.Text, wfTimerOn, "", w, railW)
}

func renderPendingLine(line *wfSourceLine, rail string, railW, w int) []string {
	marker := ldFaint.Render("○")
	prefixW := railW + ui.VisibleLen(marker) + 1
	cmdW := w - prefixW
	if cmdW < 8 {
		cmdW = 8
	}
	text := strings.TrimSpace(line.Text)
	wrapped := wrapToWidth(text, cmdW)
	prefix := rail + marker + " "
	contPrefix := rail + strings.Repeat(" ", ui.VisibleLen(marker)+1)
	lines := []string{prefix + wfPending.Render(wrapped[0])}
	for _, wl := range wrapped[1:] {
		lines = append(lines, contPrefix+wfPending.Render(wl))
	}
	return lines
}

func renderCumulativeSep(line *wfSourceLine, rail string, railW, w int, callStart time.Time) string {
	timeLabel := ""
	if !line.EndTs.IsZero() {
		cumulative := line.EndTs.Sub(callStart)
		timeLabel = formatPreciseDur(cumulative)
	}
	contentW := w - railW
	timeLabelW := len(timeLabel)
	dashW := contentW - timeLabelW - 1 // 1 space before time
	if dashW < 4 {
		dashW = 4
	}
	dashes := strings.Repeat("╌", dashW)
	return rail + wfSepLine.Render(dashes) + " " + wfSepTime.Render(timeLabel)
}

// composeLine: rail + marker + cmdText (wrapped) + right-aligned timer on first line.
func composeLine(rail, marker string, cmdStyle lipgloss.Style, cmdText string, timerStyle lipgloss.Style, timerStr string, w, railW int) []string {
	prefix := rail + marker + " "
	prefixW := railW + ui.VisibleLen(marker) + 1
	timerW := 0
	if timerStr != "" {
		timerW = len(timerStr) + 2 // 2 char gap
	}
	cmdMaxW := w - prefixW - timerW
	if cmdMaxW < 8 {
		cmdMaxW = 8
	}

	cmdTrimmed := strings.TrimSpace(cmdText)

	// Fits on one line — no wrapping needed.
	if len(cmdTrimmed) <= cmdMaxW {
		cmdR := cmdStyle.Render(cmdTrimmed)
		if timerStr == "" {
			return []string{prefix + cmdR}
		}
		used := prefixW + ui.VisibleLen(cmdR)
		pad := w - used - len(timerStr)
		if pad < 1 {
			pad = 1
		}
		return []string{prefix + cmdR + strings.Repeat(" ", pad) + timerStyle.Render(timerStr)}
	}

	// Wrap: first line at cmdMaxW (leaves room for timer), continuation at full width.
	contW := w - prefixW
	if contW < 8 {
		contW = 8
	}

	firstWrapped := wrapToWidth(cmdTrimmed, cmdMaxW)
	firstPart := firstWrapped[0]
	rest := strings.TrimSpace(cmdTrimmed[len([]rune(firstPart)):])

	// First line with timer.
	firstR := cmdStyle.Render(firstPart)
	var line1 string
	if timerStr == "" {
		line1 = prefix + firstR
	} else {
		used := prefixW + ui.VisibleLen(firstR)
		pad := w - used - len(timerStr)
		if pad < 1 {
			pad = 1
		}
		line1 = prefix + firstR + strings.Repeat(" ", pad) + timerStyle.Render(timerStr)
	}
	lines := []string{line1}

	// Continuation lines: rail + indent, no marker.
	if rest != "" {
		contPrefix := rail + strings.Repeat(" ", ui.VisibleLen(marker)+1)
		for _, wl := range wrapToWidth(rest, contW) {
			lines = append(lines, contPrefix+cmdStyle.Render(wl))
		}
	}
	return lines
}

// subGroup collapses consecutive same-text DEBUG-trap fires (typical for loop
// iterations) so the waterfall doesn't show 30 identical rows for a `for`
// loop. Pipelines (distinct sub texts) stay as individual groups.
type subGroup struct {
	first wfSub
	last  wfSub
	count int
	total time.Duration // sum of (EndTs - StartTs) across all subs in group
}

// groupSubs collapses subs by Cmd text. Order preserved by first occurrence.
func groupSubs(subs []wfSub) []subGroup {
	if len(subs) == 0 {
		return nil
	}
	var groups []subGroup
	seen := map[string]int{}
	for _, s := range subs {
		if idx, ok := seen[s.Cmd]; ok {
			groups[idx].count++
			groups[idx].last = s
			if !s.StartTs.IsZero() && !s.EndTs.IsZero() {
				groups[idx].total += s.EndTs.Sub(s.StartTs)
			}
			continue
		}
		g := subGroup{first: s, last: s, count: 1}
		if !s.StartTs.IsZero() && !s.EndTs.IsZero() {
			g.total = s.EndTs.Sub(s.StartTs)
		}
		seen[s.Cmd] = len(groups)
		groups = append(groups, g)
	}
	return groups
}

// renderSubsAndOutput renders sub-command detail rows + output rows below a
// source line. forActive=true uses bright styling for the most-recent sub.
func renderSubsAndOutput(line *wfSourceLine, rail string, railW, w int, forActive bool) []string {
	var out []string

	groups := groupSubs(line.Subs)

	// Show sub-command detail when:
	//   - multiple groups (pipeline, conditional with distinct sub-cmds)
	//   - single group whose text differs from the source line (e.g. bash
	//     normalized whitespace, or compound that reduced to one operand)
	//   - single group that fired many times (loop iteration count is useful
	//     info even when text matches source)
	singleGroupMatchesSource := len(groups) == 1 &&
		normalizeCmd(groups[0].first.Cmd) == normalizeCmd(strings.TrimSpace(line.Text))
	showSubs := !singleGroupMatchesSource || (len(groups) == 1 && groups[0].count > 1)

	if showSubs {
		for i, g := range groups {
			isLast := i == len(groups)-1
			out = append(out, renderSubGroup(line, g, rail, railW, w, forActive && isLast))
		}
	}

	// Output: ▐ gutter bar + inset background, wrapped.
	prevBlank := false
	for _, o := range line.Output {
		// Strip \r so carriage returns from SSH/remote output don't move the
		// cursor to column 0, causing trailing padding to overwrite content.
		oText := strings.ReplaceAll(o.Text, "\r", "")
		if strings.TrimSpace(oText) == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
		} else {
			prevBlank = false
		}
		gutter := wfOutGutter.Render("▐")
		styled := wfOutput
		if o.Stream == "stderr" {
			gutter = wfErrGutter.Render("▐")
			styled = wfOutErr
		}
		contentW := w - railW - 1
		for wi, wl := range wrapToWidth(oText, contentW-1) {
			prefix := " "
			if wi > 0 {
				prefix = " "
			}
			text := prefix + wl
			pad := contentW - ui.VisibleLen(text)
			if pad > 0 {
				text += strings.Repeat(" ", pad)
			}
			out = append(out, rail+gutter+styled.Render(text))
		}
	}

	return out
}

// renderSubGroup renders one grouped sub-command row. count=1 → normal
// single-fire row; count>1 → loop-iteration collapse with ↻ marker and
// [×N] badge.
func renderSubGroup(line *wfSourceLine, g subGroup, rail string, railW, w int, isLastActive bool) string {
	// Operator badge from matched operand (use first sub's match).
	// Suppressed for collapsed groups (loops): the ↻ marker + [×N] already
	// convey loop membership; showing ";" from for-loop syntax is misleading.
	var opBadge string
	if g.count > 1 {
		opBadge = "  "
	} else if g.first.OperandIdx >= 0 && g.first.OperandIdx < len(line.Operands) {
		op := line.Operands[g.first.OperandIdx].Op
		if op == "" {
			opBadge = "  "
		} else {
			opBadge = wfOpBadge.Render(padStr(op, 2))
		}
	} else {
		opBadge = "  "
	}

	// Marker reflects state: ↻ for repeated (loop iteration), ▶ for the
	// currently-executing last group, ✓ otherwise.
	var mark string
	switch {
	case g.count > 1:
		mark = wfRepeat.Render("↻")
	case isLastActive:
		mark = ldLiveDot.Render("▶")
	default:
		mark = ldOk.Render("✓")
	}

	// Timer:
	//   single-fire: "(+0.05s)"
	//   collapsed:   "[×30]  0.30s total"  (sum of all iterations)
	var timerStr string
	if g.count > 1 {
		if g.total > 0 {
			avg := g.total / time.Duration(g.count)
			timerStr = fmt.Sprintf("[×%d]  ~%s/iter", g.count, formatPreciseDur(avg))
		} else {
			timerStr = fmt.Sprintf("[×%d]", g.count)
		}
	} else if !g.first.StartTs.IsZero() && !g.first.EndTs.IsZero() {
		timerStr = "(+" + formatPreciseDur(g.first.EndTs.Sub(g.first.StartTs)) + ")"
	}

	prefix := rail + "    " + opBadge + mark + " "
	prefixW := railW + 4 + 2 + ui.VisibleLen(mark) + 1
	timerW := 0
	if timerStr != "" {
		timerW = len(timerStr) + 2
	}
	cmdMaxW := w - prefixW - timerW
	if cmdMaxW < 8 {
		cmdMaxW = 8
	}
	cmd := wfSubLine.Render(ui.Truncate(strings.TrimSpace(g.first.Cmd), cmdMaxW))
	if timerStr == "" {
		return prefix + cmd
	}
	pad := w - prefixW - ui.VisibleLen(cmd) - len(timerStr)
	if pad < 1 {
		pad = 1
	}
	timerStyle := wfTimer
	if g.count > 1 {
		timerStyle = wfRepCount
	}
	return prefix + cmd + strings.Repeat(" ", pad) + timerStyle.Render(timerStr)
}

func padStr(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// ── Selection helper (column-scoped) ───────────────────────────────────

func selectionLineW(content string, w int) string {
	gutter := ldSelGutter.Render("▎")
	target := w - 1
	if target < 1 {
		return gutter
	}
	patched := strings.ReplaceAll(content, ansiResetCS, ansiResetCS+selBgSeq)
	visW := ui.VisibleLen(content) + 1
	pad := ""
	if visW < target {
		pad = strings.Repeat(" ", target-visW)
	}
	return gutter + selBgSeq + " " + patched + pad + ansiResetCS
}

// ── Unified view: model integration ────────────────────────────────────

func (m *ldModel) refreshUnifiedView() {
	m.refreshUnifiedLeft()
	m.refreshUnifiedRight()
}

func (m *ldModel) refreshUnifiedLeft() {
	leftW, _ := m.colWidths()
	cellTotalW := leftW + 2
	vh := m.viewportHeight()
	m.unifiedLeftVP.Width = cellTotalW
	m.unifiedLeftVP.Height = vh

	sep := m.cachedUnifiedSepLine(cellTotalW)

	estLines := len(m.filtered) * (unifiedCellHeight + 1)
	lines := make([]string, 0, estLines)
	if cap(m.unifiedCellStarts) >= len(m.filtered) {
		m.unifiedCellStarts = m.unifiedCellStarts[:0]
	} else {
		m.unifiedCellStarts = make([]int, 0, len(m.filtered))
	}

	for fi, ei := range m.filtered {
		m.unifiedCellStarts = append(m.unifiedCellStarts, len(lines))
		e := &m.merged[ei]
		selected := fi == m.cursor
		cached := m.renderCell(e.ID)
		for _, cl := range cached.left {
			padded := ui.PadTo(cl, leftW)
			if selected {
				lines = append(lines, selectionLineW(padded, cellTotalW))
			} else {
				lines = append(lines, "  "+padded)
			}
		}
		lines = append(lines, sep)
	}
	if m.cacheLRUDirty {
		m.cacheEvict()
	}

	m.unifiedLeftVP.SetContent(strings.Join(lines, "\n"))

	// Cursor-following: ensure selected cell is visible.
	if m.cursor >= 0 && m.cursor < len(m.unifiedCellStarts) {
		cellStart := m.unifiedCellStarts[m.cursor]
		cellEnd := cellStart + unifiedCellHeight + 1
		if cellStart < m.unifiedLeftVP.YOffset {
			m.unifiedLeftVP.SetYOffset(cellStart)
		}
		if cellEnd > m.unifiedLeftVP.YOffset+vh {
			m.unifiedLeftVP.SetYOffset(cellEnd - vh)
		}
	}
}

func (m *ldModel) refreshUnifiedRight() {
	_, rightW := m.colWidths()
	if m.zoomed {
		rightW = m.width - 2
		if rightW < 20 {
			rightW = 20
		}
	}
	vh := m.viewportHeight()
	m.unifiedRightVP.Width = rightW
	m.unifiedRightVP.Height = vh

	if len(m.filtered) == 0 {
		m.unifiedRightVP.SetContent(ldDim.Render("  (no entries)"))
		return
	}

	idx := m.filtered[m.cursor]
	e := &m.merged[idx]

	// ensureCellRight lazily computes waterfall (disk I/O) only for selected entry.
	rightLines := m.ensureCellRight(e.ID)

	// Sticky-bottom: if at bottom or at offset 0 (fresh/cursor-change),
	// snap to bottom for active calls after content update.
	_, isActive := m.active[e.ID]
	wasAtBottom := m.unifiedRightVP.AtBottom()
	wasAtTop := m.unifiedRightVP.YOffset == 0

	m.unifiedRightVP.SetContent(strings.Join(rightLines, "\n"))

	if isActive && (wasAtBottom || wasAtTop) {
		m.unifiedRightVP.GotoBottom()
	}
}

// ── Unified view rendering ─────────────────────────────────────────────

func (m ldModel) viewUnified() string {
	var b strings.Builder
	vh := m.viewportHeight()

	// Title line
	title := ldTitle.Render("shellkit logs")
	stats := ldDim.Render(fmt.Sprintf(" %d entries", len(m.merged)))
	if q := m.filter.Value(); q != "" {
		stats += "  " + ldAccent.Render(fmt.Sprintf("/%s (%d)", q, len(m.filtered)))
	}
	tag := "unified"
	if m.zoomed {
		tag = "zoom"
	}
	stats += "  " + ldDim.Render("["+tag+"]")
	fmt.Fprintf(&b, " %s%s\n", title, stats)

	// Filter input row / breadcrumb
	if m.filtering {
		b.WriteString(" " + m.filter.View() + "\n")
	} else if len(m.filtered) > 0 {
		b.WriteString(" " + m.selectedBreadcrumb() + "\n")
	} else {
		b.WriteString("\n")
	}

	if m.zoomed {
		// Zoomed: full-width right column only
		rightLines := strings.Split(m.unifiedRightVP.View(), "\n")
		for i := 0; i < vh; i++ {
			r := ""
			if i < len(rightLines) {
				r = rightLines[i]
			}
			b.WriteString(ui.PadTo(r, m.width))
			b.WriteString("\n")
		}
	} else {
		// Normal: left cell list + right waterfall
		_, rightW := m.colWidths()
		sep := ldColSep.Render(" │ ")
		cellTotalW := m.unifiedLeftVP.Width
		leftLines := strings.Split(m.unifiedLeftVP.View(), "\n")
		rightLines := strings.Split(m.unifiedRightVP.View(), "\n")
		for i := 0; i < vh; i++ {
			l := ""
			if i < len(leftLines) {
				l = leftLines[i]
			}
			r := ""
			if i < len(rightLines) {
				r = rightLines[i]
			}
			b.WriteString(ui.PadTo(l, cellTotalW) + sep + ui.PadTo(r, rightW))
			b.WriteString("\n")
		}
	}

	// Status bar
	pos := ""
	if m.unifiedRightVP.TotalLineCount() > vh {
		pct := int(m.unifiedRightVP.ScrollPercent() * 100)
		pos = fmt.Sprintf("  %d%%", pct)
	}
	cur := 0
	if len(m.filtered) > 0 {
		cur = m.cursor + 1
	}
	zoom := ""
	if m.zoomed {
		zoom = "  " + ldAccent.Render("[ZOOM]")
	}
	lag := m.lagIndicator()
	b.WriteString(ldBar.Render(fmt.Sprintf(
		" [j/k]nav  [C-d/u]scroll  [tab]layout  [z]zoom  [/]search  [esc]list  [q]uit  %d/%d%s ",
		cur, len(m.filtered), pos)) + zoom + lag)
	return b.String()
}

// selectedBreadcrumb returns a dim contextual line showing metadata about the
// currently-selected entry: call ID, step name, hosts, and timing.
func (m ldModel) selectedBreadcrumb() string {
	idx := m.filtered[m.cursor]
	e := &m.merged[idx]

	var parts []string

	// Short call ID
	id := e.ID
	if len(id) > 8 {
		id = id[:8]
	}
	parts = append(parts, ldDim.Render("call:")+ldAccent.Render(id))

	// Step name + action
	if len(e.Steps) > 0 {
		s := e.Steps[0]
		parts = append(parts, ldDim.Render("step:")+ldStepName.Render(s.Name))
		if len(s.Hosts) > 0 {
			parts = append(parts, ldArrow.Render("→")+" "+ldHostTag.Render(strings.Join(s.Hosts, ",")))
		}
	}

	// Timestamp
	parts = append(parts, ldDim.Render(e.Timestamp.Local().Format("15:04:05")))

	// Duration or live badge
	if m.isActiveID(e.ID) {
		elapsed := time.Since(e.Timestamp)
		parts = append(parts, ldLiveDot.Render("●")+" "+ldDim.Render(formatCompactDur(elapsed)))
	} else if e.DurationMs > 0 {
		parts = append(parts, ldDim.Render(formatDuration(e.DurationMs)))
	}

	// JSON config lines from the DSL input (e.g. {"ssh": "host", "timeout": 30})
	if jsonConf := extractJSONLines(e.Input); jsonConf != "" {
		metaW := 0
		for _, p := range parts {
			metaW += ui.VisibleLen(p) + 2
		}
		confW := m.width - metaW - 6
		if confW > 20 {
			parts = append(parts, ldDSLConf.Render(ui.Truncate(jsonConf, confW)))
		}
	}

	return strings.Join(parts, "  ")
}

// ── Unified view key handling ──────────────────────────────────────────

func (m ldModel) handleUnifiedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.String() {
		case "enter", "esc":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.rebuildFiltered()
			m.refreshUnifiedView()
			return m, cmd
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.view = ldViewList
		m.refreshListView()
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.unifiedRightVP.GotoTop()
			m.refreshUnifiedView()
		}
	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			m.unifiedRightVP.GotoTop()
			m.refreshUnifiedView()
		}
	case "home", "g":
		m.cursor = 0
		m.unifiedRightVP.GotoTop()
		m.refreshUnifiedView()
	case "end", "G":
		m.cursor = max(0, len(m.filtered)-1)
		m.unifiedRightVP.GotoTop()
		m.refreshUnifiedView()
	case "ctrl+d", "pgdown":
		m.unifiedRightVP.HalfPageDown()
	case "ctrl+u", "pgup":
		m.unifiedRightVP.HalfPageUp()
	case "shift+g":
		m.unifiedRightVP.GotoBottom()
	case "tab":
		m.layout = (m.layout + 1) % ldLayoutCount
		m.cacheInvalidateAll()
		m.refreshUnifiedView()
	case "z":
		m.zoomed = !m.zoomed
		m.cacheInvalidateAll() // right-column lines are width-dependent
		m.refreshUnifiedView()
	case "/":
		m.filtering = true
		m.filter.SetValue("")
		m.filter.Focus()
		m.rebuildFiltered()
		m.refreshUnifiedView()
		return m, textinput.Blink
	}
	return m, nil
}
