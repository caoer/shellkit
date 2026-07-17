---
type: mcp-test
status: active
created: 2026-07-16
target: [host-a, host-b]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T13: Runner E2E (mvdan/sh execution core)

Live end-to-end protocol for the `shellkit-runner` execution path (U9, decision #16c gate).
Exercises the runner against real hosts through the MCP `ssh` tool. While the runner is
opt-in, every step here carries `"interp": true`; after the default-on flip, the same steps
run without it and `"interp": false` forces the legacy path.

Run each scenario on BOTH target hosts (`host-a`, `host-b` are synthetic placeholders —
substitute your own two hosts per CONTRIBUTING.md's "never commit real infrastructure
details"). First execution evidence (2026-07-16, two live linux hosts, runner version
`<ver>`) lives in the session dir, not this repo:
`16-17-shellkit-upgrade/ccc-compound/evidence/u9-live/`.

---

## T13.1: Bootstrap cold → warm

Wipe the remote cache first: `ssh <host> 'rm -rf ~/.cache/shellkit'`.

**DSL:**
```
### boot-check
{"ssh": "<host>", "interp": true, "trace": true}

echo "cold bootstrap run on $(hostname)"
ls -la ~/.cache/shellkit/
```

**Expected (cold):** step succeeds; `~/.cache/shellkit/runner-<ver>-linux-amd64` listed
(pushed this run); on a slow link the ticker shows `phase:bootstrap`. **Expected (warm,
rerun):** no re-push, faster wall clock, `<runner> --version` inside the step prints the
daemon's `RunnerVersion`. Note: `phase:bootstrap` also covers ssh connect + hello handshake
(~seconds on WAN), so warm steps may briefly show it — the push itself only happens cold.

- [x] Pass (both hosts, 2026-07-16: cold pushed the ~3.7MB `runner-<ver>-linux-amd64`;
  warm reported the same `<ver>` on-host)

---

## T13.2: Multi-step $OUTPUT + {{refs}} chain

**DSL:**
```
### stepA
{"ssh": "<host>", "interp": true, "trace": true}

kver=$(uname -r)
echo "kernel=$kver" >> $OUTPUT
echo "marker=chain-ok" >> $OUTPUT

### stepB
{"ssh": "<host>", "interp": true, "trace": true}

test "{{stepA.outputs.marker}}" = "chain-ok" && echo "REF-RESOLVED-OK"
```

**Expected:** stepA's `$OUTPUT` k=v pairs render under its result block (collected via
runner output frames); stepB's body resolves the refs and prints `REF-RESOLVED-OK`.

- [x] Pass (both hosts)

---

## T13.3: Trace stream vs wall clock (ns precision)

**DSL:**
```
### timing
{"ssh": "<host>", "interp": true, "trace": true}

sleep 1.2
cd /tmp
pwd
dd if=/dev/zero of=/dev/null bs=1M count=200 2>/dev/null
echo mix-done
```

**Expected:** per-U0 rendering — `sleep 1.2` shows a measured duration ≈1.2s; externals
(`sleep`, `dd`) carry `(dur)` suffixes down to µs; builtins (`cd`, `pwd`, `echo`) render
without a duration suffix (CallHandler observes them but has no wait-status); offsets
`+<t>` are ns-scaled, not `+0s`; the whole-step wall clock is consistent with the trace.

- [x] Pass (host-a: `sleep 1.2 (1.204s)`; host-b: `(1.363s)`)

---

## T13.4: Timeout kill + remote-process-gone

**DSL:**
```
### timeout-kill
{"ssh": "<host>", "interp": true, "trace": true, "timeout": 5}

sleep 6001 &
echo "child spawned, now blocking"
sleep 6000
```

**Expected:** exit **137**, `TIMED OUT (after 5s)`, trace to the stall point with
`← timed out here`. Then, via a SEPARATE ssh: `pgrep -af 'sleep 600[01]'` is EMPTY —
both the foreground command AND the backgrounded child are gone (U4 pgroup TERM→KILL
over real ssh). Use a self-match-proof pgrep pattern (`600[01]`), or pgrep matches its
own login shell.

- [x] Pass (both hosts; remote pgrep empty immediately after)

---

## T13.5: Cancellation mid-step

Run the T13.4 body WITHOUT `timeout`, then cancel the MCP call mid-step (client
disconnect or MCP cancellation).

**Expected:** daemon tears down the ssh transport; the runner's stdin-EOF watchdog kills
the process groups; a separate ssh finds no `sleep` survivors; no lingering daemon-side
ssh subprocess.

- [x] Pass (both hosts; client disconnect at 8s, remote pgrep empty)

---

## T13.6: Gap-route fallback (loud gap)

**DSL:**
```
### jobs-gap
{"ssh": "<host>", "interp": true, "trace": true}

sleep 0.2 &
jobs
echo "jobs-step-done"
```

**Expected:** routed to the legacy path BEFORE any ssh:
`note: job-control builtin `jobs` is not implemented by interp; running under real bash`.
Output is real bash's (`[1]+ Running …`); trace is legacy whole-second `+0s` style.

- [x] Pass (both hosts)

---

## T13.7: Silent-divergence screen (decision #16b)

**DSL:**
```
### pipe-cd
{"ssh": "<host>", "interp": true, "trace": true}

cd /
true | cd /tmp
pwd

### exec-redirect
{"ssh": "<host>", "interp": true, "trace": true}

rm -f /tmp/u9-exec-redirect.log
exec >/tmp/u9-exec-redirect.log
echo "redirected line landed in log"
```

**Expected:** both screened to legacy with notes (pipeline-final mutating builtin;
`exec` redirect-without-command). `pipe-cd` prints `/` (bash subshell semantics
preserved — interp would have leaked cwd). `exec-redirect` puts the line in the remote
log, step stdout empty.

**Known legacy-path wart (pre-existing, captured verbatim):** with trace on, the legacy
DEBUG-trap marker line (`SHELLKIT_CMD_<nonce> …`) lands INSIDE the user's redirected log
file after `exec >log` — trap scaffolding pollutes user-redirected output. The runner
path is structurally immune (no in-band markers), but screened scripts route to legacy
where this persists. Documented for U10.

- [x] Pass (both hosts; wart observed identically on both)

---

## T13.8: Fan-out + mixed-mode rendering

**DSL:**
```
### fanout
{"ssh": ["host-a", "host-b"], "interp": true, "trace": true}

echo "host=$(hostname)" >> $OUTPUT
uname -r
echo "fan-out step done on $(hostname)"
```

**Expected (both warm):** per-host result blocks with independent ns traces + per-host
`$OUTPUT`; merged `[local]` block concatenates stdout per host, unchanged.

**Mixed-mode:** force total bootstrap failure on ONE host (no root needed — replace every
candidate with a plain file so `mkdir -p` fails:
`ssh <host> 'rm -rf ~/.cache/shellkit; touch ~/.cache/shellkit /var/tmp/shellkit /dev/shm/shellkit'`),
rerun. Expected: that host renders
`note: runner bootstrap failed on <host> (no writable+executable cache dir: …) — ran under
legacy path` with a `+0s` legacy trace, while the other host keeps its ns runner trace;
stdout identical on both; merged block unchanged. Restore by removing the three files.

- [x] Pass (2026-07-16: host-a runner + host-b legacy in one call, exact U0 §2 shape)

---

## T13.9: Non-bash entrypoint through the runner

**DSL:**
```
### py-step
{"ssh": "<host>", "interp": true, "trace": true, "entrypoint": "python3"}

import os, platform
with open(os.environ["OUTPUT"], "a") as f:
    f.write("pykey=pyval\n")
print("python on", platform.node())
```

**Expected:** runs as a supervised runner subprocess (whole-step timing, no per-command
trace block); `$OUTPUT` reaches the subprocess via the closed `{OUTPUT}` env set
(decision #17) and k=v pairs render; exit code is python's.

**History:** first U9 execution found the U6b route gated on `entrypoint == "bash"`,
leaving non-bash steps on the legacy ssh path — which is broken for non-bash entrypoints
on main too (the bash `$OUTPUT` wrapper is piped into `python3 -s`; reproduced on a
main-built daemon: `SyntaxError: invalid syntax` at `_SSH_OUTPUT=$(mktemp)`). Fixed in
U9 by routing opted-in non-bash entrypoints to the runner (no preflight — the body is
not bash) and passing `Step.Entrypoint` through the client.

- [x] Pass (both hosts, after fix; control run against main captured in evidence)

---

## T13.10: Binary + unicode + large output (b64 io framing)

**DSL:**
```
### bin
{"ssh": "<host>", "interp": true}

head -c 100000 /dev/urandom > /tmp/u9-bin
echo "srcmd5=$(md5sum < /tmp/u9-bin | cut -d' ' -f1)" >> $OUTPUT
cat /tmp/u9-bin

### verify-local

got=$(md5 -q "{{bin.output}}" 2>/dev/null || md5sum < "{{bin.output}}" | cut -d' ' -f1)
test "$got" = "{{bin.outputs.srcmd5}}" && echo "BINARY-ROUNDTRIP-OK"
```

Plus a unicode step (CJK + emoji in args, stdout, and `$OUTPUT` values) and a large step
(`seq 1 50000`).

**Expected:** `BINARY-ROUNDTRIP-OK` — daemon-side md5 of received stdout equals the
remote-computed md5 (b64-iff-binary io chunks round-trip bit-exact). Unicode intact in
trace lines, preview, and `$OUTPUT`. Large output intact (50002-line .out file); preview
truncation (16000 chars) unchanged.

**Boundary (unchanged from today):** `{{step.output}}` resolves to a DAEMON-LOCAL path —
consuming it from a REMOTE step fails on the legacy path today and on the runner path
alike (the runner protocol's file frame + `client.Step.Files` exist for exactly this
upgrade, but the executor does not populate them yet — named follow-up).

- [x] Pass (both hosts)

---

## T13.11: Differential gate (decision #16c)

```
go test ./internal/interp/... -run Differential -v
```

**Expected:** GREEN. Every corpus script runs through real bash AND the runner;
divergence on cwd/env/stdout/stderr/exit/files-touched fails unless the screener routes
it or it is a named residual class (C: set -e + cmdsubst; G: IFS empty-field drop).

- [x] Pass (2026-07-16: 39 scripts — 15 interp-match strict, 22 screened-realbash,
  2 residual accepted; 0 unexplained)

---

## T13.12: tmux tripwire (no-regression)

`internal/mcp/mcp_tmux_interp.go` must be byte-identical to main (`git diff main..HEAD`
empty), the `trap - DEBUG` disarm present, and tmux unit tests green.

- [x] Pass (2026-07-16)
