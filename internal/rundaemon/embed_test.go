package rundaemon

import "testing"

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
