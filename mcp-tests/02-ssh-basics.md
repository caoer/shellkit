---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T02: SSH Basic Execution

Test fundamental SSH execution: running commands, capturing output, using inventory templates.

---

## T02.1: Simple Command

**DSL:**
```
### simple-cmd
{"ssh": "orb-mcp-test-a"}

hostname && whoami
```

**Expected:** stdout contains hostname and `root`. Exit 0. Output file created.

- [ ] Pass

---

## T02.2: Multi-Line Script

**DSL:**
```
### multiline
{"ssh": "orb-mcp-test-a"}

echo "line 1"
echo "line 2"
echo "line 3"
MYVAR="hello"
echo "var is $MYVAR"
```

**Expected:** stdout shows all 4 echo outputs in order. Variable expansion works. Exit 0.

- [ ] Pass

---

## T02.3: $OUTPUT Capture

**DSL:**
```
### output-capture
{"ssh": "orb-mcp-test-a"}

echo "visible stdout"
echo "hostname=$(hostname)" >> $OUTPUT
echo "user=$(whoami)" >> $OUTPUT
echo "date=$(date +%F)" >> $OUTPUT
echo "arch=$(uname -m)" >> $OUTPUT
```

**Expected:** stdout shows "visible stdout". Outputs section shows `hostname`, `user`, `date=2026-04-26`, `arch`. All 4 keys present.

- [ ] Pass

---

## T02.4: $OUTPUT Multiple Values Same Key

**DSL:**
```
### output-overwrite
{"ssh": "orb-mcp-test-a"}

echo "first=alpha" >> $OUTPUT
echo "first=beta" >> $OUTPUT
echo "second=gamma" >> $OUTPUT
```

**Expected:** `first=beta` (last write wins). `second=gamma` preserved.

- [ ] Pass

---

## T02.5: $OUTPUT Empty (No Writes)

**DSL:**
```
### no-output-writes
{"ssh": "orb-mcp-test-a"}

echo "I write nothing to OUTPUT"
```

**Expected:** Exit 0. Outputs section empty or absent. No error from missing output data.

- [ ] Pass

---

## T02.6: $OUTPUT Malformed Lines

**DSL:**
```
### malformed-output
{"ssh": "orb-mcp-test-a"}

echo "good_key=good_value" >> $OUTPUT
echo "no-equals-sign" >> $OUTPUT
echo "" >> $OUTPUT
echo "another_good=value2" >> $OUTPUT
```

**Expected:** `good_key=good_value` and `another_good=value2` parsed. Malformed line and empty line silently skipped. No error.

- [ ] Pass

---

## T02.7: Inventory Template — IP Lookup

**DSL:**
```
### ip-lookup
{"ssh": "orb-mcp-test-a"}

echo "My IP from inventory: {{orb-mcp-test-a.ip}}"
echo "resolved_ip={{orb-mcp-test-a.ip}}" >> $OUTPUT
```

**Expected:** Template resolves to actual IP address. stdout shows the resolved IP. Output key `resolved_ip` has valid IP.

- [ ] Pass

---

## T02.8: Inventory Template — User and Port

**DSL:**
```
### user-port
{"ssh": "orb-mcp-test-a"}

echo "user={{orb-mcp-test-a.user}} port={{orb-mcp-test-a.port}}"
echo "inv_user={{orb-mcp-test-a.user}}" >> $OUTPUT
echo "inv_port={{orb-mcp-test-a.port}}" >> $OUTPUT
```

**Expected:** user resolves to `root` (default). port resolves to `22` (default). Both appear in outputs.

- [ ] Pass

---

## T02.9: Stderr Capture

**DSL:**
```
### stderr-test
{"ssh": "orb-mcp-test-a"}

echo "this is stdout"
echo "this is stderr" >&2
echo "back to stdout"
```

**Expected:** stdout shows "this is stdout" and "back to stdout". Stderr captured separately showing "this is stderr". Exit 0.

- [ ] Pass

---

## T02.10: Non-Zero Exit Code

**DSL:**
```
### exit-code
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "before exit"
exit 42
echo "after exit (unreachable)"
```

**Expected:** Exit code 42. stdout has "before exit" but NOT "after exit (unreachable)".

- [ ] Pass

---

## T02.11: Environment Variables

**DSL:**
```
### env-vars
{"ssh": "orb-mcp-test-a"}

echo "HOME=$HOME"
echo "PATH=$PATH"
echo "SHELL=$SHELL"
echo "USER=$USER"
echo "has_output=$(test -n \"$OUTPUT\" && echo yes || echo no)" >> $OUTPUT
```

**Expected:** Standard env vars populated. `$OUTPUT` env var is set (`has_output=yes`).

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T02.1 | Simple command | |
| T02.2 | Multi-line script | |
| T02.3 | $OUTPUT capture | |
| T02.4 | $OUTPUT overwrite | |
| T02.5 | $OUTPUT empty | |
| T02.6 | $OUTPUT malformed | |
| T02.7 | Inventory IP | |
| T02.8 | Inventory user/port | |
| T02.9 | Stderr capture | |
| T02.10 | Non-zero exit | |
| T02.11 | Environment variables | |
