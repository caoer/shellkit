---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T10: Edge Cases

Special characters, heredocs, JSON ambiguity, large output, unicode, and parser boundary conditions.

---

## T10.1: Single Quotes in Script

Regression: bash -c broke on scripts with single quotes. stdin delivery (bash -s) fixed this.

**DSL:**
```
### single-quotes
{"ssh": "orb-mcp-test-a"}

echo 'single quotes work'
echo "it's a test"
VAR='quoted value'
echo "$VAR"
echo "quote_test=passed" >> $OUTPUT
```

**Expected:** All outputs correct. No quoting errors. `quote_test=passed`. stdin delivery handles single quotes.

- [ ] Pass

---

## T10.2: Double Quotes and Dollar Signs

**DSL:**
```
### double-quotes
{"ssh": "orb-mcp-test-a"}

echo "HOME is $HOME"
echo "literal: \$NOT_A_VAR"
echo "subshell: $(echo nested)"
echo "ok=true" >> $OUTPUT
```

**Expected:** `$HOME` expands to `/root`. `\$NOT_A_VAR` shows literal. Subshell works. Standard bash quoting.

- [ ] Pass

---

## T10.3: Heredoc — No Expansion

**DSL:**
```
### heredoc-literal
{"ssh": "orb-mcp-test-a"}

cat << 'EOF_INNER'
Heredoc inside DSL body.
$HOME stays literal.
"quotes" and 'apostrophes' fine.
EOF_INNER
echo "heredoc=ok" >> $OUTPUT
```

**Expected:** `$HOME` NOT expanded (single-quoted delimiter). Special chars printed literally. `heredoc=ok`.

- [ ] Pass

---

## T10.4: Heredoc — With Expansion

**DSL:**
```
### heredoc-expand
{"ssh": "orb-mcp-test-a"}

cat << EOF_EXPAND
Home: $HOME
User: $(whoami)
EOF_EXPAND
echo "expanded=true" >> $OUTPUT
```

**Expected:** `$HOME` expands to `/root`. `$(whoami)` expands to `root`. Unquoted delimiter allows expansion.

- [ ] Pass

---

## T10.5: JSON Ambiguity — Bash Group Command

Script body starting with `{` must not be confused with JSON config.

**DSL:**
```
### json-ambiguity
{"ssh": "orb-mcp-test-a"}

{ echo "inside group"; echo "still inside"; }
echo "group_ok=true" >> $OUTPUT
```

**Expected:** Bash `{ }` group command executes. Not treated as JSON (config already consumed). `group_ok=true`.

- [ ] Pass

---

## T10.6: Large Stdout (1000 Lines)

**DSL:**
```
### large-stdout
{"ssh": "orb-mcp-test-a", "timeout": 15}

for i in $(seq 1 1000); do
  echo "line $i: $(head -c 60 /dev/urandom | base64 | head -c 60)"
done
echo "total_lines=1000" >> $OUTPUT
```

**Expected:** 1000 lines written. `total_lines=1000`. Output file created. MCP response preview may truncate, but full content in file.

- [ ] Pass

---

## T10.7: Unicode Characters

**DSL:**
```
### unicode
{"ssh": "orb-mcp-test-a"}

echo "English: hello"
echo "Chinese: 你好世界"
echo "Japanese: こんにちは"
echo "Emoji: 🚀 🔥 ✅"
echo "Mixed: café résumé naïve"
echo "unicode=passed" >> $OUTPUT
```

**Expected:** All unicode passes through SSH intact. No encoding corruption. `unicode=passed`.

- [ ] Pass

---

## T10.8: Special Characters in $OUTPUT Values

**DSL:**
```
### special-vals
{"ssh": "orb-mcp-test-a"}

echo "path=/usr/local/bin:/usr/bin" >> $OUTPUT
echo "url=https://example.com?q=1&r=2" >> $OUTPUT
echo "equals_in_val=a=b=c" >> $OUTPUT
echo "empty_val=" >> $OUTPUT
```

**Expected:** `path` contains colons. `url` contains `?` and `&`. `equals_in_val` splits on first `=` only: value is `a=b=c`. `empty_val` has empty string.

- [ ] Pass

---

## T10.9: Very Long Single Line

**DSL:**
```
### long-line
{"ssh": "orb-mcp-test-a"}

python3 -c "print('x' * 10000)"
echo "long=ok" >> $OUTPUT
```

**Expected:** 10000-char line passes through. `long=ok`. No truncation in actual output file.

- [ ] Pass

---

## T10.10: Hyphenated Step Names

**DSL:**
```
### my-hyphen-step-name-here
{"ssh": "orb-mcp-test-a"}

echo "name=my-hyphen-step-name-here" >> $OUTPUT

### ref-hyphen

echo "{{my-hyphen-step-name-here.outputs.name}}"
echo "ref={{my-hyphen-step-name-here.outputs.name}}" >> $OUTPUT
```

**Expected:** Hyphenated names work in definition and template references. `ref=my-hyphen-step-name-here`.

- [ ] Pass

---

## T10.11: Pipes and Redirects

**DSL:**
```
### pipes-redirects
{"ssh": "orb-mcp-test-a"}

echo "hello world" | tr 'a-z' 'A-Z'
echo "hello" | wc -c | tr -d ' '
ls /etc 2>/dev/null | head -3 | sort
echo "pipe_ok=true" >> $OUTPUT
```

**Expected:** First: `HELLO WORLD`. Second: byte count. Third: sorted 3 items. All pipe chains work.

- [ ] Pass

---

## T10.12: Rapid Sequential Steps (5)

**DSL:**
```
### r1
{"ssh": "orb-mcp-test-a"}

echo "v1=1" >> $OUTPUT

### r2
{"ssh": "orb-mcp-test-a"}

echo "v2=2" >> $OUTPUT

### r3
{"ssh": "orb-mcp-test-a"}

echo "v3=3" >> $OUTPUT

### r4
{"ssh": "orb-mcp-test-a"}

echo "v4=4" >> $OUTPUT

### r5
{"ssh": "orb-mcp-test-a"}

echo "v5=5" >> $OUTPUT

### verify-rapid

echo "Sum: $(( {{r1.outputs.v1}} + {{r2.outputs.v2}} + {{r3.outputs.v3}} + {{r4.outputs.v4}} + {{r5.outputs.v5}} ))"
echo "sum=$(( {{r1.outputs.v1}} + {{r2.outputs.v2}} + {{r3.outputs.v3}} + {{r4.outputs.v4}} + {{r5.outputs.v5}} ))" >> $OUTPUT
```

**Expected:** `sum=15`. All 5 SSH connections succeed sequentially. No connection pooling issues.

- [ ] Pass

---

## T10.13: Empty Step Body with Config

A step with SSH config but no script body.

**DSL:**
```
### empty-ssh-body
{"ssh": "orb-mcp-test-a", "continue_on_error": true}
```

**Expected:** Either empty result (no script = nothing to run) or treated as help. Does not crash.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T10.1 | Single quotes | |
| T10.2 | Double quotes + dollar | |
| T10.3 | Heredoc no expansion | |
| T10.4 | Heredoc expansion | |
| T10.5 | JSON ambiguity | |
| T10.6 | Large stdout | |
| T10.7 | Unicode | |
| T10.8 | Special $OUTPUT values | |
| T10.9 | Long single line | |
| T10.10 | Hyphenated step names | |
| T10.11 | Pipes and redirects | |
| T10.12 | Rapid sequential (5) | |
| T10.13 | Empty step body | |
