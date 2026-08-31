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

// The MCP server is registered unconditionally on every UCC host, so no `mcp`
// subcommand may exit on a missing inventory — it serves with zero inventory
// hosts and still reaches raw `user@host` targets and ~/.ssh/config aliases.
// Every other verb keeps the inventory wall.
func TestMCPVerbNeverRequiresInventory(t *testing.T) {
	subs := []string{
		"", "stdio", "serve", "start",
		"stop", "status", "restart", "log-dashboard", "render-dashboard",
		"log-dashbaord", // a typo answers with its own message, not the wall
	}
	for _, sub := range subs {
		args := []string{"mcp"}
		if sub != "" {
			args = append(args, sub)
		}
		if !mcpVerb(args) {
			t.Errorf("mcp %q must not require the inventory", sub)
		}
	}
	for _, cmd := range []string{"list", "check", "ssh", "generate-configs", ""} {
		var args []string
		if cmd != "" {
			args = []string{cmd}
		}
		if mcpVerb(args) {
			t.Errorf("%q is not the mcp verb and must keep the inventory wall", cmd)
		}
	}
}
