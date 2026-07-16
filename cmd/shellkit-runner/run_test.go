package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// fixedLookup returns a lookup func over a fixed env for buildStepEnv tests, so
// the policy is exercised independent of the test host's real environment.
func fixedLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func pairsToMap(pairs []string) map[string]string {
	m := map[string]string{}
	for _, p := range pairs {
		if i := strings.IndexByte(p, '='); i >= 0 {
			m[p[:i]] = p[i+1:]
		}
	}
	return m
}

// TestBuildStepEnv_ClosedAllowlist is the decision #17 security assertion: an
// extra key on the run frame must NOT reach the step env, a frame PATH override
// must NOT win over the runner's base PATH, and OUTPUT is forced to the
// runner-owned scratch path.
func TestBuildStepEnv_ClosedAllowlist(t *testing.T) {
	base := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/home/runner",
		"SECRET": "should-not-be-read", // not in baseEnvAllowlist → must be ignored
	}
	frameEnv := map[string]string{
		"SSHPASS": "hunter2",          // secret the daemon must never be able to inject
		"OUTPUT":  "/attacker/output", // frame OUTPUT must lose to the runner path
		"PATH":    "/evil/bin",        // PATH override attempt must be dropped
	}

	got := pairsToMap(buildStepEnv(frameEnv, "/scratch/.shellkit-output", fixedLookup(base)))

	if _, leaked := got["SSHPASS"]; leaked {
		t.Fatalf("frame secret SSHPASS leaked into step env: %v", got)
	}
	if _, leaked := got["SECRET"]; leaked {
		t.Fatalf("non-allowlisted base key SECRET leaked into step env: %v", got)
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("frame overrode PATH: got %q, want the runner base /usr/bin:/bin", got["PATH"])
	}
	if got["HOME"] != "/home/runner" {
		t.Fatalf("base HOME missing/wrong: got %q", got["HOME"])
	}
	if got["OUTPUT"] != "/scratch/.shellkit-output" {
		t.Fatalf("OUTPUT not forced to runner scratch path: got %q", got["OUTPUT"])
	}
}

func TestBuildStepEnv_Deterministic(t *testing.T) {
	base := map[string]string{"PATH": "/bin", "HOME": "/h"}
	a := buildStepEnv(nil, "/o", fixedLookup(base))
	b := buildStepEnv(nil, "/o", fixedLookup(base))
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("buildStepEnv not deterministic:\n%v\n%v", a, b)
	}
}

func TestSafeScratchPath(t *testing.T) {
	scratch := "/tmp/scratch"
	cases := []struct {
		name string
		ok   bool
	}{
		{"output.txt", true},
		{"step.output", true},
		{".hidden", true},
		{"../etc/x", false},  // traversal
		{"/abs/path", false}, // absolute
		{"a/b", false},       // directory component
		{"..", false},        // parent
		{".", false},         // current
		{"", false},          // empty
	}
	for _, tc := range cases {
		dest, err := safeScratchPath(scratch, tc.name)
		if tc.ok {
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.name, err)
				continue
			}
			if filepath.Dir(dest) != scratch {
				t.Errorf("%q: staged outside scratch: %q", tc.name, dest)
			}
		} else if err == nil {
			t.Errorf("%q: expected refusal, got dest %q", tc.name, dest)
		}
	}
}

func TestCollectOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// "=" inside a value must SplitN at the first '='; blank lines skipped; a
	// leading-'=' line (idx==0) is ignored, matching mcp.ParseOutputs.
	writeFile(t, out, "x=1\n\n  y = a=b \n=leading\nzonly\n")
	got := collectOutput(out)
	want := map[string]string{"x": "1", "y": "a=b"}
	if len(got) != len(want) {
		t.Fatalf("collectOutput = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestCollectOutput_Missing(t *testing.T) {
	if got := collectOutput(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Fatalf("missing output file: got %v, want nil", got)
	}
}

// TestRecoverInterpPanic verifies the recover→result mapping directly (a real
// upstream panic is version-fragile to trigger): exit 2 and the fallback hint.
func TestRecoverInterpPanic(t *testing.T) {
	code, msg := func() (code int, msg string) {
		defer recoverInterpPanic(&code, &msg)
		panic("boom")
	}()
	if code != 2 {
		t.Fatalf("panic exit = %d, want 2", code)
	}
	if !strings.Contains(msg, "interp panicked") || !strings.Contains(msg, panicFallbackHint) {
		t.Fatalf("panic message missing context/hint: %q", msg)
	}
}

// TestRunInterp_OutputAndExit runs a real bash body through interp: it writes to
// $OUTPUT and exits non-zero, proving env wiring ($OUTPUT resolves), output
// collection, and exit propagation.
func TestRunInterp_OutputAndExit(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)
	writeFile(t, outputPath, "") // pre-created, as handleRun does

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{Program: []byte("printf 'k=v\\n' > \"$OUTPUT\"\nexit 4\n")}

	code, runErr := r.runInterp(context.Background(), rf, scratch, outputPath)
	if runErr != "" {
		t.Fatalf("unexpected runner error: %q", runErr)
	}
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if got := collectOutput(outputPath); got["k"] != "v" {
		t.Fatalf("$OUTPUT not collected: %v", got)
	}
}

// TestRunInterp_EnvClosed proves at runtime that a secret on the frame env is
// not visible to the body: the body prints $SSHPASS, which must be empty.
func TestRunInterp_EnvClosed(t *testing.T) {
	scratch := t.TempDir()
	outputPath := filepath.Join(scratch, outputFileName)

	var buf bytes.Buffer
	r := &runner{enc: runnerproto.NewEncoder(&buf), errOut: &buf}
	rf := &runnerproto.RunFrame{
		Program: []byte(`printf 'leak=[%s]\n' "$SSHPASS"`),
		Env:     map[string]string{"SSHPASS": "hunter2"},
	}
	if code, runErr := r.runInterp(context.Background(), rf, scratch, outputPath); runErr != "" || code != 0 {
		t.Fatalf("run failed: code=%d err=%q", code, runErr)
	}
	frames := decodeFrames(t, &buf)
	var stdout strings.Builder
	for _, f := range frames {
		if f.Type == runnerproto.FrameIO && f.IO.FD == 1 {
			b, _ := f.IO.Bytes()
			stdout.Write(b)
		}
	}
	if !strings.Contains(stdout.String(), "leak=[]") {
		t.Fatalf("SSHPASS leaked into step: stdout = %q", stdout.String())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileErr(path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
