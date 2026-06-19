package dashboard

// Execution waterfall: per-call view that shows the original script lines as
// they were written, with sub-command ticks (from bash DEBUG-trap fires)
// rolling up under each source line.
//
// Mapping: the trap reports $LINENO, which is the line number in the bash-
// executed script. prependTrace adds exactly one trap line at the top, so:
//
//	user body line 0 (1-indexed: 1) → script LINENO 2
//	user body line 1 (1-indexed: 2) → script LINENO 3
//
// trapLineOffset captures this so callers index by the LineNo reported on
// "executing" events directly.

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/caoer/shellkit/internal/mcp"
)

const trapLineOffset = 1 // prependTrace adds exactly one line at top

// maxOutputPerLine caps the Output slice on each wfSourceLine. The waterfall
// only renders a viewport-sized window anyway; keeping unbounded output wastes
// memory and makes append O(n) when the slice triggers a copy-on-grow.
const maxOutputPerLine = 200

// ── Types ──────────────────────────────────────────────────────────────

// waterfallStep is one step in the call. Source lines from the step body
// become wfSourceLine entries; sub-command ticks attach to whichever source
// line they originated from (matched by LINENO).
type waterfallStep struct {
	Name       string
	Action     string
	Hosts      []string
	Params     map[string]string // config params: timeout, trace, entrypoint, etc.
	Started    bool
	Ended      bool
	ExitCode   int
	DurationMs int64
	Lines      []wfSourceLine
}

// wfSourceLine is one line from the user's script body.
type wfSourceLine struct {
	LineNo  int    // script LINENO (== bodyIndex + trapLineOffset + 1)
	Text    string // raw text as written
	Skipped bool   // true if this line is blank/comment/JSON config (not executed)

	Operands []wfOperand // shell-operator-split sub-commands of Text
	Subs     []wfSub     // actual fires from DEBUG trap, ordered by arrival

	StartTs time.Time // first sub fire ts (when this line started executing)
	EndTs   time.Time // last sub fire ts, or step-end if last line
	Started bool
	Done    bool    // a later line started executing (so this one is finalized)
	Output  []wfOut // stdout/stderr captured between this line's first sub and the next line's first sub
}

// wfOperand is one operator-split piece of a source line, e.g. for
//
//	ps aux | grep nixos || echo none
//
// operands are: [{cmd:"ps aux", op:""}, {cmd:"grep nixos", op:"|"}, {cmd:"echo none", op:"||"}]
type wfOperand struct {
	Op  string // operator that PRECEDES this operand: "" | "|" | "|&" | "&&" | "||" | ";"
	Cmd string // text of the simple command (whitespace-trimmed)
}

// wfSub is one DEBUG-trap fire — bash actually executed this simple command.
type wfSub struct {
	Cmd        string    // BASH_COMMAND text
	StartTs    time.Time // trap fire ts
	EndTs      time.Time // next sub's ts or line end
	Host       string
	OperandIdx int // best-guess match into Lines.Operands; -1 if no match
}

// wfOut is one stdout/stderr line captured during a source line's execution.
type wfOut struct {
	Stream string // "stdout" | "stderr"
	Text   string
	Host   string
	Ts     time.Time
}

// ── Body parsing ───────────────────────────────────────────────────────

// parseStepBodies returns the script body of each step as the same set of
// lines bash actually sees (post DSL-parse: JSON config and leading blanks
// stripped). Each line carries its script LINENO so events' line_no maps
// directly. Falls back to a tolerant raw split if ParseDSL rejects the input.
func parseStepBodies(input string) [][]wfSourceLine {
	steps, err := mcp.ParseDSL(input)
	if err != nil || len(steps) == 0 {
		return parseStepBodiesRaw(input)
	}
	var bodies [][]wfSourceLine
	for _, s := range steps {
		bodies = append(bodies, sourceLinesFromBody(s.Body))
	}
	return bodies
}

// sourceLinesFromBody converts a raw step body (already stripped by the DSL
// parser) into wfSourceLine entries with proper LINENO + operator splitting.
//
// Skipped: blank lines, pure-# comments, standalone control-flow keywords
// (`done`, `fi`, `else`, `then`, `do`), and lines inside unclosed single
// quotes or heredocs. Bash never fires the DEBUG trap for these — showing
// them as "pending" would be visual noise.
//
// Multi-line single-quote tracking: when a line opens a single-quote without
// closing it (e.g. `ssh root@host '`), subsequent lines are part of that
// quoted argument — not independent commands. They're marked Skipped until
// the closing quote is found. Same for heredocs (`<<EOF` / `<<'EOF'`).
func sourceLinesFromBody(body string) []wfSourceLine {
	var lines []wfSourceLine
	inSingleQuote := false // inside an unclosed multi-line single-quote
	inHeredoc := false     // inside a heredoc
	heredocDelim := ""     // expected heredoc closing delimiter

	for i, line := range strings.Split(body, "\n") {
		ln := i + 1 + trapLineOffset
		trimmed := strings.TrimSpace(line)

		// Check heredoc termination first.
		if inHeredoc {
			if trimmed == heredocDelim {
				inHeredoc = false
				heredocDelim = ""
			}
			lines = append(lines, wfSourceLine{LineNo: ln, Text: line, Skipped: true})
			continue
		}

		// Lines inside an unclosed single-quote are continuation — skip.
		if inSingleQuote {
			// Check if this line closes the quote.
			if closesMultiLineSingleQuote(trimmed) {
				inSingleQuote = false
			}
			lines = append(lines, wfSourceLine{LineNo: ln, Text: line, Skipped: true})
			continue
		}

		skip := trimmed == "" || strings.HasPrefix(trimmed, "#") || isBashKeywordOnly(trimmed)
		sl := wfSourceLine{LineNo: ln, Text: line, Skipped: skip}
		if !skip {
			sl.Operands = splitOperators(trimmed)

			// Detect multi-line single-quote opening.
			if opensMultiLineSingleQuote(trimmed) {
				inSingleQuote = true
			}

			// Detect heredoc opening.
			if delim := parseHeredocDelim(trimmed); delim != "" {
				inHeredoc = true
				heredocDelim = delim
			}
		}
		lines = append(lines, sl)
	}
	return lines
}

// opensMultiLineSingleQuote reports whether the line opens a single-quote
// that is not closed on the same line. Counts unescaped single-quotes;
// odd count means the quote spans to a subsequent line.
func opensMultiLineSingleQuote(s string) bool {
	count := 0
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++ // skip escaped char
			continue
		}
		if c == '"' && !inDouble {
			inDouble = true
			continue
		}
		if c == '"' && inDouble {
			inDouble = false
			continue
		}
		if c == '\'' && !inDouble {
			count++
		}
	}
	return count%2 == 1 // odd = unclosed
}

// closesMultiLineSingleQuote reports whether the line closes a pending
// multi-line single-quote. A line consisting of just `'` or ending with
// an unescaped `'` outside double-quotes closes it.
func closesMultiLineSingleQuote(s string) bool {
	// Count single-quotes on this continuation line. Odd = closes.
	count := 0
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if c == '"' {
			inDouble = !inDouble
			continue
		}
		if c == '\'' && !inDouble {
			count++
		}
	}
	return count%2 == 1
}

// parseHeredocDelim extracts the delimiter from a heredoc redirect (<<EOF,
// <<'EOF', <<"EOF", <<-EOF). Returns "" if no heredoc is found.
func parseHeredocDelim(s string) string {
	idx := strings.Index(s, "<<")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(s[idx+2:])
	if rest == "" {
		return ""
	}
	// Strip optional leading '-' (<<-EOF for tab-stripped heredocs).
	if rest[0] == '-' {
		rest = strings.TrimSpace(rest[1:])
	}
	if rest == "" {
		return ""
	}
	// Strip quotes around delimiter.
	if (rest[0] == '\'' || rest[0] == '"') && len(rest) >= 2 {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end >= 0 {
			return rest[1 : end+1]
		}
	}
	// Unquoted: take first word.
	end := strings.IndexAny(rest, " \t;|&)")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// isBashKeywordOnly reports whether the trimmed line consists entirely of
// bash control keywords that never fire the DEBUG trap.
func isBashKeywordOnly(s string) bool {
	switch s {
	case "done", "fi", "else", "then", "do", "esac", ";;":
		return true
	}
	// "else if", "then ..." would be commands; only exact matches qualify.
	return false
}

// parseStepBodiesRaw is the fallback used when ParseDSL fails (malformed DSL,
// or the input is from an old log format). Counts lines naively and uses the
// same skip rules.
func parseStepBodiesRaw(input string) [][]wfSourceLine {
	var bodies [][]wfSourceLine
	var current []wfSourceLine
	inStep := false
	scriptLineNo := 0

	flush := func() {
		bodies = append(bodies, current)
		current = nil
		scriptLineNo = 0
	}

	for _, line := range strings.Split(input, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "### ") {
			if inStep {
				flush()
			}
			inStep = true
			continue
		}
		if !inStep {
			continue
		}
		scriptLineNo++
		ln := scriptLineNo + trapLineOffset
		trimmed := strings.TrimSpace(line)
		skip := trimmed == "" ||
			strings.HasPrefix(trimmed, "#") ||
			(strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}"))
		sl := wfSourceLine{LineNo: ln, Text: line, Skipped: skip}
		if !skip {
			sl.Operands = splitOperators(trimmed)
		}
		current = append(current, sl)
	}
	if inStep {
		flush()
	}
	return bodies
}

// splitOperators splits one source line on shell pipeline/list operators,
// honoring single/double-quote regions and balanced grouping (`$(...)`,
// `${...}`, `(...)`, `[[ ... ]]`) so operators inside subshells, parameter
// expansions, or test brackets don't split.
//
// Returns operands in source order. Each operand carries the operator that
// preceded it (empty for the first).
func splitOperators(line string) []wfOperand {
	var operands []wfOperand
	var buf strings.Builder
	prevOp := ""

	emit := func() {
		s := strings.TrimSpace(buf.String())
		if s == "" && prevOp == "" && len(operands) == 0 {
			return
		}
		operands = append(operands, wfOperand{Op: prevOp, Cmd: s})
		buf.Reset()
	}

	i := 0
	inSingle := false
	inDouble := false
	parenDepth := 0   // tracks ( and $( — both add 1
	braceDepth := 0   // tracks ${
	bracketDepth := 0 // tracks [[ ... ]]
	for i < len(line) {
		c := line[i]
		// Quote tracking — single quotes have no escapes
		if !inDouble && c == '\'' {
			inSingle = !inSingle
			buf.WriteByte(c)
			i++
			continue
		}
		if !inSingle && c == '"' {
			inDouble = !inDouble
			buf.WriteByte(c)
			i++
			continue
		}
		if inSingle {
			buf.WriteByte(c)
			i++
			continue
		}
		if !inDouble && c == '\\' && i+1 < len(line) {
			buf.WriteByte(c)
			buf.WriteByte(line[i+1])
			i += 2
			continue
		}

		// Group tracking (only when not in single quotes; double-quote
		// strings can contain $(...) which still nests).
		if c == '$' && i+1 < len(line) && line[i+1] == '(' {
			parenDepth++
			buf.WriteByte(c)
			buf.WriteByte('(')
			i += 2
			continue
		}
		if c == '$' && i+1 < len(line) && line[i+1] == '{' {
			braceDepth++
			buf.WriteByte(c)
			buf.WriteByte('{')
			i += 2
			continue
		}
		if c == '(' && !inDouble {
			parenDepth++
			buf.WriteByte(c)
			i++
			continue
		}
		if c == ')' && parenDepth > 0 {
			parenDepth--
			buf.WriteByte(c)
			i++
			continue
		}
		if c == '}' && braceDepth > 0 {
			braceDepth--
			buf.WriteByte(c)
			i++
			continue
		}
		if c == '[' && i+1 < len(line) && line[i+1] == '[' && !inDouble {
			bracketDepth++
			buf.WriteByte('[')
			buf.WriteByte('[')
			i += 2
			continue
		}
		if c == ']' && i+1 < len(line) && line[i+1] == ']' && bracketDepth > 0 {
			bracketDepth--
			buf.WriteByte(']')
			buf.WriteByte(']')
			i += 2
			continue
		}

		// Inside any group or quote — no splitting
		if inDouble || parenDepth > 0 || braceDepth > 0 || bracketDepth > 0 {
			buf.WriteByte(c)
			i++
			continue
		}

		// Operator detection — order matters (longer first).
		if i+1 < len(line) && line[i] == '&' && line[i+1] == '&' {
			emit()
			prevOp = "&&"
			i += 2
			continue
		}
		if i+1 < len(line) && line[i] == '|' && line[i+1] == '|' {
			emit()
			prevOp = "||"
			i += 2
			continue
		}
		if i+1 < len(line) && line[i] == '|' && line[i+1] == '&' {
			emit()
			prevOp = "|&"
			i += 2
			continue
		}
		if c == '|' {
			emit()
			prevOp = "|"
			i++
			continue
		}
		if c == ';' {
			emit()
			prevOp = ";"
			i++
			continue
		}
		buf.WriteByte(c)
		i++
	}
	emit()
	return operands
}

// matchSubToOperand picks the operand whose text best matches the BASH_COMMAND
// reported by the DEBUG trap. Returns -1 if no match.
//
// Strategy: normalize whitespace on both sides, then check if any operand has
// the cmd as a prefix (after normalization). Bash sometimes adds spaces around
// redirects (`2>` becomes `2> ` in BASH_COMMAND), so prefix-match after
// normalization is more robust than equality.
func matchSubToOperand(operands []wfOperand, cmd string) int {
	nc := normalizeCmd(cmd)
	for i, op := range operands {
		if strings.HasPrefix(normalizeCmd(op.Cmd), nc) {
			return i
		}
	}
	// Fallback: substring match
	for i, op := range operands {
		if strings.Contains(normalizeCmd(op.Cmd), nc) {
			return i
		}
	}
	return -1
}

var (
	wsRe            = regexp.MustCompile(`\s+`)
	redirectSpaceRe = regexp.MustCompile(`([<>]+)\s+`) // bash reformats `2>/dev/null` → `2> /dev/null`
)

// normalizeCmd produces a canonical form so source-line operands match
// BASH_COMMAND values reported by the DEBUG trap. Handles whitespace and
// bash's redirect-spacing quirks.
func normalizeCmd(s string) string {
	s = wsRe.ReplaceAllString(strings.TrimSpace(s), " ")
	s = redirectSpaceRe.ReplaceAllString(s, "$1")
	return s
}

// ── Build waterfall from active call ───────────────────────────────────

// buildLiveWaterfall returns the incrementally-maintained waterfall
// projection built by Apply(). Zero-cost at render time.
func buildLiveWaterfall(a *activeCall) []waterfallStep {
	if a == nil {
		return nil
	}
	return a.waterfall
}

func findLineByLineNo(lines []wfSourceLine, lineNo int) int {
	if lineNo == 0 {
		return -1
	}
	for i := range lines {
		if lines[i].LineNo == lineNo {
			return i
		}
	}
	return -1
}

func firstNonSkipped(lines []wfSourceLine) int {
	for i := range lines {
		if !lines[i].Skipped {
			return i
		}
	}
	return -1
}

// ── Build waterfall from event file (completed call) ───────────────────

// loadStaticWaterfall replays the on-disk per-call event file through the
// same activeCall machinery, producing the exact same waterfall the live
// view would have shown. Returns the call's start time and step list.
func loadStaticWaterfall(callID string) (time.Time, []waterfallStep, error) {
	path := mcp.CallEventsPath(callID)
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer f.Close()

	a := &activeCall{ID: callID, CurrentStep: -1, unlimitedTail: true}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		ev, err := mcp.ParseLiveEvent(sc.Bytes())
		if err != nil {
			continue
		}
		// Apply uses the shared reducer in log_dashboard_live.go, but it
		// expects step-start to populate a.StepStatuses. activeCall.Apply
		// reads ev.Steps for that — re-use it directly.
		a.Apply(ev)
	}

	steps := buildLiveWaterfall(a)
	return a.StartedAt, steps, sc.Err()
}

// ── Timer formatting ───────────────────────────────────────────────────

func formatCompactDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
}

func formatPreciseDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := d.Seconds()
	if sec < 60 {
		return fmt.Sprintf("%.2fs", sec)
	}
	totalSec := int(sec)
	rem := sec - float64(totalSec)
	m := totalSec / 60
	s := float64(totalSec%60) + rem
	if m < 60 {
		return fmt.Sprintf("%dm%.2fs", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh%dm%.2fs", h, m, s)
}
