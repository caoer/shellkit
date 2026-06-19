# shellkit

An SSH server inventory and connectivity toolkit.

shellkit reads a Nix host inventory and gives you a fast table view, a TUI, an
SSH config generator, and an MCP `ssh` tool that runs step-based scripts on
remote hosts.

## Features

- Inventory table and interactive TUI for your SSH fleet
- Connectivity checker across every host and overlay address
- SSH config generation from a single source of truth
- MCP `ssh` tool — a step-based DSL for running scripts, fan-out, file
  transfer, and output chaining on remote hosts
- TUI log dashboard for MCP call history

## Installation

shellkit is a single Go binary. Build it with plain Go:

```sh
git clone https://github.com/caoer/shellkit.git
cd shellkit
go build -o shellkit ./cmd/shellkit
```

Or install straight from source:

```sh
go install github.com/caoer/shellkit/cmd/shellkit@latest
```

A [Nix](https://nixos.org/) flake is provided for a reproducible dev shell —
`nix develop` (or `direnv allow` with [direnv](https://direnv.net/)), then
`just build`.

> **Runtime requirement:** shellkit reads its inventory by shelling out to
> `nix eval`, so `nix` must be on `PATH` at runtime — even for a plain
> `go build`. See [Optional dependencies](#optional-dependencies) for the rest.

## Inventory

shellkit reads hosts from a Nix file that exposes a `hosts` attribute set. Each
host's fields map to addresses, login details, and metadata:

```nix
{
  hosts = {
    web-1 = {
      wan_ip = "203.0.113.10";
      user = "deploy";
      identity = "id_ed25519";
    };
    db-1 = {
      lan_ip = "10.0.0.20";
      user = "admin";
      proxy_jump = "web-1";
    };
  };
}
```

See [examples/inventory.sample.nix](examples/inventory.sample.nix) for the full
field reference.

Point shellkit at your inventory with a flag or an environment variable:

```sh
shellkit -f ./inventory.nix list
export SHELLKIT_INVENTORY=./inventory.nix
```

If neither is set, shellkit searches the current directory upward for
`shellkit.nix`, `inventory.nix`, `hosts.nix`, or `lib/ssh/hosts/default.nix`,
and fails with guidance when none is found.

## Usage

```sh
shellkit                    # interactive TUI (default)
shellkit list               # print the server table
shellkit list --json        # machine-readable output
shellkit check              # probe every server
shellkit ssh <name>         # SSH into a server by name or alias
shellkit generate-configs   # write an SSH config to $SHELLKIT_GENERATED_CONFIG_PATH or stdout
```

Filter to a subset with `--managed <value>`, and choose which address to
connect on with `--addr auto|wan|lan|wireguard|tailscale|easytier`.

On macOS with [OrbStack](https://orbstack.dev/) running, local OrbStack VMs are
auto-discovered and appear alongside your inventory hosts. `generate-configs`
also clears stale SSH `ControlMaster` sockets under `~/.ssh/sockets/` so the
regenerated config starts with fresh connections.

## MCP server

Run shellkit as an MCP server to expose the `ssh` tool to an agent:

```sh
shellkit mcp                 # stdio transport
shellkit mcp start           # background HTTP daemon
shellkit mcp status          # check the daemon
shellkit mcp stop            # stop the daemon
shellkit mcp restart         # restart the daemon
shellkit mcp log-dashboard   # TUI for MCP call logs
```

The `ssh` tool takes one or more steps. Each step has a name, an optional JSON
config that selects the action, and a script body. Later steps read earlier
steps' output:

```
### install
{"ssh": "web-1", "timeout": 120}

apt-get update && apt-get install -y nginx
echo "installed=true" >> $OUTPUT

### verify
{"ssh": "web-1"}

if [ {{install.outputs.installed}} = true ]; then
  systemctl is-active nginx
fi
```

Fan out with `{"ssh": ["web-1", "web-2"]}`, drive interactive programs with
`{"tmux": "host:session"}`, and run a body with no config to execute it locally.
Send `### help` to the tool for the full pipeline and tmux reference.

## Environment

| Variable | Purpose |
| --- | --- |
| `SHELLKIT_INVENTORY` | Inventory file path (`.nix` exposing a `hosts` attset) |
| `SHELLKIT_ADDR_PREF` | Default address preference (overridden by `--addr`) |
| `SHELLKIT_DEFAULT_IDENTITY` | Fallback SSH identity when a host sets none (default: `~/.ssh/id_ed25519`) |
| `SHELLKIT_GENERATED_CONFIG_PATH` | Destination for `generate-configs` |
| `SHELLKIT_MCP_PORT` | MCP HTTP port (default: 19222) |
| `SHELLKIT_MCP_TOKEN` | Bearer token for the MCP HTTP server |

## Optional dependencies

shellkit shells out to these when a feature needs them:

- `ssh` — system OpenSSH client (required for connections)
- `sops` — decrypt `password_ref` secrets
- `sshpass` — password-based SSH login
- `tmux` — drive interactive remote sessions

## Development

```sh
just build   # build the binary
just test    # run the Go test suite
just lint    # go vet + golangci-lint + shellcheck + nix flake check
just fmt     # format Go, shell, and nix sources
```

The Nix flake provides the toolchain (`nix develop` / `direnv allow`); entering
the dev shell installs a fast `lefthook` pre-commit hook (`gofmt` + `go vet`).
See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
