// Package interp is the correctness spine of the mvdan/sh execution upgrade.
//
// It parses a step body once (daemon-local, before any ssh), decides statically
// whether the body is safe to run under the mvdan/sh interpreter or must fall
// back to real bash (the "legacy path"), and proves that decision with a
// bash-differential test corpus (see differential.go + differential_test.go).
//
// The gap catalog encoded here is pinned to mvdan.cc/sh/v3 v3.13.1. It was
// verified against the interpreter's dispatch switch (interp/builtin.go) and the
// syntax AST. A version bump invalidates the catalog and MUST be re-validated by
// re-running the differential corpus — see plan decision #6.
package interp

import (
	"bytes"
	"errors"
	"fmt"

	"mvdan.cc/sh/v3/syntax"
)

// Route names the two runtime execution paths. These are the canonical route
// names from plan §8: "realbash" is the legacy real-bash + trap-tracing path.
type Route string

const (
	// RouteInterp runs the body under the in-process mvdan/sh interpreter.
	RouteInterp Route = "interp"
	// RouteRealbash falls back to the legacy real-bash path.
	RouteRealbash Route = "realbash"
)

// Verdict is the routing decision for one step body.
type Verdict struct {
	Route  Route
	Reason string
}

// PreflightError is a positioned teaching error produced when a step body fails
// to parse. It refuses the step BEFORE any ssh connection is attempted, so the
// user sees a line/col diagnostic instead of a remote shell error.
type PreflightError struct {
	Line, Col  uint
	Text       string
	Incomplete bool
}

func (e *PreflightError) Error() string {
	hint := ""
	if e.Incomplete {
		hint = " (unterminated construct — check quotes/heredocs/parens)"
	}
	return fmt.Sprintf("shell syntax error at line %d, col %d: %s%s — the step body was refused before connecting; fix the syntax and retry",
		e.Line, e.Col, e.Text, hint)
}

// NewParser builds the parser used everywhere in this package. Pinned options
// per plan §2: bash dialect, comments kept (so scaffolding never strips them).
func NewParser() *syntax.Parser {
	return syntax.NewParser(syntax.KeepComments(true), syntax.Variant(syntax.LangBash))
}

// Preflight parses body and decides the execution route.
//
//   - Unrecoverable syntax error (syntax.ParseError) -> (zero Verdict, *PreflightError):
//     the caller refuses the step before connecting.
//   - Valid-but-wrong-dialect construct (syntax.LangError) -> route to realbash
//     with a note, nil error: the legacy path can still run it.
//   - Otherwise the AST is screened for gap constructs (gaps.go). A gap routes to
//     realbash with a human reason; a clean body routes to interp.
func Preflight(body []byte) (Verdict, error) {
	file, err := NewParser().Parse(bytes.NewReader(body), "")
	if err != nil {
		// A LangError is not a hard refusal: the construct is valid shell, just
		// not in the bash variant. Real bash can run it.
		var le syntax.LangError
		if errors.As(err, &le) {
			return Verdict{
				Route:  RouteRealbash,
				Reason: fmt.Sprintf("non-bash dialect feature (%s) at line %d — running under real bash", le.Feature, le.Pos.Line()),
			}, nil
		}
		var pe syntax.ParseError
		if errors.As(err, &pe) {
			return Verdict{}, &PreflightError{
				Line:       pe.Pos.Line(),
				Col:        pe.Pos.Col(),
				Text:       pe.Text,
				Incomplete: pe.Incomplete,
			}
		}
		// Unknown parser error: refuse conservatively with what we have.
		return Verdict{}, &PreflightError{Text: err.Error()}
	}
	return detectGaps(file), nil
}
