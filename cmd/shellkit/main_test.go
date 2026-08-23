package main

import "testing"

func TestNearest(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		candidates []string
		want       string
	}{
		{"transposed letters", "log-dashbaord", mcpSubcommands, "log-dashboard"},
		{"missing letter", "statu", mcpSubcommands, "status"},
		{"extra letter", "restartt", mcpSubcommands, "restart"},
		{"case insensitive", "STOP", mcpSubcommands, "stop"},
		{"top-level typo", "lst", topCommands, "list"},
		{"top-level typo long", "generate-config", topCommands, "generate-configs"},
		{"nothing close", "frobnicate", mcpSubcommands, ""},
		{"empty input", "", mcpSubcommands, ""},
		{"single char is not a guess for long verbs", "x", mcpSubcommands, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nearest(tt.input, tt.candidates); got != tt.want {
				t.Errorf("nearest(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The inventory gate must let a typo through to its own error message, and
// still hold back the subcommands that actually serve hosts.
func TestMCPNoInventorySubcmd(t *testing.T) {
	needsInventory := []string{"stdio", "serve", "start"}
	for _, sub := range needsInventory {
		if mcpNoInventorySubcmd([]string{"mcp", sub}) {
			t.Errorf("mcp %s should require the inventory", sub)
		}
	}
	skipsInventory := []string{"stop", "status", "restart", "log-dashboard", "render-dashboard", "log-dashbaord"}
	for _, sub := range skipsInventory {
		if !mcpNoInventorySubcmd([]string{"mcp", sub}) {
			t.Errorf("mcp %s should not require the inventory", sub)
		}
	}
	if mcpNoInventorySubcmd([]string{"mcp"}) {
		t.Error("bare `mcp` (stdio) should require the inventory")
	}
	if mcpNoInventorySubcmd([]string{"list"}) {
		t.Error("non-mcp commands should not be treated as inventory-free")
	}
}
