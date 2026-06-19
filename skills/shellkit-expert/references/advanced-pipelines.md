---
---
# Advanced pipelines

Multi-step composition beyond a single `ssh:` block. Everything here builds on `$OUTPUT` (export from a step) and `{{...}}` (consume in a later step) — see SKILL.md for the basics. These mirror the tool's own `### help` output; call `### help` if you need the canonical copy.

## Conditional chaining on prior output

A step branches on a value a prior step wrote to `$OUTPUT`. Substitution is literal, so the comparison is plain shell.

```
### install-nginx
{"ssh": "dev-nyc-2", "timeout": 120}

apt-get update && apt-get install -y nginx
echo "installed=true" >> $OUTPUT

### verify-nginx
{"ssh": "dev-nyc-2"}

if [ {{install-nginx.outputs.installed}} = true ]; then
  echo "status=$(systemctl is-active nginx)" >> $OUTPUT
fi
```

## Cross-host coordination

Start a service on one host, exercise it from another, clean up on the first. `{{host.wan_ip}}` resolves an inventory host's address into a remote step.

```
### start-iperf
{"ssh": "dev-nyc-2"}

iperf3 -s -D -p 5201
sleep 1
echo "pid=$(pgrep -n iperf3)" >> $OUTPUT

### benchmark
{"ssh": "dev-nyc-3", "timeout": 30}

iperf3 -c {{dev-nyc-2.wan_ip}} -p 5201 -t 10 --json | tee /tmp/iperf.json
echo "bw=$(jq -r '.end.sum_received.bits_per_second' /tmp/iperf.json)" >> $OUTPUT

### stop-iperf
{"ssh": "dev-nyc-2"}

kill {{start-iperf.outputs.pid}}
```

## Non-bash entrypoints with $OUTPUT

`$OUTPUT` is just a file path in the environment — any language can append to it. Set `entrypoint` to match the body.

```
### analyze-traffic
{"ssh": "dev-nyc-2", "entrypoint": "python3"}

import json, os
with open('/var/log/nginx/access.log') as f:
    lines = f.readlines()
print(f"Total requests: {len(lines)}")
with open(os.environ['OUTPUT'], 'a') as out:
    out.write(f"count={len(lines)}\n")
```

Note: command **trace on timeout** is bash-only (POSIX `sh` and other interpreters lack the DEBUG trap). A non-bash step that times out gives you stdout up to the kill, but no per-command trace.

A fan-out step's merged output file is `{{step.output}}`, consumed by a local block (no config) for post-processing — see the "Fan out, then post-process locally" pattern in SKILL.md.

## Debugging a stuck step with trace

`trace` (bash only, on by default) injects a DEBUG trap that logs every command with elapsed time. On timeout the last 20 commands are shown so you see exactly where it stalled.

```
### slow
{"ssh": "dev-nyc-2", "timeout": 10, "trace": true}

find / -name "*.log" -size +100M 2>/dev/null
```
