package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

type StepConfig struct {
	SSHRaw          json.RawMessage `json:"ssh,omitempty"`
	ListRaw         json.RawMessage `json:"list,omitempty"`
	Filter          string          `json:"filter,omitempty"`
	CheckRaw        json.RawMessage `json:"check,omitempty"`
	Entrypoint      string          `json:"entrypoint,omitempty"`
	Timeout         int             `json:"timeout,omitempty"`
	Trace           *bool           `json:"trace,omitempty"`
	ContinueOnError bool            `json:"continue_on_error,omitempty"`
	TmuxRaw         json.RawMessage `json:"tmux,omitempty"`
	Jump            string          `json:"jump,omitempty"`     // SSH ProxyJump — hop through this host to reach the target
	Identity        string          `json:"identity,omitempty"` // SSH identity file (key) — path like ~/.ssh/my_key
}

type StepAction int

const (
	ActionHelp StepAction = iota
	ActionList
	ActionSSH
	ActionLocal
	ActionTmux
)

func (a StepAction) String() string {
	switch a {
	case ActionHelp:
		return "help"
	case ActionList:
		return "list"
	case ActionSSH:
		return "ssh"
	case ActionLocal:
		return "local"
	case ActionTmux:
		return "tmux"
	default:
		return "unknown"
	}
}

type Step struct {
	Name   string
	Config StepConfig
	Body   string
	Action StepAction
	Hosts  []string
}

func (c *StepConfig) sshHosts() []string {
	if c.SSHRaw == nil {
		return nil
	}
	var single string
	if json.Unmarshal(c.SSHRaw, &single) == nil {
		return []string{single}
	}
	var multi []string
	if json.Unmarshal(c.SSHRaw, &multi) == nil {
		return multi
	}
	return nil
}

func (c *StepConfig) tmuxTargets() []string {
	if c.TmuxRaw == nil {
		return nil
	}
	var single string
	if json.Unmarshal(c.TmuxRaw, &single) == nil {
		return []string{single}
	}
	var multi []string
	if json.Unmarshal(c.TmuxRaw, &multi) == nil {
		return multi
	}
	return nil
}

func validateTmuxTarget(target string) (host, session string, err error) {
	if target == "" {
		return "", "", fmt.Errorf("tmux target required")
	}
	idx := strings.IndexByte(target, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("tmux target must be host:session, got %q", target)
	}
	host = target[:idx]
	session = target[idx+1:]
	if host == "" {
		return "", "", fmt.Errorf("tmux target host is empty in %q", target)
	}
	if session == "" {
		return "", "", fmt.Errorf("tmux target session is empty in %q", target)
	}
	if strings.Contains(session, "=") {
		return "", "", fmt.Errorf("tmux session name cannot contain '=' (corrupts output parsing), got %q", target)
	}
	return host, session, nil
}

func classifyStep(s *Step) error {
	c := &s.Config
	switch {
	case c.ListRaw != nil:
		s.Action = ActionList
	case c.CheckRaw != nil:
		// The 'check' probe was removed: agents verify by running real
		// commands over ssh, not a connectivity ping. Fail loudly with
		// the correct path rather than silently ignoring the field.
		return fmt.Errorf("step %q: the 'check' command was removed — run your verification commands through an ssh step instead, e.g. {\"ssh\": \"host\"}", s.Name)
	case c.TmuxRaw != nil:
		if c.SSHRaw != nil {
			return fmt.Errorf("step %q: tmux and ssh are mutually exclusive", s.Name)
		}
		s.Action = ActionTmux
		s.Hosts = c.tmuxTargets()
		for _, t := range s.Hosts {
			if _, _, err := validateTmuxTarget(t); err != nil {
				return fmt.Errorf("step %q: %w", s.Name, err)
			}
		}
	case c.SSHRaw != nil:
		s.Action = ActionSSH
		s.Hosts = c.sshHosts()
	case s.Body != "":
		s.Action = ActionLocal
	default:
		s.Action = ActionHelp
	}
	return nil
}

func ParseDSL(input string) ([]Step, error) {
	lines := strings.Split(input, "\n")
	var steps []Step
	seen := make(map[string]bool)

	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "### ") {
			i++
			continue
		}

		name := strings.TrimSpace(line[4:])
		if name == "" {
			return nil, fmt.Errorf("line %d: empty step name", i+1)
		}
		if seen[name] {
			return nil, fmt.Errorf("line %d: duplicate step name %q", i+1, name)
		}
		seen[name] = true

		step := Step{Name: name}
		i++

		// skip blank lines after ###
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}

		// try JSON config line
		if i < len(lines) {
			candidate := strings.TrimSpace(lines[i])
			if strings.HasPrefix(candidate, "{") {
				if err := json.Unmarshal([]byte(candidate), &step.Config); err != nil {
					// not valid JSON — treat as body
				} else {
					i++
				}
			}
		}

		// skip blank line between config and body
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}

		// collect body until next ### or EOF
		var bodyLines []string
		for i < len(lines) {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "### ") {
				break
			}
			bodyLines = append(bodyLines, lines[i])
			i++
		}

		// trim trailing blank lines from body
		for len(bodyLines) > 0 && strings.TrimSpace(bodyLines[len(bodyLines)-1]) == "" {
			bodyLines = bodyLines[:len(bodyLines)-1]
		}

		step.Body = strings.Join(bodyLines, "\n")
		if err := classifyStep(&step); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		steps = append(steps, step)
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("no steps found (blocks start with '### ')")
	}
	return steps, nil
}
