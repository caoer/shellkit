---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a, orb-mcp-test-b]
depends-on: []
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T00: Provision Test VMs

Create fresh orbctl VMs and verify shellkit auto-discovers them. All subsequent test files depend on this one.

## Prerequisites

- orbctl installed and working (`orbctl version`)
- shellkit MCP daemon running (`shellkit mcp status`)
- No existing VMs named `orb-mcp-test-a` or `orb-mcp-test-b`

## Setup

Run these shell commands directly (not via shellkit MCP) to create the VMs:

```bash
orbctl delete orb-mcp-test-a 2>/dev/null; orbctl create -a debian:bookworm orb-mcp-test-a
orbctl delete orb-mcp-test-b 2>/dev/null; orbctl create -a debian:bookworm orb-mcp-test-b
```

Wait for VMs to be running:

```bash
orbctl list | grep mcp-test
```

---

## T00.1: Verify Auto-Discovery

shellkit should auto-discover the orbctl VMs without any config changes.

**DSL:**
```
### find-test-vms
{"list": true, "filter": "mcp-test"}
```

**Expected:** Both `orb-mcp-test-a` and `orb-mcp-test-b` appear in results. Provider shows `orbstack`.

- [ ] Pass

---

## T00.2: Probe Test VMs

**DSL:**
```
### probe-a
{"check": "orb-mcp-test-a"}

### probe-b
{"check": "orb-mcp-test-b"}
```

**Expected:** Both hosts show OK with latency. Exit 0.

- [ ] Pass

---

## T00.3: Basic Connectivity

**DSL:**
```
### whoami-a
{"ssh": "orb-mcp-test-a"}

hostname && whoami && cat /etc/os-release | head -3

### whoami-b
{"ssh": "orb-mcp-test-b"}

hostname && whoami && cat /etc/os-release | head -3
```

**Expected:** Both return hostname, `root`, and Debian bookworm info. Exit 0.

- [ ] Pass

---

## T00.4: Install Test Dependencies

Provision both VMs with tools needed by later test files.

**DSL:**
```
### provision-a
{"ssh": "orb-mcp-test-a", "timeout": 120}

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq python3 curl jq > /dev/null 2>&1
echo "python3=$(python3 --version 2>&1)" >> $OUTPUT
echo "curl=$(curl --version | head -1 | awk '{print $2}')" >> $OUTPUT
echo "jq=$(jq --version)" >> $OUTPUT
echo "provisioned=true" >> $OUTPUT

### provision-b
{"ssh": "orb-mcp-test-b", "timeout": 120}

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq python3 curl jq > /dev/null 2>&1
echo "python3=$(python3 --version 2>&1)" >> $OUTPUT
echo "provisioned=true" >> $OUTPUT
```

**Expected:** Both VMs have python3, curl, jq installed. `provisioned=true` in outputs.

- [ ] Pass

---

## T00.5: Verify Inventory Properties

**DSL:**
```
### check-ip
{"ssh": "orb-mcp-test-a"}

ip addr show eth0 | grep 'inet ' | awk '{print $2}' | cut -d/ -f1
echo "actual_ip=$(ip addr show eth0 | grep 'inet ' | awk '{print $2}' | cut -d/ -f1)" >> $OUTPUT

### template-ip

echo "inventory says: {{orb-mcp-test-a.ip}}"
echo "actual says: {{check-ip.outputs.actual_ip}}"
echo "inventory_ip={{orb-mcp-test-a.ip}}" >> $OUTPUT
echo "actual_ip={{check-ip.outputs.actual_ip}}" >> $OUTPUT
```

**Expected:** Inventory IP matches actual VM IP (orbctl-assigned). Both resolve correctly.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T00.1 | Auto-discovery | |
| T00.2 | Probe connectivity | |
| T00.3 | Basic connectivity | |
| T00.4 | Install dependencies | |
| T00.5 | Inventory properties | |

**Teardown:** Do NOT destroy VMs here. Other test files depend on them. Run `99-teardown.md` after all tests complete.
