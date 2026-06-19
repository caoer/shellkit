---
type: mcp-test
status: active
created: 2026-04-26
updated: 2026-04-27
target: [orb-mcp-test-a]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T04: File Transfer (scp / rsync via local blocks)

The MCP DSL has no dedicated push/pull action. File transfer is expressed
as a plain local block running `scp` or `rsync`. Aliases like
`orb-mcp-test-a` resolve via `~/.ssh/config` (run `shellkit generate-configs`
to populate it from the nix host inventory).

These tests verify that pattern works end-to-end against an orbctl VM.

---

## T04.1: Push File (scp)

**DSL:**
```
### create-local-file

echo "push-test-content-$(date +%s)" > /tmp/shellkit-test-push.txt
echo "content=$(cat /tmp/shellkit-test-push.txt)" >> $OUTPUT

### push-it

scp /tmp/shellkit-test-push.txt orb-mcp-test-a:/tmp/shellkit-test-push.txt

### verify-push
{"ssh": "orb-mcp-test-a"}

cat /tmp/shellkit-test-push.txt
echo "remote_content=$(cat /tmp/shellkit-test-push.txt)" >> $OUTPUT
```

**Expected:** scp succeeds. `remote_content` matches `content` from create-local-file. File transferred intact.

- [ ] Pass

---

## T04.2: Pull File (scp)

**DSL:**
```
### create-remote-file
{"ssh": "orb-mcp-test-a"}

echo "pull-test-marker-$(date +%s)" > /tmp/shellkit-test-pull.txt
echo "marker=$(cat /tmp/shellkit-test-pull.txt)" >> $OUTPUT

### pull-it

scp orb-mcp-test-a:/tmp/shellkit-test-pull.txt /tmp/shellkit-test-pulled.txt

### verify-pull

cat /tmp/shellkit-test-pulled.txt
echo "pulled=$(cat /tmp/shellkit-test-pulled.txt)" >> $OUTPUT
```

**Expected:** scp succeeds. Local file matches remote content.

- [ ] Pass

---

## T04.3: Push to Non-Existent Directory

**DSL:**
```
### setup-push-dir

echo "dir-test" > /tmp/shellkit-push-dir-test.txt

### push-new-dir
{"continue_on_error": true}

scp /tmp/shellkit-push-dir-test.txt orb-mcp-test-a:/tmp/shellkit-newdir/subdir/test.txt
```

**Expected:** Fails — scp does not create intermediate directories. Non-zero exit, stderr mentions missing directory.

- [ ] Pass

---

## T04.4: Pull Non-Existent File

**DSL:**
```
### pull-missing
{"continue_on_error": true}

scp orb-mcp-test-a:/tmp/this-file-does-not-exist-xyz.txt /tmp/shellkit-pulled-missing.txt
```

**Expected:** Fails with error about missing remote file. Non-zero exit.

- [ ] Pass

---

## T04.5: Push Then Modify Then Pull (round-trip)

**DSL:**
```
### create-roundtrip

echo "original-content" > /tmp/shellkit-roundtrip.txt
echo "original=$(cat /tmp/shellkit-roundtrip.txt)" >> $OUTPUT

### push-roundtrip

scp /tmp/shellkit-roundtrip.txt orb-mcp-test-a:/tmp/shellkit-roundtrip.txt

### modify-remote
{"ssh": "orb-mcp-test-a"}

echo "modified-content" >> /tmp/shellkit-roundtrip.txt
echo "remote_lines=$(wc -l < /tmp/shellkit-roundtrip.txt)" >> $OUTPUT

### pull-roundtrip

scp orb-mcp-test-a:/tmp/shellkit-roundtrip.txt /tmp/shellkit-roundtrip-back.txt

### verify-roundtrip

cat /tmp/shellkit-roundtrip-back.txt
echo "pulled_lines=$(wc -l < /tmp/shellkit-roundtrip-back.txt)" >> $OUTPUT
```

**Expected:** Original file has 1 line. After modify, remote has 2 lines. Pulled file has 2 lines.

- [ ] Pass

---

## T04.6: Push Large-ish File (scp)

**DSL:**
```
### create-large

seq 1 10000 > /tmp/shellkit-large-test.txt
echo "local_lines=$(wc -l < /tmp/shellkit-large-test.txt)" >> $OUTPUT
echo "local_bytes=$(wc -c < /tmp/shellkit-large-test.txt)" >> $OUTPUT

### push-large
{"timeout": 60}

scp /tmp/shellkit-large-test.txt orb-mcp-test-a:/tmp/shellkit-large-test.txt

### verify-large
{"ssh": "orb-mcp-test-a"}

echo "remote_lines=$(wc -l < /tmp/shellkit-large-test.txt)" >> $OUTPUT
echo "remote_bytes=$(wc -c < /tmp/shellkit-large-test.txt)" >> $OUTPUT
```

**Expected:** 10000 lines transferred. Line count and byte count match between local and remote.

- [ ] Pass

---

## T04.7: Sync a Tree with rsync

Demonstrates that rsync works the same way — just another local command.

**DSL:**
```
### create-tree

mkdir -p /tmp/shellkit-rsync-src/sub
echo "a" > /tmp/shellkit-rsync-src/a.txt
echo "b" > /tmp/shellkit-rsync-src/sub/b.txt
echo "files=$(find /tmp/shellkit-rsync-src -type f | wc -l)" >> $OUTPUT

### sync-tree

rsync -avz --delete /tmp/shellkit-rsync-src/ orb-mcp-test-a:/tmp/shellkit-rsync-dst/

### verify-tree
{"ssh": "orb-mcp-test-a"}

echo "remote_files=$(find /tmp/shellkit-rsync-dst -type f | wc -l)" >> $OUTPUT
cat /tmp/shellkit-rsync-dst/a.txt
cat /tmp/shellkit-rsync-dst/sub/b.txt
```

**Expected:** Both files present on the remote with matching contents. `remote_files=2`.

- [ ] Pass

---

## T04.8: Cleanup

**DSL:**
```
### cleanup-remote
{"ssh": "orb-mcp-test-a"}

rm -rf /tmp/shellkit-test-push.txt /tmp/shellkit-test-pull.txt /tmp/shellkit-roundtrip.txt /tmp/shellkit-large-test.txt /tmp/shellkit-rsync-dst
echo "cleaned=true" >> $OUTPUT

### cleanup-local

rm -rf /tmp/shellkit-test-push.txt /tmp/shellkit-test-pulled.txt /tmp/shellkit-push-dir-test.txt /tmp/shellkit-roundtrip.txt /tmp/shellkit-roundtrip-back.txt /tmp/shellkit-large-test.txt /tmp/shellkit-rsync-src
echo "cleaned=true" >> $OUTPUT
```

**Expected:** Both cleanup steps exit 0.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T04.1 | Push file via scp | |
| T04.2 | Pull file via scp | |
| T04.3 | Push to non-existent dir | |
| T04.4 | Pull non-existent file | |
| T04.5 | Push-modify-pull roundtrip | |
| T04.6 | Push large file | |
| T04.7 | rsync tree sync | |
| T04.8 | Cleanup | |
