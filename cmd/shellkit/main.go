package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/caoer/shellkit/internal/dashboard"
	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/sshconn"
	"github.com/caoer/shellkit/internal/tui"
)

// inventorySearchPaths are conventional inventory filenames, searched from the
// current directory upward. The primary inventory source is the -f flag or
// SHELLKIT_INVENTORY; these are a convenience fallback for repos that keep an
// inventory at a well-known path (e.g. an osfiles-style nix host registry).
var inventorySearchPaths = []string{
	"shellkit.nix",
	"inventory.nix",
	"hosts.nix",
	"lib/ssh/hosts/default.nix",
}

// noInventoryHelp is shown when no inventory can be located, so the failure is
// loud and actionable instead of an empty host table.
const noInventoryHelp = `error: no SSH inventory found.

shellkit reads its hosts from a Nix file exposing a "hosts" attribute set.
Point it at one with either:

  -f <path>                    shellkit -f ./inventory.nix list
  SHELLKIT_INVENTORY=<path>    export SHELLKIT_INVENTORY=./inventory.nix

Or drop one of these files in the current directory (searched upward):
  shellkit.nix, inventory.nix, hosts.nix, lib/ssh/hosts/default.nix

See examples/inventory.sample.nix for the expected format.
`

func findInventory() string {
	if env := os.Getenv("SHELLKIT_INVENTORY"); env != "" {
		return env
	}

	dir, _ := os.Getwd()
	for {
		for _, rel := range inventorySearchPaths {
			candidate := filepath.Join(dir, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// version is the shellkit build version, injected at build time via
// -ldflags "-X main.version=<v>". Defaults to "dev" for plain source builds.
var version = "dev"

// gitCommit is the short git commit hash, injected at build time via
// -ldflags "-X main.gitCommit=<hash>".
var gitCommit = "unknown"

func usage() {
	fmt.Fprintf(os.Stderr, `shellkit — SSH server inventory & connectivity checker

Usage:
  shellkit                    Interactive TUI (default)
  shellkit list               Print server table
  shellkit check [pattern]    Probe servers (optional regex filter on name/alias)
  shellkit ssh <name>         SSH into a server by name/alias
  shellkit generate-configs   Generate SSH config (writes to SHELLKIT_GENERATED_CONFIG_PATH or stdout)
  shellkit version            Print version and exit
  shellkit mcp                Start MCP server (stdio transport)
  shellkit mcp serve [-p PORT]   Run MCP HTTP server (foreground)
  shellkit mcp start [-p PORT]   Start MCP HTTP daemon
  shellkit mcp stop              Stop MCP daemon
  shellkit mcp restart [-p PORT] Restart MCP daemon
  shellkit mcp status            Check daemon status
  shellkit mcp log-dashboard     Interactive TUI for MCP call logs

Flags:
  -f <path>          Inventory file (.nix exposing a 'hosts' attset; default: auto-detect)
  --json             JSON output (list/check only)
  --managed <value>  Filter to hosts with managed=<value> (e.g. osfiles)
  --addr <pref>      Address preference: auto|wan|lan|wireguard|tailscale|easytier (default: auto)
  -h                 Help

Environment:
  SHELLKIT_INVENTORY              Inventory file path (.nix exposing a 'hosts' attset)
  SHELLKIT_ADDR_PREF              Default address preference (overridden by --addr flag)
  SHELLKIT_DEFAULT_IDENTITY       Fallback SSH identity when a host sets none (default: ~/.ssh/id_ed25519)
  SHELLKIT_GENERATED_CONFIG_PATH  Destination for generate-configs (default: stdout)
  SHELLKIT_MCP_PORT               MCP HTTP port (default: 19222)
  SHELLKIT_MCP_TOKEN              Bearer token for the MCP HTTP server
`)
	os.Exit(0)
}

// extractGlobalFlags pulls -f/--json/-h from anywhere in args, regardless of
// position relative to the subcommand. The Go stdlib `flag` stops parsing at
// the first positional, so without this, `shellkit list --json` would silently
// ignore the flag.
func extractGlobalFlags(args []string) (inventoryPath string, jsonOutput bool, managedFilter string, addrPrefStr string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "-help":
			usage()
		case a == "--version" || a == "-version" || a == "-v":
			fmt.Println("shellkit " + version + " (" + gitCommit + ")")
			os.Exit(0)
		case a == "--json" || a == "-json":
			jsonOutput = true
		case a == "-f" || a == "--f":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: -f requires a path argument")
				os.Exit(1)
			}
			inventoryPath = args[i+1]
			i++
		case strings.HasPrefix(a, "-f=") || strings.HasPrefix(a, "--f="):
			inventoryPath = strings.SplitN(a, "=", 2)[1]
		case strings.HasPrefix(a, "--json="):
			v := strings.SplitN(a, "=", 2)[1]
			jsonOutput = v == "true" || v == "1"
		case a == "--managed":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --managed requires a value (e.g. osfiles)")
				os.Exit(1)
			}
			managedFilter = args[i+1]
			i++
		case strings.HasPrefix(a, "--managed="):
			managedFilter = strings.SplitN(a, "=", 2)[1]
		case a == "--addr":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --addr requires a value (auto|wan|lan|wireguard|tailscale|easytier)")
				os.Exit(1)
			}
			addrPrefStr = args[i+1]
			i++
		case strings.HasPrefix(a, "--addr="):
			addrPrefStr = strings.SplitN(a, "=", 2)[1]
		default:
			rest = append(rest, a)
		}
	}
	return
}

func main() {
	flag.Usage = usage

	inventoryPath, jsonOutput, managedFilter, addrPrefStr, rest := extractGlobalFlags(os.Args[1:])

	// version needs no inventory — handle it before the inventory check.
	if len(rest) > 0 && rest[0] == "version" {
		fmt.Println("shellkit " + version + " (" + gitCommit + ")")
		return
	}

	if addrPrefStr == "" {
		addrPrefStr = os.Getenv("SHELLKIT_ADDR_PREF")
	}
	if addrPrefStr != "" {
		parsed, err := inventory.ParseAddrPref(addrPrefStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		sshconn.SetDefaultAddrPref(parsed)
	}

	if inventoryPath == "" {
		inventoryPath = findInventory()
	}

	// Resolve the sops repo root (owner of .sops.yaml) from the inventory
	// location so relative "sops:" password_ref paths resolve correctly,
	// independent of the current working directory.
	if inventoryPath != "" {
		if abs, err := filepath.Abs(inventoryPath); err == nil {
			sshconn.SetSopsRoot(inventory.FindSopsRoot(filepath.Dir(abs)))
		}
	}

	// Daemon-management subcommands (stop, status, restart, log-dashboard,
	// render-dashboard) don't need the inventory — skip the check so they
	// work from any cwd.
	if inventoryPath == "" && !mcpNoInventorySubcmd(rest) {
		fmt.Fprint(os.Stderr, noInventoryHelp)
		os.Exit(1)
	}

	var servers []inventory.Server
	if inventoryPath != "" {
		var err error
		servers, err = inventory.LoadInventory(inventoryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	if managedFilter != "" {
		filtered := servers[:0]
		for _, s := range servers {
			if s.Managed == managedFilter {
				filtered = append(filtered, s)
			}
		}
		servers = filtered
	}

	cmd := ""
	if len(rest) > 0 {
		cmd = rest[0]
		rest = rest[1:]
	}

	switch cmd {
	case "", "tui":
		if err := tui.RunTUI(servers); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "list":
		tui.CLIList(servers, jsonOutput)
	case "check":
		if len(rest) > 0 {
			re, err := regexp.Compile("(?i)" + rest[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid pattern: %v\n", err)
				os.Exit(1)
			}
			filtered := servers[:0]
			for _, s := range servers {
				if re.MatchString(s.Name) || re.MatchString(s.SSHAlias) {
					filtered = append(filtered, s)
				}
			}
			servers = filtered
		}
		tui.CLICheck(servers, jsonOutput)
	case "generate-configs":
		nukeControlSockets()
		if dest := os.Getenv("SHELLKIT_GENERATED_CONFIG_PATH"); dest != "" {
			f, err := os.Create(dest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "generate-configs: %v\n", err)
				os.Exit(1)
			}
			sshconn.GenerateSSHConfig(servers, f)
			f.Close()
			fmt.Fprintf(os.Stderr, "wrote %s\n", dest)
		} else {
			sshconn.GenerateSSHConfig(servers, os.Stdout)
		}
	case "ssh":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		if name == "" {
			fmt.Fprintln(os.Stderr, "usage: shellkit ssh <name|alias>")
			os.Exit(1)
		}
		sshByName(servers, name)
	case "mcp":
		mcpSubcmd(servers, inventoryPath, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
	}
}

// mcpNoInventorySubcmd returns true when the command-line arguments point at
// an MCP subcommand that does not need the SSH inventory (stop, status,
// restart, log-dashboard, render-dashboard).
func mcpNoInventorySubcmd(args []string) bool {
	if len(args) < 2 || args[0] != "mcp" {
		return false
	}
	switch args[1] {
	case "stop", "status", "restart", "log-dashboard", "render-dashboard":
		return true
	}
	return false
}

func mcpSubcmd(servers []inventory.Server, inventoryPath string, args []string) {
	subcmd := ""
	if len(args) > 0 {
		subcmd = args[0]
	}

	var remaining []string
	if len(args) > 1 {
		remaining = args[1:]
	}

	// render-dashboard owns its own flag parsing — bypass the shared flagset
	// so flags like --all/--view aren't rejected as unknown.
	if subcmd == "render-dashboard" {
		if err := dashboard.RunRenderDashboard(remaining); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	port := fs.Int("p", mcp.Port(), "HTTP port")
	fs.Parse(remaining)

	switch subcmd {
	case "", "stdio":
		store, err := inventory.NewInventoryStore(inventoryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
		if err := store.StartWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: file watcher failed: %v (continuing without live reload)\n", err)
		}
		if err := mcp.RunMCP(store); err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		store, err := inventory.NewInventoryStore(inventoryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
		if err := store.StartWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: file watcher failed: %v (continuing without live reload)\n", err)
		}
		if err := mcp.RunMCPHTTP(store, *port); err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
	case "start":
		if err := mcp.Start(inventoryPath, *port); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "stop":
		if err := mcp.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "restart":
		if err := mcp.Restart(inventoryPath, *port); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		mcp.Status(*port)
	case "log-dashboard", "logs":
		if err := dashboard.RunLogDashboard(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

// nukeControlSockets removes all SSH ControlMaster sockets so regenerated
// config starts with fresh connections. Errors are non-fatal (best-effort).
func nukeControlSockets() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	sockDir := filepath.Join(home, ".ssh", "sockets")
	entries, err := os.ReadDir(sockDir)
	if err != nil {
		// Dir doesn't exist yet — ensure it does for new sockets.
		os.MkdirAll(sockDir, 0700)
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(sockDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "nuked %d control socket(s) in %s\n", removed, sockDir)
	}
}

func sshByName(servers []inventory.Server, name string) {
	for _, s := range servers {
		if s.Name == name || s.SSHAlias == name {
			cmd := sshconn.SSHCommand(&s)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			os.Exit(exitCode(cmd.Run()))
		}
	}
	fmt.Fprintf(os.Stderr, "server not found: %s\n", name)
	os.Exit(1)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return 1
}
