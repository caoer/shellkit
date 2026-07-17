package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/rundaemon"
)

// TestRunnerDefaultOn_DefaultOnPosture pins the post-flip default posture: the
// U9 differential gate went green and ZT authorized the flip on 2026-07-17
// (decisions/runner-default-on-flip.md), so the runner is DEFAULT-ON. An absent
// interp key engages the runner; `"interp": false` remains the forced-legacy
// escape hatch. Reverting to opt-in is the same one-line flip point.
func TestRunnerDefaultOn_DefaultOnPosture(t *testing.T) {
	if !runnerDefaultOn {
		t.Fatal("runnerDefaultOn must be true — the U9 gate is green and the flip was authorized (decisions/runner-default-on-flip.md); flipping back to opt-in requires a decision, not drift")
	}

	trueVal := true
	falseVal := false
	cases := []struct {
		name string
		cfg  StepConfig
		want bool
	}{
		{"absent -> default posture (on)", StepConfig{}, true},
		{"interp:false -> forced legacy", StepConfig{Interp: &falseVal}, false},
		{"interp:true -> runner engaged", StepConfig{Interp: &trueVal}, true},
	}
	for _, c := range cases {
		if got := runnerOptIn(c.cfg); got != c.want {
			t.Errorf("%s: runnerOptIn = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExecuteSSH_RefusesInvalidBashPreConnect proves the runner path refuses a
// syntactically-invalid body BEFORE any ssh connection: interp.Preflight returns
// a positioned PreflightError, executeSSH surfaces it, and the host (a bogus raw
// target that would otherwise fail with a connection error) is never dialed.
func TestExecuteSSH_RefusesInvalidBashPreConnect(t *testing.T) {
	trueVal := true
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	steps := []Step{{
		Name:   "bad-syntax",
		Action: ActionSSH,
		Hosts:  []string{"root@nonexistent.invalid"},
		Config: StepConfig{Interp: &trueVal},
		Body:   "echo \"unterminated\ncat foo\n",
	}}

	results, err := exec.Execute(context.Background(), steps)
	if err != nil {
		t.Fatalf("Execute returned a hard error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.ExitCode == 0 {
		t.Errorf("refused step should not exit 0, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Error, "syntax error") {
		t.Errorf("error should be a positioned syntax error, got %q", r.Error)
	}
	if !strings.Contains(r.Error, "refused before connecting") {
		t.Errorf("error should say it refused pre-connect, got %q", r.Error)
	}
}

// TestFormatResults_LegacyPathByteInvariance is the hard-constraint-#1 gate: a
// result with RunnerPath=false and RouteNote="" (a step the runner never engaged)
// renders byte-for-byte as it did before U6b — no note: line, legacy whole-second
// +Ns trace arithmetic, unchanged section order.
func TestFormatResults_LegacyPathByteInvariance(t *testing.T) {
	store, _ := NewOutputStore(nil)
	r := StepResult{
		Name:      "x",
		Host:      "h1",
		ExitCode:  0,
		Stdout:    "hello world",
		Outputs:   map[string]string{"k": "v"},
		FilePath:  "/tmp/t/x.out",
		ShowTrace: true,
		Trace: []TraceLine{
			{ElapsedSec: 0, Command: "echo a"},
			{ElapsedSec: 2, Command: "echo b"},
		},
		// RunnerPath: false, RouteNote: "" — the legacy path, untouched.
	}

	want := "=== x [h1] exit:0 ===\n" +
		"output: /tmp/t/x.out (1 lines)\n" +
		"  k=v\n" +
		"command trace:\n" +
		"  +0s  echo a (2s)\n" +
		"  +2s  echo b\n" +
		"preview:\n" +
		"  hello world\n" +
		"\n"

	got := formatResults([]StepResult{r}, store)
	if got != want {
		t.Errorf("legacy render drifted from the frozen bytes.\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(got, "note:") {
		t.Error("legacy path (empty RouteNote) must not emit a note: line")
	}
}

// TestFormatResults_RunnerPathTrace covers the runner-path render contract (U0
// §1.1/§1.2/§2): a route-provenance note: line, ns-precision +offset / (duration)
// trace, and an inline exit:N only for a non-zero per-command exit.
func TestFormatResults_RunnerPathTrace(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exit1 := 1
	r := StepResult{
		Name:       "deploy",
		Host:       "lazybox",
		ExitCode:   0,
		RunnerPath: true,
		RouteNote:  "runner bootstrap failed on lazybox (noexec ~/.cache) — ran under legacy path",
		ShowTrace:  true,
		Trace: []TraceLine{
			{ElapsedNS: 8000, DurationNS: 8000, Command: "cd /srv/app"},
			{ElapsedNS: 20000, DurationNS: 842100000, Command: "git pull --ff-only", Exit: &exit1},
		},
	}

	got := formatResults([]StepResult{r}, store)

	for _, want := range []string{
		"note: runner bootstrap failed on lazybox",
		"command trace:",
		"  +8.0µs  cd /srv/app (8.0µs)",
		"  +20.0µs  git pull --ff-only exit:1 (842.1ms)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runner render missing %q\n--- got ---\n%s", want, got)
		}
	}
	// The runner path must not fall through to the legacy whole-second renderer.
	if strings.Contains(got, "+0s") {
		t.Errorf("runner path rendered a legacy +Ns line:\n%s", got)
	}
}

// TestAdaptRunnerTrace verifies the rundaemon.TraceLine -> mcp.TraceLine bridge
// carries the ns timing fields and the per-command exit pointer, leaving the
// legacy ints zero.
func TestAdaptRunnerTrace(t *testing.T) {
	if got := adaptRunnerTrace(nil); got != nil {
		t.Errorf("empty trace should map to nil, got %v", got)
	}

	exit := 3
	in := []rundaemon.TraceLine{{
		Seq:        7,
		ElapsedNS:  5,
		DurationNS: 6,
		Command:    "systemctl is-active nginx",
		Exit:       &exit,
	}}
	out := adaptRunnerTrace(in)
	if len(out) != 1 {
		t.Fatalf("want 1 line, got %d", len(out))
	}
	g := out[0]
	if g.ElapsedNS != 5 || g.DurationNS != 6 || g.Command != "systemctl is-active nginx" {
		t.Errorf("ns fields not carried: %+v", g)
	}
	if g.Exit == nil || *g.Exit != 3 {
		t.Errorf("exit pointer not carried: %+v", g.Exit)
	}
	if g.ElapsedSec != 0 || g.LineNo != 0 {
		t.Errorf("legacy ints should stay zero, got ElapsedSec=%d LineNo=%d", g.ElapsedSec, g.LineNo)
	}
}
