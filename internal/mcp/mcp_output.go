package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/caoer/shellkit/internal/inventory"
)

type TraceLine struct {
	ElapsedSec int
	LineNo     int // source line number from $LINENO; 0 if unavailable
	Command    string

	// Runner-path (mvdan/sh) trace fields (U0 §1.1). These are set only when the
	// step ran under the runner (StepResult.RunnerPath == true); the legacy path
	// leaves them zero and renders from ElapsedSec instead, keeping today's output
	// byte-for-byte identical.
	ElapsedNS  int64 // ns since step start, measured at cmd_start
	DurationNS int64 // ns this command took, from cmd_end; 0 = unknown
	Exit       *int  // command's own exit; nil = unknown/legacy. Rendered inline only when non-zero.
}

type StepResult struct {
	Name       string
	Host       string
	ExitCode   int
	Stdout     string
	Stderr     string
	Outputs    map[string]string
	FilePath   string
	Error      string
	Trace      []TraceLine
	TimedOut   bool
	TimeoutSec int
	ShowTrace  bool // user requested trace: true → include in final output

	// RouteNote is the per-host route provenance line (U0 §2). Empty on both the
	// runner-success path and the pure legacy path (byte-identical to today);
	// non-empty only when the runner was opted into but the step ran under the
	// legacy path anyway (gap auto-route, bootstrap fallback, proto mismatch).
	RouteNote string
	// RunnerPath is true when this result came from the mvdan/sh runner. It
	// selects the ns trace renderer in formatResults; false renders the legacy
	// whole-second trace verbatim.
	RunnerPath bool
}

type OutputStore struct {
	dir     string
	results map[string]*StepResult
	servers []inventory.Server
}

func NewOutputStore(servers []inventory.Server) (*OutputStore, error) {
	dir, err := os.MkdirTemp("", "shellkit-mcp-*")
	if err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	return &OutputStore{
		dir:     dir,
		results: make(map[string]*StepResult),
		servers: servers,
	}, nil
}

// Cleanup removes the temp directory and all output files.
func (o *OutputStore) Cleanup() {
	if o.dir != "" {
		os.RemoveAll(o.dir)
	}
}

func (o *OutputStore) Store(r *StepResult) {
	key := r.Name
	if r.Host != "" {
		key = r.Name + "." + r.Host
	}
	o.results[key] = r
	o.results[r.Name] = r
}

func (o *OutputStore) StepFilePath(stepName string) string {
	return filepath.Join(o.dir, stepName+".out")
}

func (o *OutputStore) StepFilePathForHost(stepName, host string) string {
	return filepath.Join(o.dir, stepName+"."+host+".out")
}

func (o *OutputStore) findServer(name string) *inventory.Server {
	for i := range o.servers {
		s := &o.servers[i]
		if s.Name == name || s.SSHAlias == name ||
			fmt.Sprintf("%s.%s", s.Provider, s.Name) == name {
			return s
		}
	}
	return nil
}

var templateRe = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// looksLikeRef reports whether expr is a shellkit output/host reference
// (step.field, host.property, step.host.outputs.key) rather than a Go template
// action. shellkit references are dotted bare identifiers: no whitespace, no
// leading '.' or '$', and at least one '.'. Every Go template form fails one of
// these and is passed through untouched — {{.State.Status}} (leading dot),
// {{json .Ports}} / {{range .X}} / {{index .Ports "5432/tcp"}} (whitespace),
// {{end}} (no dot). A dotted typo like {{stpe.output}} still looks like a ref,
// so it reaches resolveExpr and errors loudly instead of passing silently.
func looksLikeRef(expr string) bool {
	if expr == "" || expr[0] == '.' || expr[0] == '$' {
		return false
	}
	if strings.ContainsAny(expr, " \t\r\n|()'\"`") {
		return false
	}
	return strings.Contains(expr, ".")
}

func (o *OutputStore) Resolve(body string) (string, error) {
	var resolveErr error
	resolved := templateRe.ReplaceAllStringFunc(body, func(match string) string {
		expr := strings.TrimSpace(match[2 : len(match)-2])
		if !looksLikeRef(expr) {
			return match
		}
		val, err := o.resolveExpr(expr)
		if err != nil {
			resolveErr = err
			return match
		}
		return val
	})
	return resolved, resolveErr
}

func (o *OutputStore) resolveHostProperty(srv *inventory.Server, field string) (string, error) {
	switch field {
	case "wan_ip":
		return srv.IP, nil
	case "user":
		return srv.DisplayUser(), nil
	case "port":
		port := srv.Port
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("%d", port), nil
	case "tailscale_ip":
		return srv.TailscaleIP, nil
	case "lan_ip":
		return srv.PrivateIP, nil
	case "wireguard_ip":
		return srv.WGIP, nil
	case "easytier_ip":
		return srv.EasytierIP, nil
	case "prefer_net":
		return srv.PreferNet, nil
	default:
		return "", fmt.Errorf("unknown host property: %s", field)
	}
}

func (o *OutputStore) resolveExpr(expr string) (string, error) {
	parts := strings.SplitN(expr, ".", 2)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid template expression: {{%s}}", expr)
	}

	// Try progressively longer dotted prefixes as hostname.
	// Handles dotted names like "servarica.yul-2-oc-bot-6.ip"
	// where the hostname is "servarica.yul-2-oc-bot-6".
	allParts := strings.Split(expr, ".")
	for i := len(allParts) - 1; i >= 1; i-- {
		candidate := strings.Join(allParts[:i], ".")
		field := strings.Join(allParts[i:], ".")
		if srv := o.findServer(candidate); srv != nil {
			return o.resolveHostProperty(srv, field)
		}
	}

	name := parts[0]
	field := parts[1]

	// step output lookup
	r, ok := o.results[name]
	if !ok {
		return "", fmt.Errorf("step %q not found (not yet executed?)", name)
	}

	switch {
	case field == "output":
		return r.FilePath, nil
	case strings.HasPrefix(field, "outputs."):
		key := field[len("outputs."):]
		val, ok := r.Outputs[key]
		if !ok {
			return "", fmt.Errorf("step %q has no output key %q", name, key)
		}
		return val, nil
	default:
		// try host-qualified: {{step.hostname.outputs.key}}
		subParts := strings.SplitN(field, ".", 2)
		if len(subParts) == 2 {
			hostKey := name + "." + subParts[0]
			if hr, ok := o.results[hostKey]; ok {
				subField := subParts[1]
				if strings.HasPrefix(subField, "outputs.") {
					okey := subField[len("outputs."):]
					val, ok := hr.Outputs[okey]
					if !ok {
						return "", fmt.Errorf("step %q host %q has no output key %q", name, subParts[0], okey)
					}
					return val, nil
				}
				if subField == "output" {
					return hr.FilePath, nil
				}
			}
		}
		return "", fmt.Errorf("unknown field %q on step %q", field, name)
	}
}

func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func ParseOutputs(raw string) map[string]string {
	outputs := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			outputs[key] = val
		}
	}
	return outputs
}
