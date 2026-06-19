---
type: mcp-test
status: active
created: 2026-04-27
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T12: Tmux Action

Drive remote tmux sessions via the verb DSL (`spawn`, `expect`, `send`, `key`, `snap`, `kill`, `sleep`).

**Prerequisites:** Orb VM `orb-mcp-test-a` with tmux ≥ 2.6 and bash ≥ 4.4 installed. Manual provisioning if absent.

---

## T12.1: Basic Spawn + Expect + Snap + Kill

**DSL:**
```
### tmux-basic
{"tmux": "orb-mcp-test-a:basic-test"}

spawn bash
expect "\\$ "
send "echo hello\r"
expect "hello"
snap
kill
```

**Expected:** Session `basic-test` created, prompt detected, `echo hello` sent, output captured. `snap.4` contains base64-encoded pane content; decoded content includes `hello`. Step exits 0.

**Verify:**
- `snap.4` key present in $OUTPUT (snap is verb at position 4)
- Base64-decode of `snap.4` contains `hello`
- Step exit code 0

- [ ] Pass

---

## T12.2: Send with Binary/Special Payload

**DSL:**
```
### tmux-binary-send
{"tmux": "orb-mcp-test-a:binary-test"}

spawn bash
expect "\\$ "
send "line1\tvalue1\nline2\tvalue2\r"
sleep 1
snap
kill
```

**Expected:** Multi-line text with embedded tabs sent via `load-buffer`/`paste-buffer` pipeline. Snap captures pane showing both lines with tab-separated values.

**Verify:**
- `snap.4` present in $OUTPUT
- Decoded snap contains `line1` and `line2` (tabs rendered as whitespace in pane)
- No corruption from special characters in the load-buffer pathway
- Step exit code 0

- [ ] Pass

---

## T12.3: Soft-Expect No-Match

**DSL:**
```
### tmux-soft-expect
{"tmux": "orb-mcp-test-a:soft-test"}

spawn bash
expect "\\$ "
expect? "WILLNEVERMATCH" timeout=2s
kill
```

**Expected:** Soft-expect (`expect?`) polls for 2 seconds, pattern never found. Step succeeds (soft-expect does not fail). `match.2=false` written to $OUTPUT.

**Verify:**
- `match.2=false` in $OUTPUT (expect? is verb at position 2)
- Step exit code 0 (soft-expect timeout is not an error)

- [ ] Pass

---

## T12.4: Hard-Expect Timeout

**DSL:**
```
### tmux-hard-expect
{"tmux": "orb-mcp-test-a:hard-test"}

spawn bash
expect "\\$ "
expect "WILLNEVERMATCH" timeout=1s
kill
```

**Expected:** Hard `expect` polls for 1 second, pattern never found. Step fails. Before exiting non-zero, interpreter writes pane snapshot and pattern to $OUTPUT for agent context (D-16).

**Verify:**
- `expect_failed.2` present in $OUTPUT (base64-encoded pane content)
- `expect_failed.2.pattern=WILLNEVERMATCH` present in $OUTPUT
- Step exit code non-zero
- `kill` verb does NOT execute (set -e stops on expect failure)

- [ ] Pass

---

## T12.5a: Fan-Out + continue_on_error:false

**DSL:**
```
### tmux-fanout-abort
{"tmux": ["orb-mcp-test-a:s1", "nonexistent-host:s2"], "continue_on_error": false}

spawn bash
sleep 5
snap
kill
```

**Expected:** Two goroutines launch. `nonexistent-host` fails SSH immediately. With `continue_on_error: false`, first error sets shared cancel context. The surviving target (`orb-mcp-test-a:s1`) should NOT complete its full verb stream — the `sleep 5` + `snap` should be cancelled by the context propagated from the failed target.

**Verify:**
- Step exit code non-zero
- `orb-mcp-test-a:s1` results incomplete — no `orb-mcp-test-a:s1.snap.2` in $OUTPUT (cancelled before snap)
- `nonexistent-host:s2` has SSH connection error in results
- Pipeline does NOT continue to subsequent steps

- [ ] Pass

---

## T12.5b: Fan-Out + continue_on_error:true

**DSL:**
```
### tmux-fanout-continue
{"tmux": ["orb-mcp-test-a:s1", "nonexistent-host:s2"], "continue_on_error": true}

spawn bash
expect "\\$ "
send "echo fanout-ok\r"
expect "fanout-ok"
snap
kill
```

**Expected:** Both goroutines run independently. `nonexistent-host:s2` fails SSH immediately. `orb-mcp-test-a:s1` completes normally because `continue_on_error: true` does NOT cancel siblings on failure. Both results aggregated.

**Verify:**
- `orb-mcp-test-a:s1.snap.4` present in $OUTPUT, decoded content contains `fanout-ok`
- `nonexistent-host:s2` has error in results (SSH connection failure)
- Pipeline continues to subsequent steps

- [ ] Pass

---

## T12.6: Cross-Step Session Continuity

Step A spawns a session. Step B reuses it (no spawn needed — session already exists). Step C snaps and kills.

**DSL:**
```
### tmux-persist-spawn
{"tmux": "orb-mcp-test-a:persist"}

spawn bash
expect "\\$ "
send "export PERSIST_VAR=cross_step_ok\r"
expect "\\$ "

### tmux-persist-use
{"tmux": "orb-mcp-test-a:persist"}

send "echo $PERSIST_VAR\r"
expect "cross_step_ok"

### tmux-persist-cleanup
{"tmux": "orb-mcp-test-a:persist"}

snap
kill
```

**Expected:** Session `:persist` survives across steps. Step B sends to existing session without spawning. Environment variable set in step A visible in step B. Step C captures final pane state and kills.

**Verify:**
- Step A exit code 0
- Step B exit code 0 (expect finds `cross_step_ok` — env var persisted)
- `tmux-persist-cleanup.snap.0` present, decoded content includes `cross_step_ok`
- Session `:persist` no longer exists after step C

- [ ] Pass

---

## T12.7: Idempotent Attach

Spawn against an already-running session. `tmux new-session -d -A` should attach idempotently.

**DSL:**
```
### tmux-idem-attach
{"tmux": "orb-mcp-test-a:idem-sess"}

spawn bash
expect "\\$ "
spawn bash
send "echo idem-ok\r"
expect "idem-ok"
snap
kill
```

**Expected:** First `spawn` creates session. Second `spawn` attaches to existing (no error). Subsequent verbs operate on the same session.

**Verify:**
- Step exit code 0 (second spawn does not fail)
- `snap.5` decoded content contains `idem-ok`
- Only one tmux session named `idem-sess` existed (not two)

- [ ] Pass

---

## T12.8: Idempotent Kill

Kill a session that doesn't exist. Per D-19, `tmux kill-session` failure is suppressed.

**DSL:**
```
### tmux-idem-kill
{"tmux": "orb-mcp-test-a:nonexistent-session"}

kill
```

**Expected:** No tmux session `nonexistent-session` exists. `kill` runs `tmux kill-session -t "=nonexistent-session" 2>/dev/null || true`. Step succeeds.

**Verify:**
- Step exit code 0
- No error output

- [ ] Pass

---

## T12.9: Trace Mode + Tmux

Verify DEBUG trap is disabled inside the interpreter (D-4). Trace output appears in the trace marker section around the SSH invocation, NOT inside step stdout or $OUTPUT verb results.

**DSL:**
```
### tmux-trace
{"tmux": "orb-mcp-test-a:trace-test", "trace": true}

spawn bash
expect "\\$ "
send "echo traced-hello\r"
expect "traced-hello"
snap
kill
```

**Expected:** Trace mode active. DEBUG trap runs around the SSH command (timing the overall invocation). Inside the interpreter, `set +o functrace` and `trap - DEBUG` prevent the trap from firing during verb execution.

**Verify:**
- Step exit code 0
- `snap.4` decoded content contains `traced-hello`
- Trace marker section present in result (shows SSH invocation timing)
- Verb wire-form lines (`spawn_b64`, `send_b64`, `expect`, `snap`, `kill`) do NOT appear in trace output (D-4: interpreter disables DEBUG)
- $OUTPUT contains only structured keys (`snap.4=...`), no trace leakage

- [ ] Pass

---

## T12.10: Fan-Out 2 Sessions on Same Host

Two targets on the same host, different session names. Both run in parallel.

**DSL:**
```
### tmux-same-host-fanout
{"tmux": ["orb-mcp-test-a:s1", "orb-mcp-test-a:s2"]}

spawn bash
expect "\\$ "
send "echo session-$(tmux display-message -p '#S')\r"
expect "session-"
snap
kill
```

**Expected:** Two goroutines, both SSH to `orb-mcp-test-a`, each creating a distinct tmux session. Results keyed by full target string.

**Verify:**
- `orb-mcp-test-a:s1.snap.4` present in $OUTPUT
- `orb-mcp-test-a:s2.snap.4` present in $OUTPUT
- Decoded snap for s1 contains `session-s1`
- Decoded snap for s2 contains `session-s2`
- Both sessions completed (parallel execution, not serialized)
- Step exit code 0

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T12.1 | Basic spawn+expect+snap+kill | |
| T12.2 | Binary/special send payload | |
| T12.3 | Soft-expect no-match | |
| T12.4 | Hard-expect timeout | |
| T12.5a | Fan-out + continue_on_error:false | |
| T12.5b | Fan-out + continue_on_error:true | |
| T12.6 | Cross-step session continuity | |
| T12.7 | Idempotent attach | |
| T12.8 | Idempotent kill | |
| T12.9 | Trace mode + tmux | |
| T12.10 | Fan-out same host, 2 sessions | |
