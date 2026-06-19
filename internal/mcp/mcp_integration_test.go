// Integration tests against orbctl VMs.
// Skipped in CI / when no orbctl available.
//
// Run manually: go test -run TestIntegration -v

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
)

func orbctlAvailable() bool {
	out, err := exec.Command("orbctl", "list").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "web-1" && fields[1] == "running" {
			return true
		}
	}
	return false
}

func skipWithoutOrbctl(t *testing.T) {
	t.Helper()
	if os.Getenv("SHELLKIT_INTEGRATION") == "" && !orbctlAvailable() {
		t.Skip("orbctl web-1 not running; set SHELLKIT_INTEGRATION=1 to force")
	}
}

func testServers() []inventory.Server {
	return []inventory.Server{
		{
			Name:     "orb-web-1",
			Orb:      "web-1",
			IP:       "192.168.139.185",
			Port:     0,
			User:     "",
			Provider: "local",
			Role:     "dev",
			Location: "local",
			State:    "running",
		},
	}
}

func TestIntegration_SSHExecute(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, err := NewOutputStore(servers)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(store, servers)

	input := `### echo-remote
{"ssh": "orb-web-1"}

echo "hello from remote"
hostname
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) == 0 {
		t.Fatal("no results")
	}
	r := results[0]
	if r.ExitCode != 0 {
		t.Errorf("exit: %d, stderr: %s, error: %s", r.ExitCode, r.Stderr, r.Error)
	}
	if !strings.Contains(r.Stdout, "hello from remote") {
		t.Errorf("stdout should contain greeting, got: %q", r.Stdout)
	}
}

func TestIntegration_OutputCapture(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### capture-test
{"ssh": "orb-web-1"}

echo "regular stdout"
echo "kernel=$(uname -r)" >> $OUTPUT
echo "arch=$(uname -m)" >> $OUTPUT
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if r.ExitCode != 0 {
		t.Fatalf("exit: %d, stderr: %s", r.ExitCode, r.Stderr)
	}
	if r.Outputs["kernel"] == "" {
		t.Error("expected kernel output key")
	}
	if r.Outputs["arch"] == "" {
		t.Error("expected arch output key")
	}
	if !strings.Contains(r.Stdout, "regular stdout") {
		t.Errorf("stdout should contain 'regular stdout', got: %q", r.Stdout)
	}
	t.Logf("kernel=%s arch=%s", r.Outputs["kernel"], r.Outputs["arch"])
}

func TestIntegration_MultiStepChaining(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### get-info
{"ssh": "orb-web-1"}

echo "hostname=$(hostname)" >> $OUTPUT

### use-info

echo "remote host is {{get-info.outputs.hostname}}"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
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

	if !strings.Contains(results[1].Stdout, "remote host is") {
		t.Errorf("chaining: stdout should contain resolved hostname, got: %q", results[1].Stdout)
	}
}

func TestIntegration_LocalScpRoundTrip(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	tmpDir := t.TempDir()
	localFile := filepath.Join(tmpDir, "test-push.txt")
	os.WriteFile(localFile, []byte("pushed content\n"), 0644)

	remotePath := "/tmp/shellkit-test-push.txt"
	pullPath := filepath.Join(tmpDir, "test-pull.txt")

	// File transfer is now expressed via plain local scp commands.
	// The orb-web-1 alias must resolve through ~/.ssh/config (or shellkit's
	// generated config) on the host running this test.
	input := `### push-file

scp ` + localFile + ` orb-web-1:` + remotePath + `

### verify-push
{"ssh": "orb-web-1"}

cat ` + remotePath + `

### pull-file

scp orb-web-1:` + remotePath + ` ` + pullPath + `
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range results {
		if r.ExitCode != 0 {
			t.Errorf("step %s: exit %d, stderr: %s", r.Name, r.ExitCode, r.Stderr)
		}
	}

	if !strings.Contains(results[1].Stdout, "pushed content") {
		t.Errorf("push verify: stdout should contain 'pushed content', got: %q", results[1].Stdout)
	}

	pulled, err := os.ReadFile(pullPath)
	if err != nil {
		t.Fatalf("pull file not found: %v", err)
	}
	if !strings.Contains(string(pulled), "pushed content") {
		t.Errorf("pulled content: want 'pushed content', got: %q", string(pulled))
	}

	exec.Command("ssh", "root@web-1@orb", "rm", "-f", remotePath).Run()
}

func TestIntegration_NonBashEntrypoint(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### python-test
{"ssh": "orb-web-1", "entrypoint": "python3"}

import os, sys
print(f"python {sys.version_info.major}.{sys.version_info.minor}")
with open(os.environ.get('OUTPUT', '/dev/null'), 'a') as f:
    f.write(f"pyver={sys.version_info.major}.{sys.version_info.minor}\n")
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if r.ExitCode != 0 {
		t.Fatalf("exit: %d, stderr: %s", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "python") {
		t.Errorf("stdout should contain 'python', got: %q", r.Stdout)
	}
	if r.Outputs["pyver"] == "" {
		t.Error("expected pyver output key")
	}
	t.Logf("python version: %s", r.Outputs["pyver"])
}

func TestIntegration_Timeout(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### slow-remote
{"ssh": "orb-web-1", "timeout": 2}

sleep 30
echo "should not reach here"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	results, err := executor.Execute(context.Background(), steps)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout should fire in ~2s, took %v", elapsed)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].ExitCode == 0 {
		t.Error("timed-out step should have non-zero exit")
	}
}

func TestIntegration_ContinueOnError(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### fail-step
{"ssh": "orb-web-1", "continue_on_error": true}

exit 42

### after-fail
{"ssh": "orb-web-1"}

echo "still running"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	if !names["after-fail"] {
		t.Error("step after continue_on_error failure should still run")
	}
}

func TestIntegration_AbortOnFailure(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### fail-step
{"ssh": "orb-web-1"}

exit 1

### should-skip
{"ssh": "orb-web-1"}

echo "this should not run"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	if names["should-skip"] {
		t.Error("step after failure should be skipped (abort default)")
	}
}

func TestIntegration_StderrCapture(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### stderr-test
{"ssh": "orb-web-1"}

echo "stdout line"
echo "stderr line" >&2
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if r.ExitCode != 0 {
		t.Fatalf("exit: %d", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "stdout line") {
		t.Errorf("stdout: got %q", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "stderr line") {
		t.Errorf("stderr should contain 'stderr line', got: %q", r.Stderr)
	}
}

func TestIntegration_OutputFileWritten(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### file-test
{"ssh": "orb-web-1"}

echo "file content here"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}

	r := results[0]
	if r.FilePath == "" {
		t.Fatal("expected output file path")
	}
	data, err := os.ReadFile(r.FilePath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(data), "file content here") {
		t.Errorf("output file: got %q", string(data))
	}
}

func TestIntegration_FullPipeline(t *testing.T) {
	skipWithoutOrbctl(t)
	servers := testServers()
	store, _ := NewOutputStore(servers)
	executor := NewExecutor(store, servers)

	input := `### get-kernel
{"ssh": "orb-web-1"}

echo "kernel=$(uname -r)" >> $OUTPUT
echo "arch=$(uname -m)" >> $OUTPUT

### report

echo "Remote kernel: {{get-kernel.outputs.kernel}}"
echo "Remote arch: {{get-kernel.outputs.arch}}"
echo "status=verified" >> $OUTPUT
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	results, err := executor.Execute(context.Background(), steps)
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

	// remote should capture kernel
	if results[0].Outputs["kernel"] == "" {
		t.Error("get-kernel should have kernel output")
	}

	// local step should resolve template and capture its own output
	if !strings.Contains(results[1].Stdout, "Remote kernel:") {
		t.Errorf("report stdout should contain 'Remote kernel:', got: %q", results[1].Stdout)
	}
	if results[1].Outputs["status"] != "verified" {
		t.Errorf("report status: want 'verified', got %q", results[1].Outputs["status"])
	}

	t.Logf("Full pipeline passed. kernel=%s arch=%s",
		results[0].Outputs["kernel"], results[0].Outputs["arch"])
}
