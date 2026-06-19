package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasPassword(t *testing.T) {
	if (Server{}).HasPassword() {
		t.Fatal("empty server should not have password")
	}
	if !(Server{PasswordRef: "sops:x#y"}).HasPassword() {
		t.Fatal("server with ref should have password")
	}
}

func TestFindSopsRoot(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "lib", "ssh", "hosts")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := FindSopsRoot(deep); got != dir {
		t.Fatalf("FindSopsRoot = %q, want %q", got, dir)
	}
	if got := FindSopsRoot(t.TempDir()); got != "" {
		t.Fatalf("expected empty for tree without .sops.yaml, got %q", got)
	}
}
