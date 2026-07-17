package interp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// expectation is derived from a corpus script's filename prefix.
type expectation int

const (
	// expInterpMatch: clean & FIXED-form scripts — MUST route interp AND match
	// bash on every axis. A divergence here is a hole in the screener.
	expInterpMatch expectation = iota
	// expRealbash: class-A/B gaps and the screened divergence classes (A/B/D/E/F)
	// — MUST route realbash; divergence under interp is expected (that's WHY they
	// route to bash) and only recorded, not asserted.
	expRealbash
	// expResidual: the non-screenable residual classes (C/G, plus D's concurrency
	// edge) — route interp, divergence is MEASURED and DOCUMENTED (U10), never
	// build-failing (plan decision #16).
	expResidual
)

func categoryOf(name string) expectation {
	switch {
	case strings.HasPrefix(name, "residual"):
		return expResidual
	case strings.HasPrefix(name, "gap"), strings.HasPrefix(name, "div"):
		return expRealbash
	default: // clean_, neg_
		return expInterpMatch
	}
}

func newSandbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sb := filepath.Join(root, "sb")
	if err := os.Mkdir(sb, 0o755); err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestDifferentialCorpus is the U9 go/no-go gate. For every corpus script it runs
// the body through BOTH real bash (subprocess) and the in-process interpreter, in
// a fresh temp cwd each, and diffs stdout/stderr/exit/cwd/env/files (+ recovered
// panic). See differential.go for the decision rule.
func TestDifferentialCorpus(t *testing.T) {
	if _, err := execLookBash(); err != nil {
		t.Fatalf("real bash is required on PATH for the differential corpus: %v", err)
	}
	files, err := filepath.Glob("testdata/gapcorpus/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no corpus scripts found under testdata/gapcorpus")
	}
	sort.Strings(files)

	var nMatch, nRealbash, nResidual int
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}

			v, perr := Preflight(body)
			if perr != nil {
				t.Fatalf("corpus script did not parse: %v", perr)
			}

			b := RunBash(body, newSandbox(t))
			i := RunInterp(body, newSandbox(t))
			axes := DiffAxes(b, i)

			switch categoryOf(name) {
			case expInterpMatch:
				nMatch++
				if v.Route != RouteInterp {
					t.Fatalf("expected route interp, got %q (reason: %s)", v.Route, v.Reason)
				}
				if len(axes) > 0 {
					t.Fatalf("SCREENER HOLE: %s routed to interp but diverges from bash on %v\n  %s",
						name, axes, strings.Join(details(axes, b, i), "\n  "))
				}

			case expRealbash:
				nRealbash++
				if v.Route != RouteRealbash {
					t.Fatalf("SCREENER MISS: %s must route to realbash, got %q — interp divergence axes=%v",
						name, v.Route, axes)
				}
				t.Logf("screened -> realbash: %s | interp divergence axes=%v", v.Reason, axes)

			case expResidual:
				nResidual++
				if v.Route != RouteInterp {
					t.Fatalf("residual class %s expected route interp (non-screenable), got %q", name, v.Route)
				}
				// Accepted, documented in U10 — measured, never build-failing.
				t.Logf("RESIDUAL (accepted/documented): %s interp divergence axes=%v", name, axes)
			}
		})
	}
	t.Logf("corpus size: %d scripts — %d interp-match (strict), %d screened-realbash, %d residual",
		len(files), nMatch, nRealbash, nResidual)
}

func details(axes []string, b, i RunResult) []string {
	out := make([]string, 0, len(axes))
	for _, ax := range axes {
		out = append(out, axisDetail(ax, b, i))
	}
	return out
}

// baseResult is a fully-known, non-diverging RunResult used as the identical
// baseline for the DiffAxes oracle: mutating exactly one field must surface
// exactly that axis, and an unmodified copy must surface none.
func baseResult() RunResult {
	return RunResult{
		Stdout:   "out",
		Stderr:   "err",
		Exit:     0,
		Cwd:      ".",
		CwdKnown: true,
		Env:      map[string]string{"K": "v"},
		EnvKnown: true,
		Files:    map[string]string{"f": "hash"},
		Panicked: false,
	}
}

// TestDiffAxesOracle is the positive oracle for DiffAxes (review finding #10):
// the go/no-go gate for the held default-on flip. The corpus test only proves
// DiffAxes returns EMPTY on interp-match scripts; if DiffAxes regressed to
// always-empty, every screener hole would pass silently. This test proves the
// opposite direction: each axis MUST fire when the two runs differ on exactly
// that axis, and an identical pair MUST yield no axes.
func TestDiffAxesOracle(t *testing.T) {
	// Identical pair: no axis may fire.
	if axes := DiffAxes(baseResult(), baseResult()); len(axes) != 0 {
		t.Fatalf("DiffAxes on identical results returned %v, want none", axes)
	}

	cases := []struct {
		axis   string
		mutate func(r *RunResult) // applied to the interp side only
	}{
		{"stdout", func(r *RunResult) { r.Stdout = "different" }},
		{"stderr", func(r *RunResult) { r.Stderr = "different" }},
		{"exit", func(r *RunResult) { r.Exit = 3 }},
		{"cwd", func(r *RunResult) { r.Cwd = "sub" }},
		{"env", func(r *RunResult) { r.Env = map[string]string{"K": "changed"} }},
		{"files", func(r *RunResult) { r.Files = map[string]string{"f": "other"} }},
		{"panic", func(r *RunResult) { r.Panicked = true }},
	}
	for _, c := range cases {
		t.Run(c.axis, func(t *testing.T) {
			b := baseResult()
			i := baseResult()
			c.mutate(&i)
			axes := DiffAxes(b, i)
			if len(axes) != 1 || axes[0] != c.axis {
				t.Fatalf("mutating %s: DiffAxes = %v, want exactly [%s]", c.axis, axes, c.axis)
			}
		})
	}
}

// TestDiffAxesGatedByKnown proves the cwd/env axes are correctly SUPPRESSED when
// a run could not probe them (CwdKnown/EnvKnown false) — otherwise a script that
// exits/execs before the bash probe would report a spurious divergence and mask
// or invent screener holes. Files/exit/stdout/stderr are always compared.
func TestDiffAxesGatedByKnown(t *testing.T) {
	// cwd differs but one side is unknown -> not reported.
	b := baseResult()
	i := baseResult()
	i.Cwd = "elsewhere"
	i.CwdKnown = false
	if axes := DiffAxes(b, i); len(axes) != 0 {
		t.Fatalf("cwd with CwdKnown=false must be suppressed, got %v", axes)
	}
	// env differs but one side is unknown -> not reported.
	b = baseResult()
	i = baseResult()
	i.Env = map[string]string{"K": "x"}
	i.EnvKnown = false
	if axes := DiffAxes(b, i); len(axes) != 0 {
		t.Fatalf("env with EnvKnown=false must be suppressed, got %v", axes)
	}
}
