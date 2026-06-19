package mcp

var helpText = `shellkit MCP — advanced examples (in-tool copy).
Basics (format, actions, config, templates) are in the tool description.
The "shellkit-expert" skill is the full external reference; the examples
below are the canonical runnable copy it mirrors.

--- Multi-step pipeline with conditional output chaining ---

### install-nginx
{"ssh": "web-1", "timeout": 120}

apt-get update && apt-get install -y nginx
echo "installed=true" >> $OUTPUT


### verify-nginx
{"ssh": "web-1"}

if [ {{install-nginx.outputs.installed}} = true ]; then
  systemctl status nginx --no-pager
  echo "status=$(systemctl is-active nginx)" >> $OUTPUT
fi


--- Python entrypoint with $OUTPUT ---

### analyze-traffic
{"ssh": "web-1", "entrypoint": "python3"}

import json, os
with open('/var/log/nginx/access.log') as f:
    lines = f.readlines()
print(f"Total requests: {len(lines)}")
with open(os.environ['OUTPUT'], 'a') as out:
    out.write(f"count={len(lines)}\n")


--- Cross-host coordination (3-step: start → benchmark → cleanup) ---

### start-iperf
{"ssh": "web-1"}

iperf3 -s -D -p 5201
sleep 1
echo "pid=$(pgrep -n iperf3)" >> $OUTPUT


### benchmark
{"ssh": "web-2", "timeout": 30}

iperf3 -c {{web-1.wan_ip}} -p 5201 -t 10 --json | tee /tmp/iperf.json
echo "bandwidth=$(jq -r '.end.sum_received.bits_per_second' /tmp/iperf.json)" >> $OUTPUT


### stop-iperf
{"ssh": "web-1"}

kill {{start-iperf.outputs.pid}}


--- Local step reading multi-host output ---

### kernel-versions
{"ssh": ["web-1", "web-2", "db-1"]}

uname -r
echo "kernel=$(uname -r)" >> $OUTPUT


### compare-kernels

cat {{kernel-versions.output}}


--- Debugging with trace (bash only) ---

Trace enables a DEBUG trap that logs every command with elapsed time.
On timeout, the last 20 commands are shown so you see exactly where it stalled.

### slow-command
{"ssh": "web-1", "timeout": 10, "trace": true}

find / -name "*.log" -size +100M 2>/dev/null


--- Tmux session driving ---

Drive interactive programs on remote hosts via tmux verb scripts.
Config: {"tmux": "host:session"} or {"tmux": ["h:s1", "h:s2"]} for fan-out.

Verbs:
  spawn CMD [ARGS...]              Create/attach tmux session, run command
  expect PATTERN [timeout=Ns]      Wait for pane content to match regex (hard fail)
  expect? PATTERN [timeout=Ns]     Soft expect: match.N=true/false in $OUTPUT
  send "TEXT"                       Send text to pane (\r \n \t \uHHHH \xHH escapes)
  key NAME[,NAME...]               Send tmux key(s): Enter, C-c, Tab, Escape, etc.
  snap [lines=N]                   Capture pane content (default 200 lines)
  kill                             Kill tmux session (idempotent)
  sleep N                          Sleep N seconds

### drive-session
{"tmux": "web-1:claude-dev"}

spawn bash
expect "\\$ "
send "echo hello world\r"
expect "hello world"
snap
kill

Output keys (N = 0-based verb position):
  snap.N                    Pane capture (base64) at verb position N
  match.N                   "true"/"false" for expect? at position N
  expect_failed.N           Pane snapshot (base64) on hard expect timeout at position N
  expect_failed.N.pattern   Pattern that failed to match

Fan-out: downstream steps reference {{step.host:session.outputs.snap.0}}.
  {{step.output}} resolves to merged file with all hosts.

Session continuity: tmux sessions persist across steps. Re-target same
  host:session without re-spawning.

Session names: must NOT contain '=' (breaks $OUTPUT). May contain ':'.

Constraints:
  - Linux + bash 4.4+ remote only (macOS bash 3.2 NOT supported as remote target)
  - Patterns matched per-line via grep -E (no multi-line patterns)
  - Fan-out capped at 32 concurrent goroutines

Session state introspection: tmux has no has-session verb — use a plain
ssh: step with 'tmux has-session -t "=<name>"' to check if a session exists.
`
