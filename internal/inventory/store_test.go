package inventory

import "testing"

// A host with no Nix inventory still gets an MCP server: the store opens empty
// rather than refusing, so raw `user@host` targets and ~/.ssh/config aliases
// keep working. Watching and reloading a file that does not exist are no-ops,
// not errors — an empty store must never take the server down.
func TestNewEmptyStore(t *testing.T) {
	s := NewEmptyStore()

	if got := s.Get(); len(got) != 0 {
		t.Errorf("empty store has %d servers, want 0", len(got))
	}
	if s.HasInventory() {
		t.Error("empty store reports HasInventory() true")
	}
	if err := s.StartWatcher(); err != nil {
		t.Errorf("StartWatcher on an empty store: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Errorf("Reload on an empty store: %v", err)
	}
}

// A store built from a real path reports that it has an inventory, which is
// what the MCP layer keys its "unknown host" guidance on.
func TestHasInventoryOnRealPath(t *testing.T) {
	s := &InventoryStore{path: "/somewhere/hosts.nix"}
	if !s.HasInventory() {
		t.Error("a store with a path reports HasInventory() false")
	}
}
