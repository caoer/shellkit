package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/inventory"
)

// On a UCC host with no Nix inventory the MCP server serves with zero hosts.
// A step that names an inventory host must then say WHY the name did not
// resolve — a bare "unknown host" sends an agent hunting for a typo when the
// real answer is that this machine has no inventory at all.
func TestUnknownHostNamesTheMissingInventory(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "deploy",
		Action: ActionTmux,
		Hosts:  []string{"not a host:sess"},
		Body:   "snap",
	}
	_, err := ex.executeTmux(context.Background(), 0, step)
	if err == nil {
		t.Fatal("should error on an unresolvable host")
	}
	if !strings.Contains(err.Error(), "unknown host") {
		t.Errorf("error should still say unknown host, got: %s", err)
	}
	if !strings.Contains(err.Error(), "SHELLKIT_INVENTORY") {
		t.Errorf("empty inventory should be named as the likely cause, got: %s", err)
	}
}

// With hosts loaded, an unknown name is a plain typo — the inventory guidance
// would be noise and must not appear.
func TestUnknownHostStaysTerseWithAnInventory(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, []inventory.Server{{Name: "web-1", IP: "10.0.0.1"}})

	step := &Step{
		Name:   "deploy",
		Action: ActionTmux,
		Hosts:  []string{"not a host:sess"},
		Body:   "snap",
	}
	_, err := ex.executeTmux(context.Background(), 0, step)
	if err == nil {
		t.Fatal("should error on an unresolvable host")
	}
	if strings.Contains(err.Error(), "SHELLKIT_INVENTORY") {
		t.Errorf("a loaded inventory should not draw the no-inventory hint, got: %s", err)
	}
}
