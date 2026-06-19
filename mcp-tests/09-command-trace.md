---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T09: Command Trace

Test the bash DEBUG trap command trace. On by default for bash, shows per-command timing on timeout.

---

## T09.1: Trace — Normal Execution (Transparent)

**DSL:**
```
### trace-normal
{"ssh": "orb-mcp-test-a"}

echo "step 1"
sleep 1
echo "step 2"
echo "done=true" >> $OUTPUT
```

**Expected:** Exit 0. `done=true`. Trace is active but transparent — script output is clean (trace markers filtered out by MCP server). Normal execution not affected.

- [ ] Pass

---

## T09.2: Trace — Timeout Shows Where It Stopped

**DSL:**
```
### trace-timeout
{"ssh": "orb-mcp-test-a", "timeout": 4}

echo "phase 1"
sleep 1
echo "phase 2"
sleep 1
echo "phase 3: long op"
sleep 30
echo "phase 4: unreachable"
```

**Expected:** Exit 137 (timeout). Result includes "command trace:" showing:
- `echo "phase 1"` — ran
- `sleep 1` — ran
- `echo "phase 2"` — ran
- `sleep 1` — ran
- `echo "phase 3: long op"` — ran
- `sleep 30` — "timed out here"

"phase 4" does NOT appear in trace.

- [ ] Pass

---

## T09.3: Trace Disabled

**DSL:**
```
### trace-off
{"ssh": "orb-mcp-test-a", "trace": false}

echo "no trace"
sleep 1
echo "still no trace"
echo "ok=true" >> $OUTPUT
```

**Expected:** No "command trace:" in result. Trace suppressed by `"trace": false`. Script runs normally.

- [ ] Pass

---

## T09.4: Trace Explicit Enable

**DSL:**
```
### trace-on
{"ssh": "orb-mcp-test-a", "trace": true, "timeout": 3}

echo "traced step"
sleep 1
echo "another traced step"
sleep 10
```

**Expected:** Timed out. "command trace:" present showing the commands with timing.

- [ ] Pass

---

## T09.5: No Trace for Non-Bash Entrypoints

**DSL:**
```
### trace-python
{"ssh": "orb-mcp-test-a", "entrypoint": "python3", "timeout": 3}

import time
print("py step 1")
time.sleep(1)
print("py step 2")
time.sleep(10)
```

**Expected:** Timed out. No "command trace:" — DEBUG trap not available in Python. stdout has "py step 1" and "py step 2".

- [ ] Pass

---

## T09.6: Trace with Many Commands

**DSL:**
```
### trace-many-cmds
{"ssh": "orb-mcp-test-a", "timeout": 5}

for i in $(seq 1 50); do echo "cmd $i"; done
sleep 10
echo "unreachable"
```

**Expected:** Timed out. Trace may be truncated ("N earlier commands omitted"). Shows the last commands before timeout. Bounded output.

- [ ] Pass

---

## T09.7: Trace Timing Accuracy

**DSL:**
```
### trace-timing
{"ssh": "orb-mcp-test-a", "timeout": 8}

echo "start"
sleep 2
echo "after 2s"
sleep 3
echo "after 5s"
sleep 10
echo "unreachable"
```

**Expected:** Trace shows elapsed seconds. `echo "start"` at +0s, `sleep 2` at +0s, `echo "after 2s"` at +2s, `sleep 3` at +2s, `echo "after 5s"` at +5s, `sleep 10` at +5s (timed out). Timing approximately correct.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T09.1 | Transparent trace | |
| T09.2 | Timeout shows location | |
| T09.3 | Trace disabled | |
| T09.4 | Trace explicit enable | |
| T09.5 | Non-bash no trace | |
| T09.6 | Many commands truncated | |
| T09.7 | Timing accuracy | |
