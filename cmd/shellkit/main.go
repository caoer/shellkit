package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/caoer/shellkit/internal/dashboard"
	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/mcp"
	"github.com/caoer/shellkit/internal/sshconn"
	"github.com/caoer/shellkit/internal/tui"
	"golang.org/x/term"
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

// topCommands and mcpSubcommands are the accepted verbs, in help order. They
// back the did-you-mean hint on a typo; "logs" (alias of log-dashboard) is
// omitted on purpose so the hint never names an undocumented spelling.
var topCommands = []string{"list", "check", "ssh", "generate-configs", "version", "tui", "mcp"}

var mcpSubcommands = []string{"stdio", "serve", "start", "stop", "restart", "status", "log-dashboard", "render-dashboard"}

// nearest returns the candidate closest to input by edit distance, or "" when
// nothing is close enough. The budget scales with input length (min 1 edit, and
// never more than a third of the word) so a real typo gets a suggestion while a
// wild guess gets the full list instead of a misleading one.
func nearest(input string, candidates []string) string {
	in := strings.ToLower(input)
	budget := len(in) / 3
	if budget < 1 {
		budget = 1
	}
	best, bestDist := "", budget+1
	for _, c := range candidates {
		if d := editDistance(in, strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is Levenshtein distance (a transposition counts as 2 edits).
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(min(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// unknownVerb reports a mistyped command on stderr: the bad input, the closest
// valid verb when there is one, and the full list. Never exits — the caller
// decides the exit code.
func unknownVerb(label, input, prefix string, candidates []string) {
	fmt.Fprintf(os.Stderr, "unknown %s: %s\n", label, input)
	if s := nearest(input, candidates); s != "" {
		fmt.Fprintf(os.Stderr, "did you mean: %s%s\n", prefix, s)
	}
	fmt.Fprintf(os.Stderr, "valid: %s\n", strings.Join(candidates, ", "))
}

func usage() {
	usageTo(os.Stderr)
	os.Exit(0)
}

func usageTo(w *os.File) {
	fmt.Fprintf(w, `shellkit — SSH server inventory & connectivity checker

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
  --extra-keys <path>  Try key against auth-ok hosts (repeatable; check only)
  --disable-default-key  Skip default identity fallback; no-key hosts report no-key instead of probing
  --disable-password     Skip password auth; password_ref hosts report auth-fail without resolving sops
  -h                 Help

Environment:
  SHELLKIT_INVENTORY              Inventory file path (.nix exposing a 'hosts' attset)
  SHELLKIT_ADDR_PREF              Default address preference (overridden by --addr flag)
  SHELLKIT_DEFAULT_IDENTITY       Fallback SSH identity when a host sets none (default: ~/.ssh/id_ed25519)
  SHELLKIT_GENERATED_CONFIG_PATH  Destination for generate-configs (default: stdout)
  SHELLKIT_MCP_PORT               MCP HTTP port (default: 19222)
  SHELLKIT_MCP_TOKEN              Bearer token for the MCP HTTP server
`)
}

// extractGlobalFlags pulls -f/--json/-h from anywhere in args, regardless of
// position relative to the subcommand. The Go stdlib `flag` stops parsing at
// the first positional, so without this, `shellkit list --json` would silently
// ignore the flag.
func extractGlobalFlags(args []string) (inventoryPath string, jsonOutput bool, managedFilter string, addrPrefStr string, extraKeyPaths []string, disableDefaultKey bool, disablePassword bool, rest []string) {
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
		case a == "--extra-keys":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --extra-keys requires a key path")
				os.Exit(1)
			}
			extraKeyPaths = append(extraKeyPaths, args[i+1])
			i++
		case strings.HasPrefix(a, "--extra-keys="):
			extraKeyPaths = append(extraKeyPaths, strings.SplitN(a, "=", 2)[1])
		case a == "--disable-default-key":
			disableDefaultKey = true
		case a == "--disable-password":
			disablePassword = true
		default:
			rest = append(rest, a)
		}
	}
	return
}

func main() {
	flag.Usage = usage

	inventoryPath, jsonOutput, managedFilter, addrPrefStr, extraKeyPaths, disableDefaultKey, disablePassword, rest := extractGlobalFlags(os.Args[1:])

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

	// No `mcp` subcommand exits on a missing inventory. The MCP server is
	// registered on every UCC host, most of which carry no Nix inventory, and
	// shellkit reaches raw "user@host" targets and ~/.ssh/config aliases
	// without one — so the server opens with zero inventory hosts, and a step
	// naming an inventory host is told why the name did not resolve. The
	// daemon-management subcommands (stop, status, log-dashboard, ...) never
	// needed it either, and a typo is answered by its own message.
	if inventoryPath == "" && !mcpVerb(rest) && !unknownTopVerb(rest) {
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
		tui.CLICheck(servers, jsonOutput, extraKeyPaths, disableDefaultKey, disablePassword)
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
		if reason := sshMisuse(rest); reason != "" {
			printSSHMisuse(reason, name, rest[1:])
			os.Exit(1)
		}
		sshByName(servers, name)
	case "mcp":
		mcpSubcmd(servers, inventoryPath, rest)
	default:
		unknownVerb("command", cmd, "shellkit ", topCommands)
		fmt.Fprintln(os.Stderr)
		usageTo(os.Stderr)
		os.Exit(1)
	}
}

// unknownTopVerb reports whether the first positional is not a known command,
// so a typo can be answered as a typo instead of as "no SSH inventory found".
// The switch in main() prints the message; this only opens the gate.
func unknownTopVerb(args []string) bool {
	return len(args) > 0 && !slices.Contains(topCommands, args[0])
}

// mcpVerb reports whether the command line is the `mcp` verb, in any of its
// subcommands. The whole verb is exempt from the "no SSH inventory found" wall
// — the serving subcommands open an empty store, the rest never touched the
// inventory — so that the MCP server registers on every host.
func mcpVerb(args []string) bool {
	return len(args) > 0 && args[0] == "mcp"
}

// openMCPStore builds the inventory store the MCP server serves from. With no
// inventory file on this host the store opens EMPTY instead of exiting: raw
// "user@host" targets and ~/.ssh/config aliases resolve without an inventory,
// so the server stays useful and a step that names an inventory host is told
// what is missing. A path that exists but cannot be read is still fatal — that
// is a broken inventory, not an absent one.
func openMCPStore(inventoryPath string) *inventory.InventoryStore {
	if inventoryPath == "" {
		fmt.Fprintln(os.Stderr, "warning: no SSH inventory found — serving with zero inventory hosts. Raw user@host targets and ~/.ssh/config aliases still work; set SHELLKIT_INVENTORY to serve named hosts.")
		return inventory.NewEmptyStore()
	}
	store, err := inventory.NewInventoryStore(inventoryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
		os.Exit(1)
	}
	if err := store.StartWatcher(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: file watcher failed: %v (continuing without live reload)\n", err)
	}
	return store
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
		store := openMCPStore(inventoryPath)
		if err := mcp.RunMCP(store); err != nil {
			fmt.Fprintf(os.Stderr, "mcp error: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		store := openMCPStore(inventoryPath)
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
		unknownVerb("mcp subcommand", subcmd, "shellkit mcp ", mcpSubcommands)
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

// sshMisuse reports why this `shellkit ssh` invocation looks like an agent
// shelling out instead of calling the MCP tool: a non-interactive console, or
// a trailing command that interactive ssh would silently drop. Empty string
// means the invocation is a normal interactive login.
func sshMisuse(rest []string) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "stdin is not a terminal"
	}
	if len(rest) > 1 {
		return fmt.Sprintf("trailing command %q would be silently dropped", strings.Join(rest[1:], " "))
	}
	return ""
}

// printSSHMisuse tells an agent how to redo the call through the shellkit MCP
// tool, echoing its intended command back as a ready-to-use DSL block.
func printSSHMisuse(reason, name string, cmdArgs []string) {
	body := "<your command here>"
	if len(cmdArgs) > 0 {
		body = strings.Join(cmdArgs, " ")
	}
	fmt.Fprintf(os.Stderr, `error: shellkit ssh is an interactive login command for humans (%s).

If you are an agent, do not shell out to shellkit — use the shellkit MCP tool:

  Tool name: mcp__shellkit__ssh
  If its schema is not loaded yet, load it first:
    ToolSearch query="select:mcp__shellkit__ssh"
  Then call it with input:

    ### run
    {"ssh": %q}

    %s

Daemon not running? Start it with: shellkit mcp start
`, reason, name, body)
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
