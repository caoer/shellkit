---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a, orb-mcp-test-b]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T07: Fan-Out (Multi-Host)

Test `"ssh": ["host-a", "host-b"]` — sequential execution across multiple hosts.

---

## T07.1: Basic Fan-Out

**DSL:**
```
### fanout-basic
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

hostname
echo "host=$(hostname)" >> $OUTPUT
```

**Expected:** Two result blocks — one per host. Each shows its own hostname. Merged output file has `=== orb-mcp-test-a ===` and `=== orb-mcp-test-b ===` delimiters.

- [ ] Pass

---

## T07.2: Per-Host Output Reference

**DSL:**
```
### fanout-ids
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

echo "id=$(hostname)-$$" >> $OUTPUT
echo "arch=$(uname -m)" >> $OUTPUT

### read-per-host

echo "A: {{fanout-ids.orb-mcp-test-a.outputs.id}}"
echo "B: {{fanout-ids.orb-mcp-test-b.outputs.id}}"
echo "a_id={{fanout-ids.orb-mcp-test-a.outputs.id}}" >> $OUTPUT
echo "b_id={{fanout-ids.orb-mcp-test-b.outputs.id}}" >> $OUTPUT
```

**Expected:** Per-host template `{{step.hostname.outputs.key}}` resolves. IDs differ (different hostnames/PIDs).

- [ ] Pass

---

## T07.3: Merged Output File

**DSL:**
```
### fanout-merge
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

uname -a
echo "kernel=$(uname -r)" >> $OUTPUT

### read-merged

cat {{fanout-merge.output}}
echo "lines=$(wc -l < {{fanout-merge.output}})" >> $OUTPUT
```

**Expected:** `{{fanout-merge.output}}` resolves to merged file containing both hosts' output with delimiter headers.

- [ ] Pass

---

## T07.4: Per-Host File Reference

**DSL:**
```
### fanout-files
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

echo "data from $(hostname)"
echo "marker=$(hostname)" >> $OUTPUT

### read-a-file

cat {{fanout-files.orb-mcp-test-a.output}}
echo "a_content=$(head -1 {{fanout-files.orb-mcp-test-a.output}})" >> $OUTPUT

### read-b-file

cat {{fanout-files.orb-mcp-test-b.output}}
echo "b_content=$(head -1 {{fanout-files.orb-mcp-test-b.output}})" >> $OUTPUT
```

**Expected:** Per-host output files exist as separate files. Content differs between hosts.

- [ ] Pass

---

## T07.5: Fan-Out with $OUTPUT

**DSL:**
```
### fanout-outputs
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

echo "uptime=$(cat /proc/uptime | cut -d' ' -f1)" >> $OUTPUT
echo "pid=$$" >> $OUTPUT

### check-last-wins

echo "Last host outputs.pid: {{fanout-outputs.outputs.pid}}"
echo "Host A pid: {{fanout-outputs.orb-mcp-test-a.outputs.pid}}"
echo "Host B pid: {{fanout-outputs.orb-mcp-test-b.outputs.pid}}"
echo "last_pid={{fanout-outputs.outputs.pid}}" >> $OUTPUT
echo "b_pid={{fanout-outputs.orb-mcp-test-b.outputs.pid}}" >> $OUTPUT
```

**Expected:** `{{fanout-outputs.outputs.pid}}` = last host's pid (orb-mcp-test-b). `last_pid` should equal `b_pid`. Per-host refs give distinct values.

- [ ] Pass

---

## T07.6: Fan-Out Failure — Abort

**DSL:**
```
### fanout-fail
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

exit 1

### after-fanout-fail
{"ssh": "orb-mcp-test-a"}

echo "should not run"
echo "ran=true" >> $OUTPUT
```

**Expected:** Fan-out aborts on first host failure. `after-fanout-fail` does NOT execute. No `ran=true` in any outputs.

- [ ] Pass

---

## T07.7: Fan-Out with continue_on_error

**DSL:**
```
### fanout-continue
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"], "continue_on_error": true}

hostname
echo "ran=$(hostname)" >> $OUTPUT

### after-fanout-continue
{"ssh": "orb-mcp-test-a"}

echo "pipeline continued"
echo "ok=true" >> $OUTPUT
```

**Expected:** Both hosts execute. Pipeline continues. `ok=true` in final step.

- [ ] Pass

---

## T07.8: Cross-Host Reference Chain

**DSL:**
```
### seed-hosts
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

echo "token=$(hostname | md5sum | cut -c1-8)" >> $OUTPUT

### compare
{"ssh": "orb-mcp-test-a"}

A="{{seed-hosts.orb-mcp-test-a.outputs.token}}"
B="{{seed-hosts.orb-mcp-test-b.outputs.token}}"
echo "A=$A B=$B"
if [ "$A" != "$B" ]; then
  echo "tokens differ (correct)"
  echo "differ=true" >> $OUTPUT
else
  echo "differ=false" >> $OUTPUT
fi
```

**Expected:** `differ=true`. Tokens from different hosts are distinct. Cross-host reference works in a single-host step.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T07.1 | Basic fan-out | |
| T07.2 | Per-host output ref | |
| T07.3 | Merged output file | |
| T07.4 | Per-host file ref | |
| T07.5 | Last-host-wins | |
| T07.6 | Fan-out abort | |
| T07.7 | Fan-out continue | |
| T07.8 | Cross-host reference | |
