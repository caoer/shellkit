---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T03: Cross-Step References

Test `{{step.outputs.key}}` and `{{step.output}}` template resolution between steps.

---

## T03.1: Basic Output Reference

**DSL:**
```
### producer
{"ssh": "orb-mcp-test-a"}

echo "token=secret123" >> $OUTPUT
echo "count=42" >> $OUTPUT

### consumer
{"ssh": "orb-mcp-test-a"}

echo "Got token: {{producer.outputs.token}}"
echo "Got count: {{producer.outputs.count}}"
echo "received_token={{producer.outputs.token}}" >> $OUTPUT
echo "received_count={{producer.outputs.count}}" >> $OUTPUT
```

**Expected:** consumer stdout shows "Got token: secret123" and "Got count: 42". Outputs: `received_token=secret123`, `received_count=42`.

- [ ] Pass

---

## T03.2: File Path Reference ({{step.output}})

**DSL:**
```
### generate-data
{"ssh": "orb-mcp-test-a"}

for i in 1 2 3 4 5; do echo "line-$i"; done

### count-local

wc -l < {{generate-data.output}}
echo "lines=$(wc -l < {{generate-data.output}})" >> $OUTPUT
```

**Expected:** `{{generate-data.output}}` resolves to local file path. count-local reads 5 lines.

- [ ] Pass

---

## T03.3: Three-Step Chain

**DSL:**
```
### step-1
{"ssh": "orb-mcp-test-a"}

echo "seed=alpha" >> $OUTPUT

### step-2
{"ssh": "orb-mcp-test-a"}

echo "Received: {{step-1.outputs.seed}}"
echo "derived={{step-1.outputs.seed}}-beta" >> $OUTPUT

### step-3
{"ssh": "orb-mcp-test-a"}

echo "Final: {{step-2.outputs.derived}}"
echo "final={{step-2.outputs.derived}}" >> $OUTPUT
```

**Expected:** step-2 gets `alpha`. step-3 gets `alpha-beta`. Chain resolves correctly through 3 levels.

- [ ] Pass

---

## T03.4: Reference Non-Existent Step

**DSL:**
```
### valid-step
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "ref={{nonexistent-step.outputs.key}}"
```

**Expected:** Error about unknown step `nonexistent-step`. Step fails or template resolution error surfaced.

- [ ] Pass

---

## T03.5: Reference Non-Existent Output Key

**DSL:**
```
### has-outputs
{"ssh": "orb-mcp-test-a"}

echo "real_key=real_value" >> $OUTPUT

### bad-ref
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

echo "ref={{has-outputs.outputs.fake_key}}"
```

**Expected:** Error about missing output key `fake_key` on step `has-outputs`.

- [ ] Pass

---

## T03.6: Output with Special Characters

**DSL:**
```
### special-output
{"ssh": "orb-mcp-test-a"}

echo "path=/usr/local/bin/my app" >> $OUTPUT
echo "msg=hello world & goodbye" >> $OUTPUT
echo "quoted=it's \"fine\"" >> $OUTPUT

### consume-special
{"ssh": "orb-mcp-test-a"}

echo "path is: {{special-output.outputs.path}}"
echo "msg is: {{special-output.outputs.msg}}"
echo "received_path={{special-output.outputs.path}}" >> $OUTPUT
```

**Expected:** Shell-escaped values resolve correctly. No injection or syntax errors. Special chars (spaces, ampersand, quotes) handled.

- [ ] Pass

---

## T03.7: Output File Content Verification

**DSL:**
```
### produce-json
{"ssh": "orb-mcp-test-a"}

echo '{"name": "test", "version": 1}'
echo '{"name": "test2", "version": 2}'

### read-output-file

cat {{produce-json.output}}
echo "line_count=$(wc -l < {{produce-json.output}})" >> $OUTPUT
```

**Expected:** Output file contains both JSON lines. line_count=2.

- [ ] Pass

---

## T03.8: Multiple Steps Reference Same Producer

**DSL:**
```
### shared-producer
{"ssh": "orb-mcp-test-a"}

echo "shared_val=42" >> $OUTPUT

### consumer-a
{"ssh": "orb-mcp-test-a"}

echo "A got: {{shared-producer.outputs.shared_val}}"
echo "a_got={{shared-producer.outputs.shared_val}}" >> $OUTPUT

### consumer-b
{"ssh": "orb-mcp-test-a"}

echo "B got: {{shared-producer.outputs.shared_val}}"
echo "b_got={{shared-producer.outputs.shared_val}}" >> $OUTPUT
```

**Expected:** Both consumers receive `42`. No conflict. Same output readable by multiple downstream steps.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T03.1 | Basic output reference | |
| T03.2 | File path reference | |
| T03.3 | Three-step chain | |
| T03.4 | Reference non-existent step | |
| T03.5 | Reference non-existent key | |
| T03.6 | Output with special chars | |
| T03.7 | Output file content | |
| T03.8 | Multiple consumers | |
