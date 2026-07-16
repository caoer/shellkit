package rundaemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerGz_KnownTargets(t *testing.T) {
	for _, target := range []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	} {
		b, err := RunnerGz(target.goos, target.goarch)
		if err != nil {
			t.Fatalf("RunnerGz(%s, %s): %v", target.goos, target.goarch, err)
		}
		if len(b) == 0 {
			t.Fatalf("RunnerGz(%s, %s): empty payload", target.goos, target.goarch)
		}
	}
}

func TestRunnerGz_UnknownTarget(t *testing.T) {
	if _, err := RunnerGz("plan9", "386"); err == nil {
		t.Fatal("RunnerGz(plan9, 386): expected error for unsupported target, got nil")
	}
}

// TestEmbeddedRunner_VersionMatches decompresses the embedded runner for THIS
// host's platform, execs it with --version, and asserts the output equals
// RunnerVersion. That proves two things at once: the embedded blob is a real
// bootable binary (not a placeholder), and the bootstrap version-sync invariant
// holds — the runner's stamped main.version, which probe/exec-test compare
// against, equals the daemon's RunnerVersion (both set by `just build-runners`).
// It runs only when the host GOOS/GOARCH is in decision #13's embed matrix;
// other hosts skip, since a cross-compiled binary can't be exec'd here.
func TestEmbeddedRunner_VersionMatches(t *testing.T) {
	gz, err := RunnerGz(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no embedded runner for host %s/%s: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	raw, err := gunzip(gz)
	if err != nil {
		t.Fatalf("gunzip embedded runner: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "shellkit-runner")
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("exec %s --version: %v", bin, err)
	}
	got := strings.TrimSpace(string(out))
	if got != RunnerVersion {
		t.Fatalf("embedded runner --version = %q, want RunnerVersion %q (version-sync broken)", got, RunnerVersion)
	}
	if got == "dev" {
		t.Fatalf("RunnerVersion is still %q — version_gen.go not wired (run `just build-runners`)", got)
	}
}
