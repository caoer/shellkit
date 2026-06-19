---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T08: Local Execution

Test steps without `"ssh"` config — scripts run locally on the machine hosting the shellkit MCP daemon.

---

## T08.1: Basic Local

**DSL:**
```
### local-basic

echo "running locally on $(hostname)"
echo "local_host=$(hostname)" >> $OUTPUT
echo "local_user=$(whoami)" >> $OUTPUT
```

**Expected:** Runs on the MCP daemon's host (not orb-mcp-test-a). `local_host` is the Mac hostname (not a VM).

- [ ] Pass

---

## T08.2: Local $OUTPUT

**DSL:**
```
### local-output

echo "key1=value1" >> $OUTPUT
echo "key2=value2" >> $OUTPUT
echo "key3=value3" >> $OUTPUT
```

**Expected:** All 3 key=value pairs captured. Same $OUTPUT mechanism as remote execution.

- [ ] Pass

---

## T08.3: Local Timeout

**DSL:**
```
### local-timeout
{"timeout": 2}

echo "start local"
sleep 10
echo "unreachable"
```

**Expected:** Killed after ~2s. Exit 137. stdout has "start local" but NOT "unreachable".

- [ ] Pass

---

## T08.4: Local continue_on_error

**DSL:**
```
### local-fail
{"continue_on_error": true}

echo "local failure"
exit 1

### local-after

echo "continued"
echo "ok=true" >> $OUTPUT
```

**Expected:** local-fail exit 1. local-after executes. `ok=true`.

- [ ] Pass

---

## T08.5: Local to Remote Chain

**DSL:**
```
### local-generate

TOKEN=$(date +%s%N | shasum | head -c 16)
echo "token=$TOKEN" >> $OUTPUT

### remote-consume
{"ssh": "orb-mcp-test-a"}

echo "Token from local: {{local-generate.outputs.token}}"
echo "received={{local-generate.outputs.token}}" >> $OUTPUT
```

**Expected:** Token generated locally, resolved remotely. `received` matches `token`.

- [ ] Pass

---

## T08.6: Remote to Local Chain

**DSL:**
```
### remote-produce
{"ssh": "orb-mcp-test-a"}

echo "kernel=$(uname -r)" >> $OUTPUT
echo "remote_pid=$$" >> $OUTPUT

### local-consume

echo "Remote kernel: {{remote-produce.outputs.kernel}}"
echo "got_kernel={{remote-produce.outputs.kernel}}" >> $OUTPUT
echo "got_pid={{remote-produce.outputs.remote_pid}}" >> $OUTPUT
```

**Expected:** Local step reads remote outputs. Both values present and valid.

- [ ] Pass

---

## T08.7: Local → Remote → Local Pipeline

**DSL:**
```
### phase-1-local

echo "origin=local-$(date +%s)" >> $OUTPUT

### phase-2-remote
{"ssh": "orb-mcp-test-a"}

echo "forwarded={{phase-1-local.outputs.origin}}" >> $OUTPUT
echo "enriched={{phase-1-local.outputs.origin}}-remote" >> $OUTPUT

### phase-3-local

echo "Final: {{phase-2-remote.outputs.enriched}}"
echo "result={{phase-2-remote.outputs.enriched}}" >> $OUTPUT
```

**Expected:** Three-step pipeline crosses local→remote→local. `result` contains `local-TIMESTAMP-remote`.

- [ ] Pass

---

## T08.8: Local File Path Reference

**DSL:**
```
### local-writer

echo "alpha"
echo "beta"
echo "gamma"
echo "delta"

### local-reader

wc -l < {{local-writer.output}}
echo "count=$(wc -l < {{local-writer.output}} | tr -d ' ')" >> $OUTPUT
```

**Expected:** `count=4`. File path reference works between local steps.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T08.1 | Basic local | |
| T08.2 | Local $OUTPUT | |
| T08.3 | Local timeout | |
| T08.4 | Local continue_on_error | |
| T08.5 | Local → Remote | |
| T08.6 | Remote → Local | |
| T08.7 | Local → Remote → Local | |
| T08.8 | Local file ref | |
