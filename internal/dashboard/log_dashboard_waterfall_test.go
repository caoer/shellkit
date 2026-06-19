package dashboard

import (
	"strings"
	"testing"
	"time"
)

func TestSplitOperators_Basic(t *testing.T) {
	cases := []struct {
		in   string
		want []wfOperand
	}{
		{
			in: "ps aux | grep nixos | grep -v grep || echo none",
			want: []wfOperand{
				{Op: "", Cmd: "ps aux"},
				{Op: "|", Cmd: "grep nixos"},
				{Op: "|", Cmd: "grep -v grep"},
				{Op: "||", Cmd: "echo none"},
			},
		},
		{
			in: `kill 3474909 2>/dev/null && echo "killed" || echo "not found"`,
			want: []wfOperand{
				{Op: "", Cmd: "kill 3474909 2>/dev/null"},
				{Op: "&&", Cmd: `echo "killed"`},
				{Op: "||", Cmd: `echo "not found"`},
			},
		},
		{
			// Quoted | should not split.
			in: `echo "a|b"; echo done`,
			want: []wfOperand{
				{Op: "", Cmd: `echo "a|b"`},
				{Op: ";", Cmd: "echo done"},
			},
		},
		{
			in:   "echo single",
			want: []wfOperand{{Op: "", Cmd: "echo single"}},
		},
	}
	for _, c := range cases {
		got := splitOperators(c.in)
		if len(got) != len(c.want) {
			t.Errorf("split %q: want %d operands, got %d: %+v", c.in, len(c.want), len(got), got)
			continue
		}
		for i, op := range got {
			if op.Op != c.want[i].Op || op.Cmd != c.want[i].Cmd {
				t.Errorf("split %q [%d]: want %+v, got %+v", c.in, i, c.want[i], op)
			}
		}
	}
}

// parseStepBodies should return bodies that match what bash sees: JSON config
// and leading blanks stripped (DSL parser does this), and the first user line
// of the body is at script LINENO 2 (one trap line above).
func TestParseStepBodies_LineNoMapping(t *testing.T) {
	input := "### step-1\n" +
		"{\"local\": true}\n" +
		"\n" +
		"echo first\n" +
		"echo second\n" +
		"ps aux | grep foo\n" +
		"\n" +
		"### step-2\n" +
		"{\"local\": true}\n" +
		"sleep 1\n"
	bodies := parseStepBodies(input)
	if len(bodies) != 2 {
		t.Fatalf("want 2 step bodies, got %d", len(bodies))
	}

	step1 := bodies[0]
	// Step 1 body (post-DSL-strip): "echo first", "echo second", "ps aux | grep foo"
	// LINENOs: 2, 3, 4 (after the trap on line 1).
	want := []struct {
		ln   int
		text string
		skip bool
	}{
		{2, "echo first", false},
		{3, "echo second", false},
		{4, "ps aux | grep foo", false},
	}
	if len(step1) != len(want) {
		t.Fatalf("step1: want %d lines, got %d (%+v)", len(want), len(step1), step1)
	}
	for i, w := range want {
		if step1[i].LineNo != w.ln {
			t.Errorf("step1[%d].LineNo = %d, want %d", i, step1[i].LineNo, w.ln)
		}
		if strings.TrimSpace(step1[i].Text) != w.text {
			t.Errorf("step1[%d].Text = %q, want %q", i, step1[i].Text, w.text)
		}
		if step1[i].Skipped != w.skip {
			t.Errorf("step1[%d].Skipped = %v, want %v", i, step1[i].Skipped, w.skip)
		}
	}

	// Step 2: per-step bash invocation → LINENO scope resets to 2.
	step2 := bodies[1]
	if len(step2) < 1 {
		t.Fatalf("step2: want >=1 line, got %d", len(step2))
	}
	if step2[0].LineNo != 2 {
		t.Errorf("step2[0].LineNo = %d, want 2 (per-step LINENO scope)", step2[0].LineNo)
	}
}

func TestMatchSubToOperand(t *testing.T) {
	ops := []wfOperand{
		{Op: "", Cmd: "ps aux"},
		{Op: "|", Cmd: "grep nixos"},
		{Op: "||", Cmd: `echo "none"`},
	}
	cases := []struct {
		cmd  string
		want int
	}{
		{"ps aux", 0},
		{"grep nixos", 1},
		// Bash sometimes reformats: `2> /dev/null` instead of `2>/dev/null`.
		// Our test case uses unmodified strings; verify exact-match path.
		{`echo "none"`, 2},
		{"unrelated cmd", -1},
	}
	for _, c := range cases {
		got := matchSubToOperand(ops, c.cmd)
		if got != c.want {
			t.Errorf("match %q: want %d, got %d", c.cmd, c.want, got)
		}
	}
}

func TestFormatTimers(t *testing.T) {
	cases := []struct {
		startMs, elapsedMs int64
		done               bool
		want               string
	}{
		{0, 120, false, "0s(+0s)"},
		{0, 120, true, "0.00s(+0.12s)"},
		{73 * 1000, 2 * 1000, false, "1m13s(+2s)"},
		{73330, 10450, true, "1m13.33s(+10.45s)"},
		{3661 * 1000, 0, false, "1h1m(+0s)"},
	}
	for _, c := range cases {
		startD := time.Duration(c.startMs) * time.Millisecond
		elapsedD := time.Duration(c.elapsedMs) * time.Millisecond
		var got string
		if c.done {
			got = formatPreciseDur(startD) + "(+" + formatPreciseDur(elapsedD) + ")"
		} else {
			got = formatCompactDur(startD) + "(+" + formatCompactDur(elapsedD) + ")"
		}
		if got != c.want {
			t.Errorf("timer(start=%dms elapsed=%dms done=%v): want %q got %q",
				c.startMs, c.elapsedMs, c.done, c.want, got)
		}
	}
}
