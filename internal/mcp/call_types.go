package mcp

import (
	"fmt"
	"strings"
	"time"
)

// CallEntry represents one MCP tool invocation.
type CallEntry struct {
	ID         string        `json:"id"`
	Timestamp  time.Time     `json:"ts"`
	SessionID  string        `json:"session_id,omitempty"`
	Input      string        `json:"input"`
	Steps      []StepBrief   `json:"steps"`
	Results    []ResultBrief `json:"results,omitempty"`
	DurationMs int64         `json:"duration_ms"`
	Error      string        `json:"error,omitempty"`
}

// StepBrief is a compact representation of a parsed step.
type StepBrief struct {
	Name        string            `json:"name"`
	Action      string            `json:"action"`
	Hosts       []string          `json:"hosts,omitempty"`
	BodyPreview string            `json:"body,omitempty"`   // first non-empty body line, truncated
	Params      map[string]string `json:"params,omitempty"` // config params: timeout, trace, entrypoint, etc.
}

// ResultBrief is a compact representation of a step result.
type ResultBrief struct {
	Name     string            `json:"name"`
	Host     string            `json:"host,omitempty"`
	ExitCode int               `json:"exit_code"`
	Stdout   string            `json:"stdout,omitempty"`
	Stderr   string            `json:"stderr,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	Error    string            `json:"error,omitempty"`
	TimedOut bool              `json:"timed_out,omitempty"`
}

// CallStatus returns a summary status string for a call entry.
func (e *CallEntry) CallStatus() string {
	if e.Error != "" {
		return "error"
	}
	for _, r := range e.Results {
		if r.ExitCode != 0 || r.TimedOut || r.Error != "" {
			return "fail"
		}
	}
	return "ok"
}

// StepSummary returns "3 steps → host1, host2" style summary.
func (e *CallEntry) StepSummary() string {
	if len(e.Steps) == 0 {
		return "no steps"
	}
	var hosts []string
	seen := make(map[string]bool)
	for _, s := range e.Steps {
		for _, h := range s.Hosts {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	parts := fmt.Sprintf("%d steps", len(e.Steps))
	if len(hosts) > 0 {
		if len(hosts) > 3 {
			parts += fmt.Sprintf(" → %s +%d more", strings.Join(hosts[:3], ", "), len(hosts)-3)
		} else {
			parts += " → " + strings.Join(hosts, ", ")
		}
	}
	return parts
}

// InputPreview returns first N visible chars of the input DSL for display.
func (e *CallEntry) InputPreview(maxLen int) string {
	s := strings.ReplaceAll(e.Input, "\n", " ↵ ")
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen-1]) + "…"
	}
	return s
}

// BriefFromSteps converts parsed Steps into compact StepBrief slice.
func BriefFromSteps(steps []Step) []StepBrief {
	out := make([]StepBrief, len(steps))
	for i, s := range steps {
		out[i] = StepBrief{
			Name:        s.Name,
			Action:      s.Action.String(),
			Hosts:       s.Hosts,
			BodyPreview: FirstScriptLine(s.Body),
			Params:      extractParams(&s.Config),
		}
	}
	return out
}

// extractParams pulls display-worthy config fields into a flat map.
func extractParams(c *StepConfig) map[string]string {
	m := make(map[string]string)
	if c.Timeout > 0 {
		m["timeout"] = fmt.Sprintf("%ds", c.Timeout)
	}
	if c.Trace != nil && *c.Trace {
		m["trace"] = "on"
	}
	if c.ContinueOnError {
		m["continue_on_error"] = "true"
	}
	if c.Entrypoint != "" {
		m["entrypoint"] = c.Entrypoint
	}
	if c.Filter != "" {
		m["filter"] = c.Filter
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// FirstScriptLine returns the first meaningful line from a script body,
// truncated for display. Skips blank lines, comments, and JSON config lines.
func FirstScriptLine(body string) string {
	const maxLen = 200
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// skip JSON config line ({"ssh": ...})
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			continue
		}
		if len(t) > maxLen {
			return t[:maxLen-1] + "…"
		}
		return t
	}
	return ""
}
