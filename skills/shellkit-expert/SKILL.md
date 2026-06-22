---
name: shellkit-expert
description: "Author and debug shellkit MCP `ssh` calls — the one-tool DSL that runs commands on remote hosts. Use for running scripts on a server, fan-out across hosts, file transfer (scp/rsync), capturing and chaining step output, raw or Daytona SSH targets, choosing which address (--addr) a host resolves on, timeout or 'unknown host' errors, or an interactive remote tmux session. The tool's own description is heavy and truncated in the tool list — load this for the full reference."
---

# shellkit-expert

The shellkit MCP server exposes **one tool** — `ssh` — that takes an HTTP-file-inspired DSL as its `input`. One tool keeps schema cost O(1) no matter how many capabilities it has (a 37-tool SSH server burned ~43.5k context tokens; this one stays flat). The cost: the tool description is dense and gets truncated in the tool list, so you may never see whole actions. This skill is the full reference.

## Block format

`input` is one or more blocks. Each block: a `###` name line, an optional JSON config line, a blank line, then the script body.

```
### step-name
{"ssh": "hostname"}

uname -a
df -h /
```

Steps run **sequentially**, top to bottom. Each step's output is captured and addressable by later steps. The JSON config's keys decide what the step *does* — there is no separate "action" field.

## Actions — chosen by config keys

| Config | Action |
|---|---|
| `{"ssh": "host"}` + body | Run body on a remote host |
| `{"ssh": ["h1","h2"]}` + body | Fan out — same body on every host, in parallel |
| `{"ssh": "user@host:port"}` + body | Run on a raw SSH target (no inventory/ssh_config entry needed) |
| `{"ssh": "host", "jump": "proxy"}` + body | Hop through a jump host (ProxyJump) — works with any host type and fan-out |
| `{"ssh": "host", "identity": "~/.ssh/key"}` + body | Use a specific SSH key — sets IdentitiesOnly; works with any host type, fan-out, and tmux |
| body only, no config | Run **locally** (post-process output, scp/rsync, compare results) |
| `{"list": true}` | List inventory hosts (`"filter": "k=v"` to narrow) |
| `{"tmux": "host:session"}` + body | Drive an interactive remote tmux session — see `references/tmux-runtime.md` |
| no config, no body (`### help`) | Print the tool's advanced-examples help |

`entrypoint` (default `bash`) picks the interpreter: `bash, sh, zsh, python3, python, node, deno, bun, ruby, perl`. Set it explicitly when the body isn't bash.

## Host resolution — 3 layers, fall through on miss

A host string in `ssh:` / `tmux:` is resolved in order:

1. **Inventory** — nix host registry (`lib/ssh/hosts/`) by name. Wins when present. Only inventory hosts get `{{host.<field>}}` template lookups and `--addr` preference routing.
2. **Raw SSH target** — `user@host[:port]` runs ssh directly. For ad-hoc cloud sandboxes with single-use creds (Daytona: `token@ssh.app.daytona.io`).
3. **ssh_config alias** — a bare name passing `[a-zA-Z0-9._-]+` is handed to `ssh` as-is; `~/.ssh/config` resolves user/port/identity/ProxyJump.

No match → `unknown host`. Layer 3 has no pre-flight check, so a typo surfaces as ssh's "could not resolve hostname", not shellkit's error. Layers 2 and 3 get no `{{host.<field>}}` lookup and no `--addr` routing — there's no inventory record to read. For repeated scripted use, add an inventory entry. Address/port selection once a host *is* named (`--addr`, NAT-port rules, mesh-vs-public) lives in `references/resolution.md`.

## Capturing and chaining output

Two mechanisms, both modeled on GitHub Actions:

- **`$OUTPUT`** — append `key=value` lines to the `$OUTPUT` file to export values from a step. Works in any entrypoint (it's just a file path in the env).
  ```
  echo "version=$(nginx -v 2>&1 | cut -d/ -f2)" >> $OUTPUT
  ```
- **`{{...}}` references** — pull from prior steps or inventory into a later step's body. **Literal substitution** (Ansible/GHA convention): the value is inlined verbatim, so *you* shell-quote it if the body needs quoting.
  | Form | Resolves to |
  |---|---|
  | `{{step.outputs.key}}` | A `key=value` you wrote to `$OUTPUT` in `step` |
  | `{{step.output}}` | Path to `step`'s full output file (auto-uploaded if the consuming step is remote) |
  | `{{host.wan_ip}}` | An inventory host's address. Valid fields: `wan_ip`, `lan_ip`, `wireguard_ip`, `tailscale_ip`, `easytier_ip`, `user`, `port` (inventory hosts only — there is **no** `ip` field) |

There are **no user-defined variables** — inline values directly; `{{}}` is only for runtime lookups.

## File transfer — local scp/rsync blocks

There is **no `push:`/`pull:` key** — an older pattern that no longer exists. Transfer a file with an ordinary local block (no config) that calls `scp` or `rsync`. Hosts resolve via `~/.ssh/config` (run `shellkit generate-configs` to populate it from the host registry).

```
### upload
scp ./app.tar.gz dev-nyc-2:/tmp/
```

## Errors, timeouts, and trace

- `timeout` (seconds, default 360) kills a step. On a bash timeout the result includes a **command trace** — every command with elapsed time, so you see exactly where it stalled. On by default for `bash`; disable with `"trace": false`. Not available for non-bash entrypoints (POSIX `sh` lacks the DEBUG trap).
- **Fan-out** defaults to `continue_on_error: false` — first host failure cancels the rest. Set `true` to run all and aggregate errors.
- **Long, silent steps stay connected**: MCP's HTTP transport idles out after 60s of no wire data, but the daemon sends a keepalive progress notification every 3s *regardless of command output* — so a step that prints nothing for minutes is fine. (`exit_code: -1, timed_out: false` would mean the wire was cut despite keepalive — just rerun.)

## Daemon

```bash
shellkit mcp start | stop | restart | status
```

> Inventory **source**: `$SHELLKIT_INVENTORY` if set, else `lib/ssh/hosts/default.nix` discovered by walking up from the cwd (the osfiles checkout). It **live-reloads** — the daemon watches that registry via fsnotify and picks up any `.nix` change automatically, no restart needed after adding a host. A freshly provisioned VM resolves once it's written into the registry; until then, reach it as a raw `user@host` target.

## Common patterns

**Run + verify across steps:**
```
### install
{"ssh": "dev-nyc-2", "timeout": 120}

apt-get update && apt-get install -y nginx
echo "ok=$(systemctl is-active nginx)" >> $OUTPUT

### check
{"ssh": "dev-nyc-2"}

[ {{install.outputs.ok}} = active ] && echo "up"
```

**Fan out, then post-process locally:**
```
### kernels
{"ssh": ["dev-nyc-2", "dev-nyc-3", "dev-sfo-1"]}

echo "kernel=$(uname -r)" >> $OUTPUT

### compare
cat {{kernels.output}}
```

**Reach a host behind a jump host (ProxyJump):**
```
### check-internal
{"ssh": "root@192.168.1.100", "jump": "bastion-host"}

uname -a && df -h /
```

The `"jump"` field works with any host type — inventory names, raw `user@host:port` targets, or ssh_config aliases. It also works with fan-out (all targets hop through the same proxy) and tmux steps. The proxy authenticates via your ssh_config / ssh-agent; the target uses its own auth (key or password).

**Use a specific SSH key (identity):**
```
### connect
{"ssh": "admin@192.168.80.1:2213", "identity": "~/.ssh/mac_m4_ed25519"}

uname -a
```

The `"identity"` field overrides the server's key and sets `IdentitiesOnly`, so ssh uses only that key. Works with any host type, fan-out, and tmux steps. Paths with `~/` expand to `$HOME`; bare names (no `/`) resolve to `~/.ssh/keys/<name>` (inventory convention).

Multi-step conditional chaining, cross-host coordination (start service on A, benchmark from B, clean up on A), and non-bash `$OUTPUT` examples are in `references/advanced-pipelines.md`.

## Gotchas

- **`{{}}` is literal, not shell-escaped** — the value is inlined verbatim, so quote substituted values yourself if they may contain spaces or special characters.
- **No `{"check"}` action** — the connectivity probe was removed. Verify by running your check through a plain `ssh:` step (e.g. `{"ssh": "host"}` running `true`); a `check:` config now errors with that instruction.
- **`unknown host`** means the name matched no inventory entry, raw `user@host` target, *or* ssh_config alias — it is **not** a stale cache (inventory live-reloads). Add it to the registry, or use a raw target.
- **Raw targets / ssh_config aliases** get no `{{host.<field>}}` lookup and no `--addr`.
- **tmux cancel orphans the remote session** — cancelling/timing out a `tmux:` step kills only the local ssh; clean up with `ssh <host> "tmux kill-session -t '=<session>'"`. Details in `references/tmux-runtime.md`.
- **macOS not supported as a remote tmux target** (bash 3.2).

## References

| File | Read when |
|---|---|
| `references/tmux-runtime.md` | Driving an interactive remote session — verbs, fan-out, cleanup |
| `references/resolution.md` | Tuning which address/port a named host connects on (`--addr`, NAT, mesh) |
| `references/advanced-pipelines.md` | Multi-step conditionals, cross-host coordination, non-bash entrypoints |
