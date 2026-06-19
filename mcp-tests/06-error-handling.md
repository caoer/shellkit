---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T06: Error Handling

Test timeout, abort-on-failure, continue_on_error, and error propagation.

---

## T06.1: Timeout — Script Killed

**DSL:**
```
### timeout-kill
{"ssh": "orb-mcp-test-a", "timeout": 3}

echo "starting"
sleep 30
echo "should never print"
```

**Expected:** Step aborted after ~3s. Exit code 137. stdout has "starting" but NOT "should never print". Trace should show where it was killed.

- [ ] Pass

---

## T06.2: Timeout — Fast Script Completes

**DSL:**
```
### timeout-ok
{"ssh": "orb-mcp-test-a", "timeout": 30}

echo "done quickly"
echo "status=fast" >> $OUTPUT
```

**Expected:** Completes well within timeout. Exit 0. Output captured.

- [ ] Pass

---

## T06.3: Timeout — Exact Boundary

**DSL:**
```
### timeout-boundary
{"ssh": "orb-mcp-test-a", "timeout": 5}

echo "start"
sleep 3
echo "after 3s sleep"
echo "completed=true" >> $OUTPUT
```

**Expected:** Completes (3s sleep < 5s timeout). Exit 0. `completed=true` in outputs.

- [ ] Pass

---

## T06.4: Abort on Failure (Default)

**DSL:**
```
### will-fail
{"ssh": "orb-mcp-test-a"}

echo "about to fail"
exit 1

### should-skip
{"ssh": "orb-mcp-test-a"}

echo "this must not execute"
echo "ran=true" >> $OUTPUT
```

**Expected:** `will-fail` exits 1. `should-skip` does NOT execute. No "this must not execute" in any output. Pipeline aborted.

- [ ] Pass

---

## T06.5: continue_on_error — Pipeline Continues

**DSL:**
```
### fail-ok
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "failing gracefully"
exit 1

### after-fail
{"ssh": "orb-mcp-test-a"}

echo "I still ran"
echo "alive=true" >> $OUTPUT
```

**Expected:** `fail-ok` exit 1. `after-fail` STILL executes. `alive=true` in outputs. Pipeline continues past the failure.

- [ ] Pass

---

## T06.6: Multiple Failures with continue_on_error

**DSL:**
```
### fail-1
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

exit 1

### fail-2
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

exit 2

### success-after
{"ssh": "orb-mcp-test-a"}

echo "survived both failures"
echo "ok=true" >> $OUTPUT
```

**Expected:** fail-1 exit 1, fail-2 exit 2, success-after exit 0. All three execute. `ok=true` captured.

- [ ] Pass

---

## T06.7: Abort After continue_on_error Step

A non-continue_on_error step fails AFTER a continue_on_error step succeeded.

**DSL:**
```
### safe-fail
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "safe fail"
exit 1

### unsafe-fail
{"ssh": "orb-mcp-test-a"}

echo "this fails and aborts"
exit 3

### never-runs
{"ssh": "orb-mcp-test-a"}

echo "unreachable"
echo "ran=true" >> $OUTPUT
```

**Expected:** safe-fail executes (exit 1, continues). unsafe-fail executes (exit 3, aborts). never-runs does NOT execute.

- [ ] Pass

---

## T06.8: SSH to Unknown Host

**DSL:**
```
### ssh-unknown
{"ssh": "nonexistent-host-xyz-99", "continue_on_error": true}

echo "unreachable"
```

**Expected:** Error about unknown host. Step fails. Clear error message naming the host.

- [ ] Pass

---

## T06.9: Command Not Found

**DSL:**
```
### cmd-not-found
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

some_nonexistent_command_xyz_123
```

**Expected:** Exit code 127. Stderr mentions "command not found".

- [ ] Pass

---

## T06.10: Script Syntax Error

**DSL:**
```
### syntax-error
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "start
```

**Expected:** Bash parse error (unclosed double quote). Non-zero exit (2). Stderr shows syntax error.

- [ ] Pass

---

## T06.11: Permission Denied

**DSL:**
```
### perm-denied
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

cat /etc/shadow-nonexistent 2>&1 || true
chmod 000 /tmp/shellkit-perm-test 2>/dev/null
touch /tmp/shellkit-perm-test && chmod 000 /tmp/shellkit-perm-test
cat /tmp/shellkit-perm-test
```

**Expected:** Permission denied error for reading the chmod'd file. Non-zero exit (but as root on Debian, root may bypass — test documents actual behavior).

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T06.1 | Timeout kill | |
| T06.2 | Timeout OK | |
| T06.3 | Timeout boundary | |
| T06.4 | Abort on failure | |
| T06.5 | continue_on_error | |
| T06.6 | Multiple failures continue | |
| T06.7 | Abort after continue | |
| T06.8 | Unknown host | |
| T06.9 | Command not found | |
| T06.10 | Syntax error | |
| T06.11 | Permission denied | |
