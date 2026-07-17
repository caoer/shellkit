---
name: shellkit-expert
description: "Author and debug shellkit MCP `ssh` calls — the one-tool DSL that runs commands on remote hosts. Use for running scripts on a server, fan-out across hosts, file transfer (scp/rsync), SSH key/identity selection, ProxyJump through a bastion, password auth for inventory hosts, targeting hosts by name or IP, capturing and chaining step output, choosing which address (--addr) a host resolves on, timeout or 'unknown host' errors, or an interactive remote tmux session. The tool's own description is heavy and truncated in the tool list — load this for the full reference."
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
| `{"ssh": ["h1","h2"]}` + body | Fan out — same body on every host, **sequentially, one host at a time** (concurrent fan-out is a named future upgrade, not shipped) |
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
  | `{{step.output}}` | Path to `step`'s full output file — always a **local** (daemon-side) path. Only useful when the *consuming* step is itself local (no config); a remote consuming step does **not** get the file staged onto its host (the protocol plumbing for that exists but isn't wired yet — named follow-up). `scp`/`rsync` the file yourself in a local block first if a remote step needs the bytes. |
  | `{{host.wan_ip}}` | An inventory host's address. Valid fields: `wan_ip`, `lan_ip`, `wireguard_ip`, `tailscale_ip`, `easytier_ip`, `user`, `port` (inventory hosts only — there is **no** `ip` field) |

There are **no user-defined variables** — inline values directly; `{{}}` is only for runtime lookups.

## File transfer — local scp/rsync blocks

There is **no `push:`/`pull:` key** — an older pattern that no longer exists. Transfer a file with an ordinary local block (no config) that calls `scp` or `rsync`. Hosts resolve via `~/.ssh/config` (run `shellkit generate-configs` to populate it from the host registry).

```
### upload
scp ./app.tar.gz dev-nyc-2:/tmp/
```

## Two execution paths: runner vs legacy

Every `ssh:` step runs one of two ways (the runner path is `ssh:`-only — `tmux:` steps always
use the legacy real-bash path below, never the runner):

- **The runner (mvdan/sh)** — a small Go binary pushed to the host once and cached there, that
  parses and interprets the script in-process. External commands (`git`, `systemctl`, `ssh`, ...)
  still spawn as real subprocesses; only the shell *glue* (loops, pipelines, variables, builtins)
  runs through the interpreter. This is what gives ns-precision per-command timing, a real
  per-command exit code, and a genuine process-group kill on timeout — the remote command is
  confirmed **gone**, not just disconnected from. That kill guarantee is exec-path only; it does
  not cover `tmux:` steps (see Gotchas).
- **Legacy (real bash + trap)** — today's path: the body ships to the remote host's own `bash`,
  wrapped in a `DEBUG`-trap tracer. Always available, byte-identical to what shipped before the
  runner existed.

### The `interp` flag

```
{"ssh": "host", "interp": true}   // use the runner for this step
{"ssh": "host", "interp": false}  // force legacy, even for a script the runner could handle
```

**Current posture: default-on.** The runner cleared its go/no-go gate (a bash-vs-runner
differential corpus, 0 unexplained divergences) and the flip landed 2026-07-17: an absent `interp`
field engages the runner for statically-screened scripts, with gap constructs and silent-divergence
idioms still auto-routed to legacy real bash. `"interp": false` is the per-step escape hatch that
forces legacy; `"interp": true` remains valid (explicit request, same routing rules). Don't assume
either posture from this doc alone — `runnerDefaultOn` in `internal/mcp/mcp_exec.go` is the live
source of truth.

**Environment contract (runner path).** A step run under the runner does **not** inherit the full
remote login environment. It gets a fixed operational allowlist — `PATH HOME USER LOGNAME SHELL
LANG LC_ALL LC_CTYPE TERM TZ` plus `OUTPUT` — and nothing else (a deliberate security boundary,
so no forwarded agent socket or stray secret leaks into a fleet runner). The legacy path inherits
the whole remote session env. If a script depends on some other remote-session variable, either
export it inside the body or run that step with `"interp": false`.

### When a step falls back to legacy anyway

`"interp": true` is a request, not a guarantee. Three things route a step to legacy regardless,
each surfaced as a `note:` line right after the step's header:

1. **Gap constructs** — job control (`jobs`/`fg`/`bg`/...), `kill`, `ulimit`, `trap` with a named
   signal other than EXIT/ERR, and a handful of other builtins/flags the runner doesn't implement.
   Detected by parsing the script before ever connecting, e.g.:
   ```
   note: job-control builtin `jobs` is not implemented by interp; running under real bash
   ```
2. **Silent-divergence idioms** — constructs the runner *can* execute but would produce a subtly
   wrong result for (pipeline ending in a mutating builtin, `exec` with a redirect and no command,
   `$$`/`$!` in a word, `trap … EXIT` combined with `exec`, an `IFS=` reassignment feeding a
   word-split loop). Screened the same way, also with a `note:`, e.g.:
   ```
   note: pipeline's final stage runs the state-mutating builtin `cd` — interp can leak its cwd/env
   into the parent (bash isolates it in a subshell); running under real bash
   ```
   ```
   note: `exec` with a redirect and no command reconfigures shell file descriptors — interp
   diverges (output can be discarded); running under real bash
   ```
3. **Bootstrap failure** — no usable runner binary on the host; see `references/runner-execution.md`
   for the cache chain and diagnostics. Falls back with a `note:` naming the cause, e.g.:
   ```
   note: runner bootstrap failed on host-b (no writable+executable cache dir: ~/.cache,
   $XDG_CACHE_HOME, /var/tmp, /dev/shm all unusable — noexec or read-only home) — ran under legacy path
   ```

A construct that evades all three (e.g. built dynamically via `eval`/`"$cmd"`) and only fails once
the runner is actually executing it renders as a plain `error:` line instead — and is **not**
retried under legacy, since the body may have partially run and the script might not be idempotent.
Add `"interp": false` up front for a step you know needs that escape hatch. Two divergence classes
can't be screened at all and are documented known limitations — see
`references/runner-execution.md`.

## Command trace

- `timeout` (seconds, default 360) kills a step. On timeout, or with `"trace": true`, the result
  includes a **command trace** — every command with elapsed time, so you see exactly where it
  stalled or spent its time. Not available for non-bash entrypoints (POSIX `sh` and friends have no
  DEBUG trap; the runner's non-bash mode times the whole step, not per command).
- **Legacy path**: whole-second resolution (bash's own `$SECONDS`) — every command inside the same
  second renders as `+0s`, and the *last* command in a non-timeout trace never gets a duration (no
  next trap firing left to diff against):
  ```
  command trace:
    +0s  cd /srv/app
    +0s  git pull --ff-only
    +0s  echo "deployed" >> $OUTPUT
  ```
- **Runner path**: nanosecond-precision, self-scaling (ns/µs/ms/s), each command's *own* measured
  duration — including the last one — and an inline `exit:N` only when a command's own exit was
  non-zero (invisible on the legacy path, which has no per-command exit capture). Real capture from
  a mixed builtin+external step (`sleep`, `cd`, `pwd`, `dd`):
  ```
  command trace:
    +166.9ms  sleep 1.2 (1.204s)
    +1.371s   cd /tmp
    +1.371s   pwd
    +1.371s   dd if=/dev/zero of=/dev/null bs=1M count=200 (7.1ms)
    +1.378s   echo mix-done
  ```
- **Mixed fan-out**: each host renders its own trace shape independently — one host on the runner
  (ns-precision) and another that fell back to legacy (whole-second) can appear in the same result,
  each under its own `=== name [host] exit:N ===` block. The precision difference is itself a
  visual tell for which path a host took, without needing to read the `note:` line.
- Caps are unchanged either way: 20 trace lines shown by default, 50 with `"trace": true`; each
  command truncated to 500 characters.

## Timeouts, cancellation, and fan-out defaults

- **Fan-out** defaults to `continue_on_error: false` — first host failure cancels the rest. Set
  `true` to run all and aggregate errors.
- **Long, silent steps stay connected**: MCP's HTTP transport idles out after 60s of no wire data,
  but the daemon sends a keepalive progress notification every 3s *regardless of command output* —
  so a step that prints nothing for minutes is fine. (`exit_code: -1, timed_out: false` would mean
  the wire was cut despite keepalive — just rerun.)

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

## Auth — key or password, handled by the daemon

Shellkit handles both key and password auth. **Always use shellkit** — don't fall back to Bash `ssh` or `sshpass` for auth reasons.

- **Key auth** — default. Via ssh-agent, `~/.ssh/config`, or the `"identity"` config field.
- **Password auth** — automatic for inventory hosts that carry stored credentials. No DSL config needed; the daemon resolves credentials at connection time. Hosts with both a deployed key and stored credentials try key first — password is the fallback, never the blocker.

The DSL call is the same either way: `{"ssh": "host"}`. The auth method is a property of the host, not the step.

Multi-step conditional chaining, cross-host coordination (start service on A, benchmark from B, clean up on A), and non-bash `$OUTPUT` examples are in `references/advanced-pipelines.md`.

## Gotchas

- **`{{}}` is literal, not shell-escaped** — the value is inlined verbatim, so quote substituted values yourself if they may contain spaces or special characters.
- **No `{"check"}` action** — the connectivity probe was removed. Verify by running your check through a plain `ssh:` step (e.g. `{"ssh": "host"}` running `true`); a `check:` config now errors with that instruction.
- **`unknown host`** means the name matched no inventory entry, raw `user@host` target, *or* ssh_config alias — it is **not** a stale cache (inventory live-reloads). Add it to the registry, or use a raw target.
- **Raw targets / ssh_config aliases** get no `{{host.<field>}}` lookup and no `--addr`.
- **`{{step.output}}` never stages onto a remote host** — it's always a local, daemon-side path;
  only a *local* consuming step (no config) can read it directly. A remote step needs you to
  `scp`/`rsync` the file yourself first.
- **Clean-kill on timeout is exec-path only** — a timed-out/cancelled `ssh:` step has its whole
  process group killed and confirmed gone on the remote host. This guarantee does not extend to
  `tmux:` steps — see the next gotcha, a separate, still-open issue.
- **tmux cancel orphans the remote session** — cancelling/timing out a `tmux:` step kills only the local ssh; clean up with `ssh <host> "tmux kill-session -t '=<session>'"`. Details in `references/tmux-runtime.md`.
- **macOS not supported as a remote tmux target** (bash 3.2).
- **Legacy path: `exec >logfile` can leak one trace-marker line into your log** — the DEBUG-trap
  tracer keeps firing after a script redirects its own stdout via `exec >log`, so the last
  command's marker line (`SHELLKIT_CMD_<nonce> ...`) can land inside the redirected file itself.
  Pre-existing wart of the trap-based tracer, same on any host; the runner path is structurally
  immune (no trap, no marker) — but a script using this exact idiom is auto-screened *away* from
  the runner (to preserve `exec`'s real fd semantics, see above), so it's exactly the script most
  likely to hit this wart, not the fix.

## References

| File | Read when |
|---|---|
| `references/tmux-runtime.md` | Driving an interactive remote session — verbs, fan-out, cleanup |
| `references/resolution.md` | Tuning which address/port a named host connects on (`--addr`, NAT, mesh) |
| `references/advanced-pipelines.md` | Multi-step conditionals, cross-host coordination, non-bash entrypoints |
| `references/runner-execution.md` | Runner bootstrap/cache internals, version-mismatch diagnostics, and the documented residual silent-divergence classes |
