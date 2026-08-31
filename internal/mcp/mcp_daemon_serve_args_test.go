package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `shellkit mcp start` re-execs itself as `mcp serve`. With an inventory it
// pins the child to the absolute path and that file's directory (relative
// "sops:" password_ref paths resolve from there).
func TestServeInvocationWithInventory(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "hosts.nix")
	if err := os.WriteFile(inv, []byte("{ hosts = {}; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args, workDir, err := serveInvocation(inv, 19222)
	if err != nil {
		t.Fatalf("serveInvocation: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f "+inv) {
		t.Errorf("child should be pinned to the inventory, got: %v", args)
	}
	if !strings.HasSuffix(joined, "mcp serve -p 19222") {
		t.Errorf("child should run `mcp serve -p PORT`, got: %v", args)
	}
	if workDir != dir {
		t.Errorf("workDir = %q, want the inventory's directory %q", workDir, dir)
	}
}

// With NO inventory the child must be spawned WITHOUT -f. filepath.Abs("")
// returns the current directory, so passing it through would hand the child
// `-f <cwd>` — a directory, which LoadInventory rejects as an unsupported
// format, and the daemon would die on its first breath on every UCC host that
// carries no inventory.
func TestServeInvocationWithoutInventory(t *testing.T) {
	args, workDir, err := serveInvocation("", 19222)
	if err != nil {
		t.Fatalf("serveInvocation: %v", err)
	}
	for _, a := range args {
		if a == "-f" {
			t.Fatalf("no inventory must mean no -f flag, got: %v", args)
		}
	}
	if strings.Join(args, " ") != "mcp serve -p 19222" {
		t.Errorf("args = %v, want [mcp serve -p 19222]", args)
	}
	home, _ := os.UserHomeDir()
	if workDir != home {
		t.Errorf("workDir = %q, want the home directory %q", workDir, home)
	}
}
