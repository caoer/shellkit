package mcp

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGenerateInterpSyntax(t *testing.T) {
	script := "#!/bin/bash\n" + GenerateInterp("test-session", "abc123", "SHELLKIT_VERBS_test")
	f, err := os.CreateTemp("", "interp-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cmd := exec.Command("bash", "-n", f.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

func TestGenerateInterpDeterminism(t *testing.T) {
	a := GenerateInterp("my-session", "nonce1", "SHELLKIT_VERBS_test")
	b := GenerateInterp("my-session", "nonce1", "SHELLKIT_VERBS_test")
	if a != b {
		t.Fatal("non-deterministic output")
	}
}

func TestGenerateInterpSessionEscape(t *testing.T) {
	cases := []struct {
		name    string
		session string
		want    string
	}{
		{"single quotes", "it's", `SESS='it'"'"'s'`},
		{"double quotes", `say "hi"`, `SESS='say "hi"'`},
		{"spaces", "my session", `SESS='my session'`},
		{"dollar", "cost$100", `SESS='cost$100'`},
		{"backticks", "run `cmd`", "SESS='run `cmd`'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := GenerateInterp(tc.session, "n", "SHELLKIT_VERBS_test")
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q in output, got:\n%s", tc.want, out)
			}
		})
	}
}

func TestGenerateInterpEmptyStream(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	if !strings.Contains(script, "while IFS= read -r line; do") {
		t.Fatal("missing dispatch loop")
	}
}

func TestGenerateInterpNoBash5(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	bash5 := []string{"${var@Q}", "${var@a}", "wait -p"}
	for _, feat := range bash5 {
		if strings.Contains(script, feat) {
			t.Errorf("found bash 5+ feature: %s", feat)
		}
	}
}

func TestGenerateInterpExitTrap(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	if !strings.Contains(script, "trap 'do_kill' EXIT") {
		t.Fatal("missing EXIT trap for do_kill")
	}
}

func TestGenerateInterpExactMatch(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	bare := strings.Count(script, `-t "$SESS"`)
	exact := strings.Count(script, `-t "=$SESS"`)
	if bare != 0 {
		t.Fatalf("found %d bare -t \"$SESS\" without = prefix", bare)
	}
	if exact < 5 {
		t.Fatalf("expected at least 5 exact-match -t \"=$SESS\", got %d", exact)
	}
}

func TestGenerateInterpOutputRef(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	if !strings.Contains(script, `OUTPUT_FILE="$OUTPUT"`) {
		t.Fatal("missing OUTPUT_FILE reference to $OUTPUT env var")
	}
}

func TestBuildTmuxBodyContract(t *testing.T) {
	delim := "SHELLKIT_VERBS_test1234"
	interp := GenerateInterp("test-session", "test1234nonce", delim)
	if !strings.HasSuffix(interp, "done <<'"+delim+"'\n") {
		t.Errorf("GenerateInterp must end with heredoc redirect, got trailing: %q",
			interp[len(interp)-40:])
	}
}

func TestDoKeyQuoting(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	if strings.Contains(script, `send-keys -t "=$SESS" $1`) {
		t.Fatal("do_key still uses bare $1 — must use quoted \"$@\" loop")
	}
	if !strings.Contains(script, `"$@"`) {
		t.Fatal("do_key missing quoted \"$@\"")
	}
	if !strings.Contains(script, `send-keys -t "=$SESS" -- "$k"`) {
		t.Fatal("do_key missing -- guard and quoted $k")
	}
}

func TestExpectScrollbackBound(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	if strings.Contains(script, "-S - |") || strings.Contains(script, "-S - 2>") {
		t.Fatal("do_expect still uses unbounded -S - scrollback capture")
	}
	if !strings.Contains(script, "-S -200") {
		t.Fatal("do_expect must use -S -200 for bounded scrollback")
	}
}

func TestBase64Check(t *testing.T) {
	script := GenerateInterp("test", "n", "SHELLKIT_VERBS_test")
	checkIdx := strings.Index(script, "base64 -w0 >/dev/null")
	if checkIdx == -1 {
		t.Fatal("missing base64 -w0 compatibility check")
	}
	funcIdx := strings.Index(script, "decode_b64()")
	if funcIdx == -1 {
		t.Fatal("missing decode_b64 function")
	}
	if checkIdx > funcIdx {
		t.Fatal("base64 check must appear before function definitions")
	}
	if !strings.Contains(script, "FATAL: base64 -w0 not supported") {
		t.Fatal("missing FATAL error message in base64 check")
	}
}

func TestBuildTmuxBodyNonceDelimiter(t *testing.T) {
	body1, _ := buildTmuxBody("s1", "nonce-aaa", []TmuxVerb{{Wire: "kill"}})
	body2, _ := buildTmuxBody("s1", "nonce-bbb", []TmuxVerb{{Wire: "kill"}})
	if strings.Contains(body1, "nonce-bb") || strings.Contains(body2, "nonce-aa") {
		t.Error("delimiter should be nonce-specific")
	}
}
