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
