package mcp

import (
	"strings"
	"testing"
)

func TestCallStatus(t *testing.T) {
	if s := (&CallEntry{}).CallStatus(); s != "ok" {
		t.Errorf("empty entry: got %s, want ok", s)
	}
	if s := (&CallEntry{Error: "boom"}).CallStatus(); s != "error" {
		t.Errorf("error entry: got %s, want error", s)
	}
	if s := (&CallEntry{Results: []ResultBrief{{ExitCode: 1}}}).CallStatus(); s != "fail" {
		t.Errorf("fail entry: got %s, want fail", s)
	}
}

func TestStepSummary(t *testing.T) {
	e := &CallEntry{
		Steps: []StepBrief{
			{Name: "a", Hosts: []string{"h1", "h2"}},
			{Name: "b", Hosts: []string{"h2", "h3"}},
		},
	}
	s := e.StepSummary()
	if s != "2 steps → h1, h2, h3" {
		t.Errorf("unexpected summary: %q", s)
	}
}

func TestInputPreview(t *testing.T) {
	e := &CallEntry{Input: "### step\n{\"ssh\": \"host\"}\ncommand here"}
	p := e.InputPreview(50)
	if p == "" {
		t.Error("preview empty")
	}
	if !strings.Contains(p, "↵") {
		t.Errorf("expected newline replacement, got %q", p)
	}
}

func TestStepActionString(t *testing.T) {
	tests := []struct {
		action StepAction
		want   string
	}{
		{ActionHelp, "help"},
		{ActionList, "list"},
		{ActionSSH, "ssh"},
		{ActionLocal, "local"},
		{ActionTmux, "tmux"},
	}
	for _, tt := range tests {
		if got := tt.action.String(); got != tt.want {
			t.Errorf("StepAction(%d).String() = %q, want %q", tt.action, got, tt.want)
		}
	}
}
