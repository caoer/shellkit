package mcp

import (
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/inventory"
)

func TestParseDSL_Help(t *testing.T) {
	steps, err := ParseDSL("### help\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	if steps[0].Action != ActionHelp {
		t.Errorf("want ActionHelp, got %d", steps[0].Action)
	}
}

func TestParseDSL_SSH(t *testing.T) {
	input := `### check-nix
{"ssh": "web-1"}

which nix 2>/dev/null && nix --version
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	s := steps[0]
	if s.Name != "check-nix" {
		t.Errorf("name: want check-nix, got %s", s.Name)
	}
	if s.Action != ActionSSH {
		t.Errorf("action: want ActionSSH, got %d", s.Action)
	}
	if len(s.Hosts) != 1 || s.Hosts[0] != "web-1" {
		t.Errorf("hosts: want [web-1], got %v", s.Hosts)
	}
	if s.Body != "which nix 2>/dev/null && nix --version" {
		t.Errorf("body: got %q", s.Body)
	}
}

func TestParseDSL_MultiStep(t *testing.T) {
	input := `### find-files
{"ssh": "web-1"}

find /etc -name "*.conf"

### filter-local

grep "nginx" {{find-files.output}}

### upload-config

scp ./nginx.conf web-1:/etc/nginx/nginx.conf
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	if steps[0].Action != ActionSSH {
		t.Errorf("step 0: want SSH, got %d", steps[0].Action)
	}
	if steps[1].Action != ActionLocal {
		t.Errorf("step 1: want Local, got %d", steps[1].Action)
	}
	if steps[2].Action != ActionLocal {
		t.Errorf("step 2: want Local, got %d", steps[2].Action)
	}
	if steps[2].Body != "scp ./nginx.conf web-1:/etc/nginx/nginx.conf" {
		t.Errorf("step 2 body: got %q", steps[2].Body)
	}
}

func TestParseDSL_FanOut(t *testing.T) {
	input := `### kernel-check
{"ssh": ["web-1", "web-2", "db-1"]}

uname -r
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps[0].Hosts) != 3 {
		t.Errorf("want 3 hosts, got %d", len(steps[0].Hosts))
	}
}

func TestParseDSL_List(t *testing.T) {
	input := `### dev-servers
{"list": true, "filter": "role=dev"}
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionList {
		t.Errorf("want ActionList, got %d", steps[0].Action)
	}
	if steps[0].Config.Filter != "role=dev" {
		t.Errorf("filter: want role=dev, got %s", steps[0].Config.Filter)
	}
}

func TestParseDSL_DuplicateNames(t *testing.T) {
	input := `### step-a
{"ssh": "host-1"}

echo a

### step-a
{"ssh": "host-2"}

echo b
`
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error for duplicate names")
	}
}

func TestParseDSL_Entrypoint(t *testing.T) {
	input := `### analyze
{"ssh": "web-1", "entrypoint": "python3"}

import os
print("hello")
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Config.Entrypoint != "python3" {
		t.Errorf("entrypoint: want python3, got %s", steps[0].Config.Entrypoint)
	}
}

func TestParseOutputs(t *testing.T) {
	raw := "pid=12345\nversion=3.2.1\nstatus=active\n"
	outputs := ParseOutputs(raw)
	if outputs["pid"] != "12345" {
		t.Errorf("pid: want 12345, got %s", outputs["pid"])
	}
	if outputs["version"] != "3.2.1" {
		t.Errorf("version: want 3.2.1, got %s", outputs["version"])
	}
}

func TestOutputStore_Resolve(t *testing.T) {
	servers := []inventory.Server{
		{Name: "web-1", IP: "1.2.3.4", Port: 22, User: "root", Provider: "example-cloud"},
	}
	store, err := NewOutputStore(servers)
	if err != nil {
		t.Fatal(err)
	}

	// store a step result
	store.Store(&StepResult{
		Name:     "step-a",
		FilePath: "/tmp/test/step-a.out",
		Outputs:  map[string]string{"pid": "9999"},
	})

	// resolve host property
	resolved, err := store.Resolve("ip is {{web-1.wan_ip}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "ip is 1.2.3.4" {
		t.Errorf("host resolve: got %q", resolved)
	}

	// resolve step output
	resolved, err = store.Resolve("kill {{step-a.outputs.pid}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "kill 9999" {
		t.Errorf("output resolve: got %q", resolved)
	}

	// resolve step file path
	resolved, err = store.Resolve("cat {{step-a.output}}")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "cat /tmp/test/step-a.out" {
		t.Errorf("path resolve: got %q", resolved)
	}
}

func TestLocalExecution(t *testing.T) {
	servers := []inventory.Server{}
	store, err := NewOutputStore(servers)
	if err != nil {
		t.Fatal(err)
	}

	input := `### echo-test

echo "hello world"
echo "key=value123" >> $OUTPUT
`
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(store, servers)
	results, err := exec.Execute(t.Context(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.ExitCode != 0 {
		t.Errorf("exit code: want 0, got %d (stderr: %s)", r.ExitCode, r.Stderr)
	}
	if r.Outputs["key"] != "value123" {
		t.Errorf("output key: want value123, got %q", r.Outputs["key"])
	}
}

func TestParseDSL_TmuxSingle(t *testing.T) {
	input := "### send-keys\n{\"tmux\": \"web-1:main\"}\n\necho hello\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	s := steps[0]
	if s.Action != ActionTmux {
		t.Errorf("action: want ActionTmux, got %d", s.Action)
	}
	if len(s.Hosts) != 1 || s.Hosts[0] != "web-1:main" {
		t.Errorf("hosts: want [web-1:main], got %v", s.Hosts)
	}
}

func TestParseDSL_TmuxFanOut(t *testing.T) {
	input := "### broadcast\n{\"tmux\": [\"a:s1\", \"b:s2\"]}\n\necho hi\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionTmux {
		t.Errorf("action: want ActionTmux, got %d", s.Action)
	}
	if len(s.Hosts) != 2 {
		t.Fatalf("want 2 hosts, got %d", len(s.Hosts))
	}
	if s.Hosts[0] != "a:s1" || s.Hosts[1] != "b:s2" {
		t.Errorf("hosts: want [a:s1 b:s2], got %v", s.Hosts)
	}
}

func TestParseDSL_TmuxRejectSSHCoexist(t *testing.T) {
	input := "### conflict\n{\"tmux\": \"foo:bar\", \"ssh\": \"baz\"}\n\necho x\n"
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error for tmux+ssh coexistence")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutually exclusive, got: %s", err)
	}
}

func TestParseDSL_TmuxRejectNoColon(t *testing.T) {
	input := "### bad-target\n{\"tmux\": \"no-colon\"}\n\necho x\n"
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error for target without colon")
	}
	if !strings.Contains(err.Error(), "host:session") {
		t.Errorf("error should mention host:session, got: %s", err)
	}
}

func TestParseDSL_TmuxRejectEqualsInSession(t *testing.T) {
	input := "### bad-session\n{\"tmux\": \"foo:bad=name\"}\n\necho x\n"
	_, err := ParseDSL(input)
	if err == nil {
		t.Fatal("want error for = in session name")
	}
	if !strings.Contains(err.Error(), "'='") {
		t.Errorf("error should mention =, got: %s", err)
	}
}

func TestParseDSL_TmuxColonInSession(t *testing.T) {
	input := "### colon-session\n{\"tmux\": \"foo:claude:dev\"}\n\necho x\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Action != ActionTmux {
		t.Errorf("action: want ActionTmux, got %d", s.Action)
	}
	if len(s.Hosts) != 1 || s.Hosts[0] != "foo:claude:dev" {
		t.Errorf("hosts: want [foo:claude:dev], got %v", s.Hosts)
	}
	host, session, err := validateTmuxTarget("foo:claude:dev")
	if err != nil {
		t.Fatal(err)
	}
	if host != "foo" {
		t.Errorf("host: want foo, got %s", host)
	}
	if session != "claude:dev" {
		t.Errorf("session: want claude:dev, got %s", session)
	}
}

func TestParseDSL_TmuxBodyLazy(t *testing.T) {
	input := "### lazy-body\n{\"tmux\": \"host:sess\"}\n\nthis is not a valid verb but classify should not care\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != ActionTmux {
		t.Errorf("action: want ActionTmux, got %d", steps[0].Action)
	}
}

func TestParseDSL_JumpField(t *testing.T) {
	input := "### via-jump\n{\"ssh\": \"root@10.0.0.5:22\", \"jump\": \"bastion\"}\n\nuname -a\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	s := steps[0]
	if s.Action != ActionSSH {
		t.Errorf("action: want ActionSSH, got %d", s.Action)
	}
	if s.Config.Jump != "bastion" {
		t.Errorf("jump: want 'bastion', got %q", s.Config.Jump)
	}
	if len(s.Hosts) != 1 || s.Hosts[0] != "root@10.0.0.5:22" {
		t.Errorf("hosts: want [root@10.0.0.5:22], got %v", s.Hosts)
	}
}

func TestParseDSL_JumpWithFanout(t *testing.T) {
	input := "### fan-jump\n{\"ssh\": [\"host-a\", \"host-b\"], \"jump\": \"proxy\"}\n\nhostname\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	s := steps[0]
	if s.Config.Jump != "proxy" {
		t.Errorf("jump: want 'proxy', got %q", s.Config.Jump)
	}
	if len(s.Hosts) != 2 {
		t.Errorf("hosts: want 2, got %d", len(s.Hosts))
	}
}

func TestParseDSL_JumpOmitted(t *testing.T) {
	input := "### no-jump\n{\"ssh\": \"host\"}\n\nhostname\n"
	steps, err := ParseDSL(input)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Config.Jump != "" {
		t.Errorf("jump should be empty when omitted, got %q", steps[0].Config.Jump)
	}
}
