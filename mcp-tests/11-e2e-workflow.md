---
type: mcp-test
status: active
created: 2026-04-26
target: [orb-mcp-test-a, orb-mcp-test-b]
depends-on: [00-provision]
tags: [type/mcp-test, domain/infra, domain/tooling]
---

# T11: End-to-End Workflows

Multi-step integration tests combining SSH, local (including scp/rsync file transfers), cross-step refs, fan-out, and error handling in realistic workflows.

---

## T11.1: Deploy and Verify Workflow

Simulate deploying a config file and verifying it's applied.

**DSL:**
```
### create-config

cat > /tmp/shellkit-e2e-config.txt << 'CONF'
server_name=test-app
port=8080
workers=4
debug=false
CONF
echo "config_created=true" >> $OUTPUT

### push-config

scp /tmp/shellkit-e2e-config.txt orb-mcp-test-a:/tmp/app-config.txt

### apply-config
{"ssh": "orb-mcp-test-a"}

source /tmp/app-config.txt
echo "Config applied: server=$server_name port=$port workers=$workers"
echo "server_name=$server_name" >> $OUTPUT
echo "port=$port" >> $OUTPUT
echo "workers=$workers" >> $OUTPUT

### verify-config

echo "Server: {{apply-config.outputs.server_name}}"
echo "Port: {{apply-config.outputs.port}}"
echo "Workers: {{apply-config.outputs.workers}}"
test "{{apply-config.outputs.port}}" = "8080" && echo "PASS" || echo "FAIL"
echo "verified=true" >> $OUTPUT
```

**Expected:** Config created locally, pushed, sourced on remote, values read back. `server_name=test-app`, `port=8080`, `workers=4`, `verified=true`.

- [ ] Pass

---

## T11.2: Cross-Host Data Collection

Gather system info from multiple hosts and merge locally.

**DSL:**
```
### gather-info
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

echo "hostname=$(hostname)" >> $OUTPUT
echo "kernel=$(uname -r)" >> $OUTPUT
echo "uptime=$(cat /proc/uptime | cut -d' ' -f1)" >> $OUTPUT
echo "memory=$(free -m 2>/dev/null | awk '/Mem:/{print $2}' || echo unknown)" >> $OUTPUT
echo "cpu_count=$(nproc)" >> $OUTPUT

### build-report

echo "=== Fleet Report ==="
echo "Host A: {{gather-info.orb-mcp-test-a.outputs.hostname}}"
echo "  Kernel: {{gather-info.orb-mcp-test-a.outputs.kernel}}"
echo "  Memory: {{gather-info.orb-mcp-test-a.outputs.memory}}MB"
echo "  CPUs: {{gather-info.orb-mcp-test-a.outputs.cpu_count}}"
echo "Host B: {{gather-info.orb-mcp-test-b.outputs.hostname}}"
echo "  Kernel: {{gather-info.orb-mcp-test-b.outputs.kernel}}"
echo "  Memory: {{gather-info.orb-mcp-test-b.outputs.memory}}MB"
echo "  CPUs: {{gather-info.orb-mcp-test-b.outputs.cpu_count}}"
echo "report=complete" >> $OUTPUT
```

**Expected:** `report=complete`. All per-host fields populated. Report shows data from both hosts.

- [ ] Pass

---

## T11.3: Install → Test → Report Pipeline

Install a package, verify it works, report version.

**DSL:**
```
### install-package
{"ssh": "orb-mcp-test-a", "timeout": 60}

export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq sqlite3 > /dev/null 2>&1
echo "installed=$(which sqlite3 >/dev/null && echo true || echo false)" >> $OUTPUT

### run-sql
{"ssh": "orb-mcp-test-a"}

sqlite3 :memory: "CREATE TABLE test(id INTEGER, name TEXT); INSERT INTO test VALUES(1,'hello'),(2,'world'); SELECT * FROM test;"
echo "version=$(sqlite3 --version | awk '{print $1}')" >> $OUTPUT
echo "query_ok=true" >> $OUTPUT

### report-local

echo "SQLite installed: {{install-package.outputs.installed}}"
echo "Version: {{run-sql.outputs.version}}"
echo "Query test: {{run-sql.outputs.query_ok}}"
echo "pipeline=complete" >> $OUTPUT
```

**Expected:** sqlite3 installed, query runs, version captured. `pipeline=complete`.

- [ ] Pass

---

## T11.4: File Round-Trip with Transformation

Push a file, transform it on remote, pull back the result.

**DSL:**
```
### generate-data

echo "alice,30,engineering" > /tmp/shellkit-e2e-data.csv
echo "bob,25,marketing" >> /tmp/shellkit-e2e-data.csv
echo "charlie,35,engineering" >> /tmp/shellkit-e2e-data.csv
echo "diana,28,marketing" >> /tmp/shellkit-e2e-data.csv
echo "rows=4" >> $OUTPUT

### push-data

scp /tmp/shellkit-e2e-data.csv orb-mcp-test-a:/tmp/data.csv

### transform-data
{"ssh": "orb-mcp-test-a", "entrypoint": "python3"}

import csv, json, os
rows = []
with open('/tmp/data.csv') as f:
    for name, age, dept in csv.reader(f):
        rows.append({"name": name, "age": int(age), "dept": dept})
with open('/tmp/data.json', 'w') as f:
    json.dump(rows, f, indent=2)
with open(os.environ['OUTPUT'], 'a') as out:
    out.write(f"count={len(rows)}\n")
    out.write(f"avg_age={sum(r['age'] for r in rows)/len(rows):.1f}\n")

### pull-result

scp orb-mcp-test-a:/tmp/data.json /tmp/shellkit-e2e-result.json

### verify-result

cat /tmp/shellkit-e2e-result.json
echo "local_valid=$(python3 -c 'import json; d=json.load(open(\"/tmp/shellkit-e2e-result.json\")); print(len(d))' 2>/dev/null || echo error)" >> $OUTPUT
```

**Expected:** CSV pushed, transformed to JSON via Python, pulled back. JSON has 4 entries. `count=4`, `avg_age=29.5`.

- [ ] Pass

---

## T11.5: Error Recovery Workflow

Attempt operation, handle failure, retry with fix.

**DSL:**
```
### attempt-missing
{"ssh": "orb-mcp-test-a", "continue_on_error": true}

cat /tmp/shellkit-e2e-missing.conf
echo "found=true" >> $OUTPUT

### handle-missing
{"ssh": "orb-mcp-test-a"}

if [ ! -f /tmp/shellkit-e2e-missing.conf ]; then
  echo "File missing, creating default"
  echo "mode=default" > /tmp/shellkit-e2e-missing.conf
  echo "created=true" >> $OUTPUT
else
  echo "created=false" >> $OUTPUT
fi

### retry-read
{"ssh": "orb-mcp-test-a"}

cat /tmp/shellkit-e2e-missing.conf
echo "content=$(cat /tmp/shellkit-e2e-missing.conf)" >> $OUTPUT
echo "success=true" >> $OUTPUT
```

**Expected:** First read fails (continue_on_error). Handler creates file. Retry succeeds. `content=mode=default`, `success=true`.

- [ ] Pass

---

## T11.6: Multi-Host Deployment

Deploy config to both hosts and verify.

**DSL:**
```
### create-deploy-config

echo "version=2.0" > /tmp/shellkit-e2e-deploy.conf
echo "env=test" >> /tmp/shellkit-e2e-deploy.conf

### deploy-a

scp /tmp/shellkit-e2e-deploy.conf orb-mcp-test-a:/tmp/app.conf

### deploy-b

scp /tmp/shellkit-e2e-deploy.conf orb-mcp-test-b:/tmp/app.conf

### verify-deploy
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

source /tmp/app.conf
echo "host=$(hostname) version=$version env=$env"
echo "version=$version" >> $OUTPUT
echo "env=$env" >> $OUTPUT

### check-consistency

A_VER="{{verify-deploy.orb-mcp-test-a.outputs.version}}"
B_VER="{{verify-deploy.orb-mcp-test-b.outputs.version}}"
if [ "$A_VER" = "$B_VER" ]; then
  echo "CONSISTENT: both hosts running v$A_VER"
  echo "consistent=true" >> $OUTPUT
else
  echo "INCONSISTENT: A=$A_VER B=$B_VER"
  echo "consistent=false" >> $OUTPUT
fi
```

**Expected:** Config pushed to both hosts. Both report `version=2.0`. `consistent=true`.

- [ ] Pass

---

## T11.7: Cleanup All E2E Artifacts

**DSL:**
```
### cleanup-remote
{"ssh": ["orb-mcp-test-a", "orb-mcp-test-b"]}

rm -f /tmp/shellkit-e2e-* /tmp/app-config.txt /tmp/data.csv /tmp/data.json /tmp/app.conf
echo "cleaned=true" >> $OUTPUT

### cleanup-local

rm -f /tmp/shellkit-e2e-*
echo "cleaned=true" >> $OUTPUT
```

**Expected:** All temp files removed. Exit 0 on both hosts and locally.

- [ ] Pass

---

## Summary

| # | Test | Status |
|---|------|--------|
| T11.1 | Deploy and verify | |
| T11.2 | Cross-host data collection | |
| T11.3 | Install → test → report | |
| T11.4 | File round-trip + transform | |
| T11.5 | Error recovery | |
| T11.6 | Multi-host deployment | |
| T11.7 | Cleanup | |
