---
---
# tmux runtime — driving interactive remote sessions

The `tmux:` action drives a **persistent** remote tmux session through expect/send verbs. Use it when the workflow needs interactive session state: prompts to answer, TUI programs, long-running processes you return to across steps. For a one-shot command and its exit code, use a plain `ssh:` step instead.

Config: `{"tmux": "host:session"}` or `{"tmux": ["h:s1", "h:s2"]}` for fan-out. Host resolves with the same 3 layers as `ssh:` (inventory → `user@host:port` → ssh_config alias). The **first `:`** splits host from session, so `user@host:session` is unambiguous. Session names may contain `:` but **not `=`** (would corrupt `$OUTPUT` k=v parsing).

Sessions **persist across steps** — re-target the same `host:session` in a later step without re-spawning.

## Verbs

Each line in the body is one verb call.

| Verb | Does |
|---|---|
| `spawn CMD [ARGS...]` | Create/attach the session and run a command |
| `expect PATTERN [timeout=Ns]` | Wait for a regex match in the pane; **hard fail** on timeout (default 30s) |
| `expect? PATTERN [timeout=Ns]` | Soft expect — records match without failing; use for optional prompts / Y-n branching |
| `send "TEXT"` | Type text into the pane (escapes below) |
| `key NAME[,NAME...]` | Send tmux key name(s): `Enter`, `C-c`, `Tab`, `Escape`, `Up`, `F1`-`F12`, `C-a`..`C-z`, `M-a`..`M-z`, etc. |
| `snap [lines=N]` | Capture pane content (default 200 lines), stored base64 in `$OUTPUT` |
| `sleep N` | Pause N seconds between verbs |
| `kill` | Destroy the session — idempotent, no error if already gone |

**Patterns** are matched **per-line** via `grep -E` — no multi-line patterns.

**`send` escapes:** `\r` (CR), `\n` (LF), `\t` (tab), `\uHHHH` (Unicode), `\xHH` (hex byte), `\\` (literal backslash). Most prompts want `\r` to submit.

## Output keys

`N` = the 0-based position of the verb in the body.

| Key | Value |
|---|---|
| `snap.N` | Pane capture (base64) at verb position N |
| `match.N` | `"true"`/`"false"` for an `expect?` at position N |
| `expect_failed.N` | Pane snapshot (base64) when a hard `expect` times out at N |
| `expect_failed.N.pattern` | The pattern that failed to match |

For fan-out, downstream steps reference `{{step.host:session.outputs.snap.0}}`; `{{step.output}}` is the merged file across all hosts.

## Session state introspection

There is **no `has-session` verb**. To check whether a session is alive, use a plain `ssh:` step:

```
### alive
{"ssh": "host"}

tmux has-session -t '=mysession'   # exit 0 = alive
```

Pane-death detection: `tmux list-panes -t mysession -F '#{pane_dead}'`.

## Fan-out

Array target runs goroutine-parallel, capped at 32 concurrent. `continue_on_error: false` (default) cancels siblings on first failure; `true` runs all and aggregates. **ControlMaster** avoids a per-branch SSH handshake, so reused connections fan out much faster; hosts with `ControlMaster no` (e.g. the mutagen fleet) pay a full handshake per branch.

## Constraints

- **Linux + bash 4.4+ remote only** — macOS bash 3.2 is **not** supported as a remote target.
- **tmux 2.6+** on the remote (Ubuntu 18.04 floor).
- No PTY allocation (stdout/stderr kept separate for framing); no `pipe-pane` logging.
- No multi-line pattern matching.

## Patterns

**Drive Claude Code:**
```
spawn claude-code --project /path
expect "❯ "
send "review src/main.go\r"
expect "Done"
snap
kill
```

**Soft-expect Y/n branch:**
```
expect? "Continue? [Y/n]" timeout=5s
send "Y\r"
expect "Done"
```

**Fleet broadcast** — `{"tmux": ["worker-1:deploy","worker-2:deploy"], "continue_on_error": true}`:
```
spawn bash -c "./deploy.sh"
expect "Deploy complete" timeout=120s
snap
kill
```

## Gotcha — cancel orphans the remote session

Cancelling or timing out a `tmux:` step kills only the **local** ssh process. The **remote** tmux session and its verb interpreter keep running (a driven Claude Code session keeps burning resources). With no `-tt` PTY the local kill never sends SIGHUP to the remote, and the interpreter's `trap EXIT` cleanup fires only on a *normal* remote-bash exit. Always clean up explicitly:

```
ssh <host> "tmux kill-session -t '=<session>'"
```
