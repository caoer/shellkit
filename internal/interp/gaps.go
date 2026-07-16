package interp

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// detectGaps walks the parsed body and returns a routing Verdict. It runs two
// detector classes (plan U1):
//
//	Class A — LOUD gaps: constructs the interpreter refuses at runtime with an
//	          exit-2 error ("unsupported builtin", "invalid signal specification",
//	          …). Routing to bash turns a loud remote failure into a silent,
//	          correct fallback.
//	Class B — SILENT-divergence idioms (decision #16b): constructs that run exit-0
//	          under interp but produce WRONG results (cwd/env leak, discarded
//	          output, synthetic $!). They are invisible at runtime, so they must
//	          be screened statically.
//
// When several gaps match, the lowest-priority-number reason wins (priorities
// below). Any match routes to realbash; a clean body routes to interp.
func detectGaps(file *syntax.File) Verdict {
	var findings []finding

	// Whole-body predicates for the combination screens (class B).
	var hasTrapExit, hasExecCmd, hasIFSAssign, hasForWordIter bool

	// Track nested pipe stages so a pipeline's last stage is screened exactly once.
	coveredPipes := map[*syntax.BinaryCmd]bool{}

	add := func(prio int, format string, args ...any) {
		findings = append(findings, finding{prio, fmt.Sprintf(format, args...)})
	}

	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CallExpr:
			inspectCall(x, add)
			if isTrapWithSignal(x, "EXIT") {
				hasTrapExit = true
			}
			if isExecWithCommand(x) {
				hasExecCmd = true
			}
			if callAssignsIFS(x) {
				hasIFSAssign = true
			}

		case *syntax.DeclClause:
			if x.Variant != nil && x.Variant.Value == "export" {
				// export FOO=... as a keyword works under interp; only the
				// assignment target matters for the IFS screen.
			}
			if declAssignsIFS(x) {
				hasIFSAssign = true
			}

		case *syntax.Stmt:
			// exec with a redirect and no command reconfigures shell fds; interp
			// diverges (output discarded). (class B)
			if isExecRedirectNoCommand(x) {
				add(prioExecRedir, "`exec` with a redirect and no command reconfigures shell file descriptors — interp diverges (output can be discarded); running under real bash")
			}

		case *syntax.BinaryCmd:
			if (x.Op == syntax.Pipe || x.Op == syntax.PipeAll) && !coveredPipes[x] {
				markNestedPipes(x, coveredPipes)
				if reason := pipelineLastStageMutating(x.Y); reason != "" {
					add(prioPipeMutate, "%s", reason)
				}
			}

		case *syntax.ForClause:
			if x.Select {
				// select is parsed but unimplemented under interp. (class A)
				add(prioSelect, "`select` is parsed but not implemented by interp — running under real bash")
			} else if _, ok := x.Loop.(*syntax.WordIter); ok {
				hasForWordIter = true
			}

		case *syntax.ParamExp:
			// $$ / $! in a word: $$ collides across concurrent runners and $! is
			// a synthetic "gN" under interp (useless as a real PID). (class B)
			if x.Param != nil && x.Short {
				switch x.Param.Value {
				case "$":
					add(prioProcParam, "`$$` (process id) is used in a word — interp's value collides across concurrent steps; running under real bash")
				case "!":
					add(prioProcParam, "`$!` (last background pid) is used in a word — interp emits a synthetic \"gN\" instead of a real pid; running under real bash")
				}
			}
		}
		return true
	})

	// Combination screens (class B), evaluated after the full walk.
	if hasTrapExit && hasExecCmd {
		add(prioTrapExec, "body combines `trap … EXIT` with `exec <cmd>` — under interp the exit trap fires after exec (bash replaces the process and never runs it); running under real bash")
	}
	if hasIFSAssign && hasForWordIter {
		add(prioIFSLoop, "body reassigns `IFS` and then word-splits in a loop — interp's empty-field handling diverges from bash; running under real bash")
	}

	if len(findings) == 0 {
		return Verdict{Route: RouteInterp, Reason: "no interpreter gaps detected"}
	}
	best := findings[0]
	for _, f := range findings[1:] {
		if f.prio < best.prio {
			best = f
		}
	}
	return Verdict{Route: RouteRealbash, Reason: best.reason}
}

type finding struct {
	prio   int
	reason string
}

// Priority bands: class A (loud gaps) 1-19, class B (silent idioms) 20-39.
// Lower number = reported first when multiple gaps match.
const (
	prioTrapSignal = 1
	prioKill       = 2
	prioUlimit     = 3
	prioJobControl = 4
	prioCommandDcl = 5
	prioPrintf     = 6
	prioRead       = 7
	prioWait       = 8
	prioShopt      = 9
	prioType       = 10
	prioSelect     = 11
	prioMapfile    = 12

	prioPipeMutate = 20
	prioExecRedir  = 21
	prioProcParam  = 22
	prioTrapExec   = 23
	prioIFSLoop    = 24
)

// jobControlBuiltins are recognized by IsBuiltin but have no dispatch case, so
// interp refuses them with "unsupported builtin" (exit 2). kill/ulimit are the
// same failure class but called out separately for their reason strings.
var jobControlBuiltins = map[string]bool{
	"jobs": true, "fg": true, "bg": true, "disown": true,
	"suspend": true, "fc": true, "newgrp": true,
}

// declKeywords dispatch at the AST level as declarations; `command <kw>` breaks
// because command re-dispatches through the builtin switch where they are absent.
var declKeywords = map[string]bool{
	"declare": true, "local": true, "export": true,
	"readonly": true, "typeset": true, "let": true,
}

// readSupportedFlags: interp implements only -s -r -a -p. Every other flag is
// "read: invalid option". (read -a is FIXED at v3.13.x — do NOT trigger on it.)
var readSupportedFlags = map[rune]bool{'s': true, 'r': true, 'a': true, 'p': true}

// shoptSupported is the full set interp can actually toggle. Every other name is
// recognized-but-unsupported (~45 names) and errors under `shopt -s`.
var shoptSupported = map[string]bool{
	"dotglob": true, "expand_aliases": true, "extglob": true,
	"globstar": true, "nocaseglob": true, "nullglob": true,
}

// pipelineMutatingBuiltins mutate shell state; as a pipeline's LAST stage they
// leak that state into the parent under interp (bash isolates them in a subshell).
var pipelineMutatingBuiltins = map[string]bool{
	"cd": true, "read": true, "set": true, "shopt": true, "export": true,
}

// inspectCall runs the class-A per-command detectors on a simple command.
func inspectCall(c *syntax.CallExpr, add func(int, string, ...any)) {
	if len(c.Args) == 0 {
		return
	}
	name := c.Args[0].Lit()
	rest := c.Args[1:]

	switch name {
	case "trap":
		flags, sigspecs := parseTrapArgs(rest)
		for _, f := range flags {
			if f == "-l" || f == "-p" {
				add(prioTrapSignal, "`trap %s` is not implemented by interp — running under real bash", f)
			}
		}
		for _, sig := range sigspecs {
			if sig != "EXIT" && sig != "ERR" {
				label := sig
				if label == "" {
					label = "<computed>"
				}
				add(prioTrapSignal, "`trap` uses signal %s — interp supports only EXIT and ERR — running under real bash", label)
			}
		}

	case "kill":
		add(prioKill, "`kill` is not implemented by interp (recognized builtin, no dispatch — exit 2); running under real bash")

	case "ulimit":
		add(prioUlimit, "`ulimit` is not implemented by interp; running under real bash")

	case "printf":
		if len(rest) > 0 {
			if verb := printfGapVerb(staticText(rest[0])); verb != "" {
				add(prioPrintf, "`printf` format uses %s — unsupported by interp; running under real bash", verb)
			}
		}

	case "read":
		for _, f := range flagLetters(rest) {
			if !readSupportedFlags[f] {
				add(prioRead, "`read -%c` is unsupported by interp (only -s -r -a -p); running under real bash", f)
			}
		}

	case "wait":
		for _, f := range flagLetters(rest) {
			if f == 'n' || f == 'p' {
				add(prioWait, "`wait -%c` is unsupported by interp; running under real bash", f)
			}
		}

	case "shopt":
		flags, names := splitFlagsAndOperands(rest)
		posixMode := false
		for _, f := range flags {
			switch f {
			case "-p", "-q":
				add(prioShopt, "`shopt %s` is unsupported by interp; running under real bash", f)
			case "-o":
				posixMode = true
			}
		}
		if !posixMode {
			for _, opt := range names {
				if opt != "" && !shoptSupported[opt] {
					add(prioShopt, "`shopt … %s` targets an option interp does not support; running under real bash", opt)
				}
			}
		}

	case "type":
		for _, f := range flagLetters(rest) {
			if f == 'a' || f == 'f' {
				add(prioType, "`type -%c` is not implemented by interp (only -p -P -t); running under real bash", f)
			}
		}

	case "mapfile", "readarray":
		for _, f := range flagLetters(rest) {
			if f != 't' && f != 'd' {
				add(prioMapfile, "`%s -%c` is unsupported by interp (only -t -d); running under real bash", name, f)
			}
		}

	case "command":
		if len(rest) > 0 && declKeywords[rest[0].Lit()] {
			add(prioCommandDcl, "`command %s` breaks under interp — declaration keywords are not reachable through `command`; running under real bash", rest[0].Lit())
		}

	default:
		if jobControlBuiltins[name] {
			add(prioJobControl, "job-control builtin `%s` is not implemented by interp; running under real bash", name)
		}
	}
}

// parseTrapArgs splits trap operands into flags and signal specs. Grammar:
// `trap [-lp] [[action] sigspec ...]`. A single non-flag operand is a sigspec
// (reset form); two or more means the first is the action and the rest sigspecs.
// The action word is normally quoted, so its Lit() is "" and it is never mistaken
// for a signal.
func parseTrapArgs(args []*syntax.Word) (flags, sigspecs []string) {
	var operands []*syntax.Word
	stopped := false
	for _, w := range args {
		lit := w.Lit()
		if !stopped && lit == "--" {
			stopped = true
			continue
		}
		if !stopped && len(lit) > 1 && lit[0] == '-' {
			flags = append(flags, lit)
			continue
		}
		operands = append(operands, w)
	}
	switch len(operands) {
	case 0:
		// bare `trap` (print) — no signals to screen.
	case 1:
		sigspecs = []string{operands[0].Lit()}
	default:
		for _, w := range operands[1:] {
			sigspecs = append(sigspecs, w.Lit())
		}
	}
	return flags, sigspecs
}

// isTrapWithSignal reports whether c is `trap … <sig>` naming the given signal.
func isTrapWithSignal(c *syntax.CallExpr, sig string) bool {
	if len(c.Args) == 0 || c.Args[0].Lit() != "trap" {
		return false
	}
	_, sigspecs := parseTrapArgs(c.Args[1:])
	for _, s := range sigspecs {
		if s == sig {
			return true
		}
	}
	return false
}

// isExecWithCommand reports whether c is `exec <cmd> …` (exec that, under bash,
// replaces the process image).
func isExecWithCommand(c *syntax.CallExpr) bool {
	return len(c.Args) >= 2 && c.Args[0].Lit() == "exec"
}

// isExecRedirectNoCommand reports whether stmt is `exec >file` (no command),
// which reconfigures the current shell's descriptors.
func isExecRedirectNoCommand(stmt *syntax.Stmt) bool {
	c, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return false
	}
	return len(c.Args) == 1 && c.Args[0].Lit() == "exec" && len(stmt.Redirs) > 0
}

func callAssignsIFS(c *syntax.CallExpr) bool {
	for _, a := range c.Assigns {
		if a.Name != nil && a.Name.Value == "IFS" {
			return true
		}
	}
	return false
}

func declAssignsIFS(d *syntax.DeclClause) bool {
	for _, a := range d.Args {
		if a.Name != nil && a.Name.Value == "IFS" {
			return true
		}
	}
	return false
}

// pipelineLastStageMutating returns a reason if the pipeline's last stage runs a
// state-mutating builtin or declaration.
func pipelineLastStageMutating(stage *syntax.Stmt) string {
	switch c := stage.Cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Args) > 0 {
			name := c.Args[0].Lit()
			if pipelineMutatingBuiltins[name] {
				return fmt.Sprintf("pipeline's final stage runs the state-mutating builtin `%s` — interp can leak its cwd/env into the parent (bash isolates it in a subshell); running under real bash", name)
			}
		}
	case *syntax.DeclClause:
		if c.Variant != nil {
			return fmt.Sprintf("pipeline's final stage is a `%s` declaration — interp can leak it into the parent; running under real bash", c.Variant.Value)
		}
	}
	return ""
}

// markNestedPipes walks the left spine of a pipe chain, marking every nested pipe
// BinaryCmd as covered so its (non-final) stage is not re-screened.
func markNestedPipes(top *syntax.BinaryCmd, covered map[*syntax.BinaryCmd]bool) {
	b := top
	for {
		inner, ok := b.X.Cmd.(*syntax.BinaryCmd)
		if !ok || (inner.Op != syntax.Pipe && inner.Op != syntax.PipeAll) {
			return
		}
		covered[inner] = true
		b = inner
	}
}

// flagLetters returns the individual flag letters from a word list, e.g.
// ["-rn", "5", "v"] -> ['r','n']. Words not starting with '-' (or "--") end
// flag scanning at that word but later flag-shaped words are still ignored as
// operands — matching how the builtins parse their own flags.
func flagLetters(args []*syntax.Word) []rune {
	var out []rune
	for _, w := range args {
		lit := w.Lit()
		if lit == "--" {
			break
		}
		if len(lit) < 2 || lit[0] != '-' {
			continue
		}
		for _, r := range lit[1:] {
			out = append(out, r)
		}
	}
	return out
}

// splitFlagsAndOperands separates -flag words from operand words.
func splitFlagsAndOperands(args []*syntax.Word) (flags, operands []string) {
	stopped := false
	for _, w := range args {
		lit := w.Lit()
		if !stopped && lit == "--" {
			stopped = true
			continue
		}
		if !stopped && len(lit) > 1 && lit[0] == '-' {
			flags = append(flags, lit)
			continue
		}
		operands = append(operands, lit)
	}
	return flags, operands
}

// staticText extracts the statically-known text of a word, including single- and
// double-quoted literal parts (unlike Word.Lit(), which only handles bare
// literals). It returns "" if any part is dynamic (parameter/command expansion),
// so dynamic printf formats are a documented static-scan blind spot rather than a
// false trigger.
func staticText(w *syntax.Word) string {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, dp := range p.Parts {
				lit, ok := dp.(*syntax.Lit)
				if !ok {
					return ""
				}
				sb.WriteString(lit.Value)
			}
		default:
			return ""
		}
	}
	return sb.String()
}

// printfGapVerb reports the first unsupported printf verb in a literal format
// string: %q, a %(fmt)T time spec, or a floating-point verb. Returns "" if the
// format is clean or non-literal (dynamic formats are a documented static-scan
// blind spot, not screened).
func printfGapVerb(format string) string {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i >= len(format) {
			break
		}
		if format[i] == '%' {
			continue // escaped %%
		}
		// %(fmt)T time specifier
		if format[i] == '(' {
			if end := strings.IndexByte(format[i:], ')'); end >= 0 {
				j := i + end + 1
				if j < len(format) && format[j] == 'T' {
					return "a `%(fmt)T` time specifier"
				}
			}
		}
		// skip flags/width/precision to the verb
		j := i
		for j < len(format) && strings.IndexByte("-+ #0123456789.*", format[j]) >= 0 {
			j++
		}
		if j >= len(format) {
			break
		}
		switch format[j] {
		case 'q':
			return "the `%q` quote specifier"
		case 'e', 'E', 'f', 'F', 'g', 'G', 'a', 'A':
			return fmt.Sprintf("the floating-point specifier `%%%c`", format[j])
		}
	}
	return ""
}
