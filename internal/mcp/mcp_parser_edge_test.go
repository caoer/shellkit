package mcp

import (
	"strings"
	"testing"
)

func TestParseDSL_LocalScp(t *testing.T) {
	// File transfer is now expressed as a plain local block running scp/rsync.
	input := `### pull-logs

scp web-1:/var/log/syslog ./syslog.txt
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	s := steps[0]
	if s.Action != ActionLocal {
		t.Errorf("action: want ActionLocal, got %d", s.Action)
	}
	if s.Body != "scp web-1:/var/log/syslog ./syslog.txt" {
		t.Errorf("body: got %q", s.Body)
	}
	if s.Hosts != nil {
		t.Errorf("hosts: want nil, got %v", s.Hosts)
	}
}

func TestParseDSL_CheckRejectedSingle(t *testing.T) {
	input := `### probe-host
{"check": "web-1"}
`
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error: 'check' command was removed")
	}
	if !strings.Contains(err.Error(), "ssh step") {
		t.Errorf("error should steer to an ssh step, got: %s", err)
	}
}

func TestParseDSL_CheckRejectedAll(t *testing.T) {
	input := `### probe-fleet
{"check": true}
`
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error: 'check' command was removed")
	}
	if !strings.Contains(err.Error(), "ssh step") {
		t.Errorf("error should steer to an ssh step, got: %s", err)
	}
}

func TestParseDSL_EmptyInput(t *testing.T) {
	_, err := ParseDSL("")
	if err == nil {
		t.Fatal("want error for empty input")
	}
}

func TestParseDSL_NoSteps(t *testing.T) {
	_, err := ParseDSL("just some text\nwith no ### blocks\n")
	if err == nil {
		t.Fatal("want error for input without steps")
	}
}

func TestParseDSL_JSONAmbiguity_BashGroup(t *testing.T) {
	// bash { echo } is not valid JSON — should be treated as body
	input := `### bash-group

{ echo "hello"; echo "world"; }
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionLocal {
		t.Errorf("action: want ActionLocal, got %d", s.Action)
	}
	if s.Body != `{ echo "hello"; echo "world"; }` {
		t.Errorf("body: got %q", s.Body)
	}
}

func TestParseDSL_JSONAmbiguity_ValidJSON(t *testing.T) {
	// valid JSON should be parsed as config
	input := `### remote-cmd
{"ssh": "host-1"}

echo hello
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionSSH {
		t.Errorf("action: want ActionSSH, got %d", s.Action)
	}
	if s.Body != "echo hello" {
		t.Errorf("body: got %q", s.Body)
	}
}

func TestParseDSL_ConfigNoBody(t *testing.T) {
	input := `### all-hosts
{"list": true}
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionList {
		t.Errorf("action: want ActionList, got %d", s.Action)
	}
	if s.Body != "" {
		t.Errorf("body: want empty, got %q", s.Body)
	}
}

func TestParseDSL_BodyNoConfig(t *testing.T) {
	input := `### local-cmd

echo "running locally"
ls -la
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionLocal {
		t.Errorf("action: want ActionLocal, got %d", s.Action)
	}
	if s.Hosts != nil {
		t.Errorf("hosts: want nil, got %v", s.Hosts)
	}
}

func TestParseDSL_TimeoutAndContinueOnError(t *testing.T) {
	input := `### slow-cmd
{"ssh": "web-1", "timeout": 120, "continue_on_error": true}

sleep 100
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Config.Timeout != 120 {
		t.Errorf("timeout: want 120, got %d", s.Config.Timeout)
	}
	if !s.Config.ContinueOnError {
		t.Error("continue_on_error: want true")
	}
}

func TestParseDSL_MultipleBlankLines(t *testing.T) {
	input := `### step-a
{"ssh": "host-1"}



echo "lots of blank lines above"



### step-b

echo "local step"
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].Body != `echo "lots of blank lines above"` {
		t.Errorf("step-a body: got %q", steps[0].Body)
	}
	if steps[1].Body != `echo "local step"` {
		t.Errorf("step-b body: got %q", steps[1].Body)
	}
}

func TestParseDSL_HelpExplicit(t *testing.T) {
	// no config, no body → help
	input := `### help
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionHelp {
		t.Errorf("want ActionHelp, got %d", steps[0].Action)
	}
}

func TestParseDSL_HelpWithExtraBlankLines(t *testing.T) {
	input := `### help


`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionHelp {
		t.Errorf("want ActionHelp, got %d", steps[0].Action)
	}
	if steps[0].Body != "" {
		t.Errorf("body: want empty, got %q", steps[0].Body)
	}
}

func TestParseDSL_NonBashEntrypoints(t *testing.T) {
	input := `### py-script
{"ssh": "host-1", "entrypoint": "python3"}

import os
print("hello")

### node-script
{"ssh": "host-1", "entrypoint": "node"}

console.log("hello")
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Config.Entrypoint != "python3" {
		t.Errorf("step 0 entrypoint: want python3, got %s", steps[0].Config.Entrypoint)
	}
	if steps[1].Config.Entrypoint != "node" {
		t.Errorf("step 1 entrypoint: want node, got %s", steps[1].Config.Entrypoint)
	}
}

func TestParseDSL_ListNoFilter(t *testing.T) {
	input := `### all-servers
{"list": true}
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionList {
		t.Errorf("action: want ActionList, got %d", steps[0].Action)
	}
	if steps[0].Config.Filter != "" {
		t.Errorf("filter: want empty, got %s", steps[0].Config.Filter)
	}
}

func TestParseDSL_HeredocInBody(t *testing.T) {
	input := `### write-file
{"ssh": "host-1"}

cat > /tmp/test.txt << 'EOF'
line 1
line 2
EOF
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionSSH {
		t.Errorf("action: want ActionSSH, got %d", steps[0].Action)
	}
	if !strings.Contains(steps[0].Body, "EOF") {
		t.Errorf("body should contain heredoc markers, got %q", steps[0].Body)
	}
}

func TestParseDSL_PipesAndRedirects(t *testing.T) {
	input := `### complex-cmd
{"ssh": "host-1"}

ps aux | grep nginx | awk '{print $2}' > /tmp/pids.txt 2>&1
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(steps[0].Body, "|") || !strings.Contains(steps[0].Body, ">") {
		t.Errorf("body should preserve pipes and redirects, got %q", steps[0].Body)
	}
}

func TestParseDSL_FullExample(t *testing.T) {
	// Parse the complete example from DSL design doc
	input := `### help


### all-servers
{"list": true}


### dev-servers
{"list": true, "filter": "role=dev"}


### check-nix
{"ssh": "web-1", "entrypoint": "bash"}

which nix 2>/dev/null && nix --version


### upload-config

scp ./nginx.conf web-1:/etc/nginx/nginx.conf


### download-logs

scp web-1:/var/log/nginx/error.log ./error.log


### kernel-versions
{"ssh": ["web-1", "web-2", "db-1"], "entrypoint": "bash"}

uname -r
echo "kernel=$(uname -r)" >> $OUTPUT


### compare-kernels

cat {{kernel-versions.output}}
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	expected := []struct {
		name   string
		action StepAction
	}{
		{"help", ActionHelp},
		{"all-servers", ActionList},
		{"dev-servers", ActionList},
		{"check-nix", ActionSSH},
		{"upload-config", ActionLocal},
		{"download-logs", ActionLocal},
		{"kernel-versions", ActionSSH},
		{"compare-kernels", ActionLocal},
	}

	if len(steps) != len(expected) {
		t.Fatalf("want %d steps, got %d", len(expected), len(steps))
	}
	for i, exp := range expected {
		if steps[i].Name != exp.name {
			t.Errorf("step %d name: want %s, got %s", i, exp.name, steps[i].Name)
		}
		if steps[i].Action != exp.action {
			t.Errorf("step %d (%s) action: want %d, got %d", i, exp.name, exp.action, steps[i].Action)
		}
	}

	// kernel-versions should have 3 hosts
	if len(steps[6].Hosts) != 3 {
		t.Errorf("kernel-versions hosts: want 3, got %d", len(steps[6].Hosts))
	}
}
