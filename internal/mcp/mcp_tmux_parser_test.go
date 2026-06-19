package mcp

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseTmuxSpawn(t *testing.T) {
	verbs, err := ParseVerbScript(`spawn bash -c "echo hello"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 1 {
		t.Fatalf("expected 1 verb, got %d", len(verbs))
	}
	v := verbs[0]
	if v.Name != "spawn" {
		t.Fatalf("expected spawn, got %s", v.Name)
	}
	payload := strings.Join([]string{"bash", "-c", "echo hello"}, "\x00")
	want := "spawn_b64 " + base64.StdEncoding.EncodeToString([]byte(payload))
	if v.Wire != want {
		t.Fatalf("wire mismatch:\n  got:  %s\n  want: %s", v.Wire, want)
	}
}

func TestParseTmuxSend(t *testing.T) {
	verbs, err := ParseVerbScript(`send "hello world\r"`)
	if err != nil {
		t.Fatal(err)
	}
	v := verbs[0]
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(v.Wire, "send_b64 "))
	if string(decoded) != "hello world\r" {
		t.Fatalf("decoded mismatch: got %q", decoded)
	}
}

func TestParseTmuxSendEscapes(t *testing.T) {
	tests := []struct {
		input string
		want  []byte
	}{
		{`\r`, []byte{'\r'}},
		{`\n`, []byte{'\n'}},
		{`\t`, []byte{'\t'}},
		{`\u0003`, []byte{0x03}},
		{`\u001b[A`, []byte{0x1b, '[', 'A'}},
		{`\x41`, []byte{'A'}},
		{`\\`, []byte{'\\'}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			verbs, err := ParseVerbScript("send " + tt.input)
			if err != nil {
				t.Fatal(err)
			}
			b64 := strings.TrimPrefix(verbs[0].Wire, "send_b64 ")
			got, _ := base64.StdEncoding.DecodeString(b64)
			if string(got) != string(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTmuxExpectUnicode(t *testing.T) {
	verbs, err := ParseVerbScript(`expect "❯ "`)
	if err != nil {
		t.Fatal(err)
	}
	v := verbs[0]
	if v.Name != "expect" {
		t.Fatalf("expected expect, got %s", v.Name)
	}
	parts := strings.Fields(v.Wire)
	if parts[0] != "expect" {
		t.Fatalf("wire verb mismatch: %s", parts[0])
	}
	decoded, _ := base64.StdEncoding.DecodeString(parts[1])
	if string(decoded) != "❯ " {
		t.Fatalf("pattern mismatch: got %q", decoded)
	}
}

func TestParseTmuxKeyCommaSplit(t *testing.T) {
	verbs, err := ParseVerbScript(`key Enter,Up,Up,Enter`)
	if err != nil {
		t.Fatal(err)
	}
	v := verbs[0]
	if v.Wire != "key Enter Up Up Enter" {
		t.Fatalf("wire mismatch: got %q", v.Wire)
	}
}

func TestParseTmuxKeyRejectFlagInjection(t *testing.T) {
	_, err := ParseVerbScript(`key Enter -t hijack`)
	if err == nil {
		t.Fatal("expected error for flag injection, got nil")
	}
	if !strings.Contains(err.Error(), "invalid key name") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxUnknownVerb(t *testing.T) {
	_, err := ParseVerbScript(`frobnicate something`)
	if err == nil {
		t.Fatal("expected error for unknown verb")
	}
	if !strings.Contains(err.Error(), "unknown verb") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error missing verb/line: %s", err)
	}
}

func TestParseTmuxSpawnShellMetachars(t *testing.T) {
	verbs, err := ParseVerbScript(`spawn bash -c "$(whoami)"`)
	if err != nil {
		t.Fatal(err)
	}
	b64 := strings.TrimPrefix(verbs[0].Wire, "spawn_b64 ")
	decoded, _ := base64.StdEncoding.DecodeString(b64)
	parts := strings.Split(string(decoded), "\x00")
	if len(parts) != 3 {
		t.Fatalf("expected 3 NUL-delimited parts, got %d: %q", len(parts), parts)
	}
	if parts[2] != "$(whoami)" {
		t.Fatalf("metachar not preserved: got %q", parts[2])
	}
}

func TestParseTmuxExpectRE2(t *testing.T) {
	// RE2 handles this without catastrophic backtracking
	verbs, err := ParseVerbScript(`expect "(a+)+$"`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Name != "expect" {
		t.Fatalf("expected expect verb")
	}
}

func TestParseTmuxExpectInvalidRegex(t *testing.T) {
	_, err := ParseVerbScript(`expect "[invalid"`)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "pattern invalid") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxExpectTooLong(t *testing.T) {
	long := strings.Repeat("a", 201)
	_, err := ParseVerbScript(`expect "` + long + `"`)
	if err == nil {
		t.Fatal("expected error for long pattern")
	}
	if !strings.Contains(err.Error(), "exceeds 200") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxEmptyBody(t *testing.T) {
	verbs, err := ParseVerbScript("")
	if err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 0 {
		t.Fatalf("expected 0 verbs, got %d", len(verbs))
	}
}

func TestParseTmuxSleep(t *testing.T) {
	verbs, err := ParseVerbScript(`sleep 0.5`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Wire != "sleep 0.5" {
		t.Fatalf("wire mismatch: got %q", verbs[0].Wire)
	}
}

func TestParseTmuxExpectTimeout(t *testing.T) {
	verbs, err := ParseVerbScript(`expect "prompt" timeout=5s`)
	if err != nil {
		t.Fatal(err)
	}
	v := verbs[0]
	parts := strings.Fields(v.Wire)
	if parts[2] != "5" {
		t.Fatalf("timeout mismatch: got %s, want 5", parts[2])
	}
}

func TestParseTmuxSnapLines(t *testing.T) {
	verbs, err := ParseVerbScript(`snap lines=50`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Wire != "snap lines=50" {
		t.Fatalf("wire mismatch: got %q", verbs[0].Wire)
	}
}

func TestParseTmuxExpectMissingPattern(t *testing.T) {
	_, err := ParseVerbScript(`expect`)
	if err == nil {
		t.Fatal("expected error for missing pattern")
	}
	if !strings.Contains(err.Error(), "expect requires pattern") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxKill(t *testing.T) {
	verbs, err := ParseVerbScript(`kill`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Wire != "kill" {
		t.Fatalf("wire mismatch: got %q", verbs[0].Wire)
	}
}

func TestParseTmuxSnapDefault(t *testing.T) {
	verbs, err := ParseVerbScript(`snap`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Wire != "snap lines=200" {
		t.Fatalf("wire mismatch: got %q", verbs[0].Wire)
	}
}

func TestParseTmuxExpectSoft(t *testing.T) {
	verbs, err := ParseVerbScript(`expect? "maybe"`)
	if err != nil {
		t.Fatal(err)
	}
	v := verbs[0]
	if !strings.HasPrefix(v.Wire, "expect_q ") {
		t.Fatalf("expected expect_q prefix, got %q", v.Wire)
	}
}

func TestParseTmuxSleepInf(t *testing.T) {
	_, err := ParseVerbScript(`sleep Inf`)
	if err == nil {
		t.Fatal("expected error for sleep Inf")
	}
	if !strings.Contains(err.Error(), "0-3600") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxSleepNaN(t *testing.T) {
	_, err := ParseVerbScript(`sleep NaN`)
	if err == nil {
		t.Fatal("expected error for sleep NaN")
	}
	if !strings.Contains(err.Error(), "0-3600") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxSleepNegative(t *testing.T) {
	_, err := ParseVerbScript(`sleep -1`)
	if err == nil {
		t.Fatal("expected error for negative sleep")
	}
	if !strings.Contains(err.Error(), "0-3600") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxSleepOverMax(t *testing.T) {
	_, err := ParseVerbScript(`sleep 7200`)
	if err == nil {
		t.Fatal("expected error for sleep > 3600")
	}
	if !strings.Contains(err.Error(), "0-3600") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxExpectEmpty(t *testing.T) {
	_, err := ParseVerbScript(`expect ""`)
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxSnapOverLimit(t *testing.T) {
	_, err := ParseVerbScript(`snap lines=99999`)
	if err == nil {
		t.Fatal("expected error for snap lines > 10000")
	}
	if !strings.Contains(err.Error(), "1-10000") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxSnapNegative(t *testing.T) {
	_, err := ParseVerbScript(`snap lines=-1`)
	if err == nil {
		t.Fatal("expected error for negative snap lines")
	}
	if !strings.Contains(err.Error(), "1-10000") {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestParseTmuxExpectNewline(t *testing.T) {
	_, err := ParseVerbScript("expect \"foo\nbar\"")
	if err == nil {
		t.Fatal("expected error for newline in pattern")
	}
}

func TestParseTmuxKeyPageUp(t *testing.T) {
	verbs, err := ParseVerbScript(`key PageUp,PageDown`)
	if err != nil {
		t.Fatal(err)
	}
	if verbs[0].Wire != "key PageUp PageDown" {
		t.Fatalf("wire mismatch: got %q", verbs[0].Wire)
	}
}

func TestParseTmuxMultiLine(t *testing.T) {
	script := `spawn bash
send "ls\r"
expect "\\$"
snap
kill`
	verbs, err := ParseVerbScript(script)
	if err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 5 {
		t.Fatalf("expected 5 verbs, got %d", len(verbs))
	}
	names := []string{"spawn", "send", "expect", "snap", "kill"}
	for i, v := range verbs {
		if v.Name != names[i] {
			t.Fatalf("verb %d: expected %s, got %s", i, names[i], v.Name)
		}
		if v.Pos != i {
			t.Fatalf("verb %d: expected pos %d, got %d", i, i, v.Pos)
		}
	}
}

func TestParseTmuxPosContiguousWithBlanks(t *testing.T) {
	script := "spawn bash\n\n\nsend \"ls\\r\"\n\nexpect \"\\\\$\"\nsnap\n\nkill"
	verbs, err := ParseVerbScript(script)
	if err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 5 {
		t.Fatalf("expected 5 verbs, got %d", len(verbs))
	}
	for i, v := range verbs {
		if v.Pos != i {
			t.Fatalf("verb %d (%s): expected contiguous pos %d, got %d", i, v.Name, i, v.Pos)
		}
	}
}
