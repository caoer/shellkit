---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T01: Core DSL Operations

Test the non-SSH actions: help, list, check. These are metadata operations that don't execute scripts on remote hosts.

## Instructions

Execute each test in order. Verify expected result matches actual output. Mark pass/fail.

---

## T01.1: Help

**DSL:**
```
### help
```

**Expected:** Returns DSL reference with examples including `### all-servers`, `### probe-nyc`, `### check-nix`, etc. Exit 0. Output is the help text, not an error.

- [ ] Pass

---

## T01.2: List All Servers

**DSL:**
```
### all-servers
{"list": true}
```

**Expected:** Returns server table with `count=N` where N > 0. Both `orb-mcp-test-a` and `orb-mcp-test-b` appear. Format: columns for name, provider, IP, role, location.

- [ ] Pass

---

## T01.3: List Filter by Provider

**DSL:**
```
### orb-vms
{"list": true, "filter": "provider=orbstack"}
```

**Expected:** Only orbstack-provider hosts returned. `orb-mcp-test-a` and `orb-mcp-test-b` present. Count >= 2.

- [ ] Pass

---

## T01.4: List Filter by Substring

**DSL:**
```
### filter-substring
{"list": true, "filter": "mcp-test"}
```

**Expected:** Only hosts with `mcp-test` in name. Count = 2 (orb-mcp-test-a and orb-mcp-test-b).

- [ ] Pass

---

## T01.5: List Filter No Results

**DSL:**
```
### filter-empty
{"list": true, "filter": "provider=nonexistent-provider-xyz"}
```

**Expected:** Empty results. Count = 0. No error — valid query, just no matches.

- [ ] Pass

---

## T01.6: List Filter by Role

**DSL:**
```
### filter-role
{"list": true, "filter": "role=proxy"}
```

**Expected:** Only hosts with role=proxy. Count > 0. No hosts without proxy role.

- [ ] Pass

---

## T01.7: Check Single Host

**DSL:**
```
### probe-one
{"check": "orb-mcp-test-a"}
```

**Expected:** Shows OK status with latency in ms. Exit 0. Count = 1.

- [ ] Pass

---

## T01.8: Check All Hosts

**DSL:**
```
### probe-all
{"check": true}
```

**Expected:** Probes all servers in inventory. Shows status per host. Count matches total inventory. Some may be unreachable (that's OK — test verifies the operation runs, not that all hosts are up).

- [ ] Pass

---

## T01.9: Check Unknown Host

**DSL:**
```
### probe-invalid
{"check": "nonexistent-host-xyz-99"}
```

**Expected:** Error mentioning unknown host. Does not hang or crash.

- [ ] Pass

---

## T01.10: Multiple Help Blocks

Verify that a DSL with only help blocks works (no actual execution).

**DSL:**
```
### first-help

### second-help
```

**Expected:** Both blocks return help text. No errors.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T01.1 | Help | |
| T01.2 | List all | |
| T01.3 | List filter provider | |
| T01.4 | List filter substring | |
| T01.5 | List filter no results | |
| T01.6 | List filter role | |
| T01.7 | Check single host | |
| T01.8 | Check all hosts | |
| T01.9 | Check unknown host | |
| T01.10 | Multiple help blocks | |
