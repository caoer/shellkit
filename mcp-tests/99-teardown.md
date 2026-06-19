---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a, orb-mcp-test-b]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T99: Teardown

Destroy test VMs after all test suites complete. Run this LAST.

## Instructions

1. Confirm all other test files have been executed
2. Destroy both test VMs via orbctl
3. Verify cleanup

---

## T99.1: Final Cleanup on VMs

**DSL:**
```
### final-remote-cleanup
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"], "continue_on_error": true}

rm -rf /tmp/shellkit-*
echo "cleaned=true" >> $OUTPUT
```

**Expected:** Any remaining temp files removed on both VMs.

- [ ] Pass

---

## T99.2: Destroy Test VMs

**Run via Bash tool (not MCP):**
```bash
orbctl delete orb-mcp-test-a
orbctl delete orb-mcp-test-b
```

**Expected:** Both VMs deleted. `orbctl list | grep mcp-test` returns nothing.

- [ ] Pass

---

## T99.3: Verify Removal from Inventory

**DSL:**
```
### verify-gone
{"list": true, "filter": "mcp-test"}
```

**Expected:** Count = 0. No mcp-test VMs in inventory (auto-discovery no longer finds them).

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T99.1 | Remote cleanup | |
| T99.2 | Destroy VMs | |
| T99.3 | Verify removal | |
