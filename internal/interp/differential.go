package interp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// runTimeout bounds a single corpus execution. A screener hole (or an
// unimplemented construct such as `select`) that hangs the interpreter must
// fail the build loudly, never block it.
const runTimeout = 8 * time.Second

// This file is the corpus oracle (plan decision #16a). Every corpus script is
// executed through BOTH real bash (subprocess) and the in-process mvdan/sh
// interpreter, in a fresh temp cwd each, and the two runs are diffed across six
// axes: stdout, stderr, exit code, final cwd, exported-env delta, files-touched.
// A recovered interp panic (go-task #2429) counts as divergence.
//
// Decision rule (differential_test.go): a script the screener routes to
// realbash is *expected* to diverge — we only assert the screener flagged it. A
// script the screener passes to interp MUST match bash on every axis; any
// divergence there is a hole in the screener and fails the build.
//
// Residual, non-screenable classes (plan §8, decision #16):
//
//	C — `set -e` + command-substitution truncation/abort: interp does not abort
//	    the script the way bash does. Routed to interp; diverges; DOCUMENTED.
//	D — concurrent `$$` tempfile collision: a LITERAL `$$` is screened to bash by
//	    the class-B rule, but the deeper residual is two runners sharing one host
//	    pid, or a tempfile name built dynamically (evading the static scan). That
//	    concurrency edge no static screen can catch; DOCUMENTED.
//	G — `IFS=` empty-field drop outside a for-loop (e.g. `set -- $x`): the loop
//	    screen does not fire; interp's field splitting diverges. DOCUMENTED.
//
// The harness MEASURES C/D/G (they are marked residual in the corpus manifest)
// but does not fail the build on them; U10 documents them for users.

// RunResult captures the observable outcome of one execution across all axes.
type RunResult struct {
	Stdout   string
	Stderr   string
	Exit     int
	Cwd      string // final cwd relative to the sandbox ("." if unchanged)
	CwdKnown bool
	Env      map[string]string // exported-env delta vs the base env
	EnvKnown bool
	Files    map[string]string // sandbox-relative path -> content hash
	Panicked bool              // interp only: recovered panic
}

// baseEnv is the closed base environment handed to BOTH engines so their env
// deltas and locale-sensitive output are comparable. HOME points at the sandbox
// so a bare `cd` is deterministic; LC_ALL/LANG pin C locale.
func baseEnv(sandbox string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + sandbox,
		"LC_ALL=C",
		"LANG=C",
	}
}

func baseEnvMap(sandbox string) map[string]string {
	m := map[string]string{}
	for _, kv := range baseEnv(sandbox) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// denyEnvKey filters volatile/shell-internal exported vars that legitimately
// differ between bash and interp and are not part of a script's meaningful
// env delta.
func denyEnvKey(name string) bool {
	switch name {
	case "PWD", "OLDPWD", "SHLVL", "_", "IFS", "LINENO", "RANDOM",
		"SECONDS", "PPID", "BASHPID", "HOME", "PATH", "LANG", "LC_ALL":
		return true
	}
	return strings.HasPrefix(name, "BASH")
}

// RunInterp runs script through the in-process interpreter in sandbox. A panic
// (go-task #2429) is recovered and reported as Panicked. A watchdog abandons a
// run that exceeds runTimeout even if interp ignores context cancellation (an
// unimplemented construct such as `select` can spin without checking ctx) — the
// abandoned goroutine leaks harmlessly in the test binary, and the divergence is
// reported rather than hanging the build.
func RunInterp(script []byte, sandbox string) RunResult {
	done := make(chan RunResult, 1)
	go func() { done <- runInterpInner(script, sandbox) }()
	select {
	case r := <-done:
		return r
	case <-time.After(runTimeout + 2*time.Second):
		return RunResult{
			Panicked: true,
			Stderr:   fmt.Sprintf("interp run abandoned after %s (did not honor cancellation)\n", runTimeout),
			Exit:     2,
			Env:      map[string]string{},
			Files:    snapshotFiles(sandbox),
		}
	}
}

func runInterpInner(script []byte, sandbox string) RunResult {
	res := RunResult{Env: map[string]string{}, Files: map[string]string{}}

	file, err := NewParser().Parse(bytes.NewReader(script), "")
	if err != nil {
		res.Stderr = err.Error() + "\n"
		res.Exit = 2
		res.Files = snapshotFiles(sandbox)
		return res
	}

	var stdout, stderr bytes.Buffer
	runner, nerr := interp.New(
		interp.Dir(sandbox),
		interp.Env(expand.ListEnviron(baseEnv(sandbox)...)),
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
	)
	if nerr != nil {
		res.Stderr = "interp.New: " + nerr.Error() + "\n"
		res.Exit = 2
		res.Files = snapshotFiles(sandbox)
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	func() {
		defer func() {
			if r := recover(); r != nil {
				res.Panicked = true
				fmt.Fprintf(&stderr, "interp panic: %v\n", r)
				res.Exit = 2
			}
		}()
		res.Exit = exitCodeFromErr(runner.Run(ctx, file))
	}()
	if ctx.Err() != nil {
		res.Panicked = true // treat a hung/timed-out interp run as divergence
		fmt.Fprintf(&stderr, "interp run timed out after %s\n", runTimeout)
	}

	res.Stdout = stdout.String()
	res.Stderr = stderr.String()

	if rel, ok := relCwd(sandbox, runner.Dir); ok {
		res.Cwd = rel
		res.CwdKnown = true
	}

	base := baseEnvMap(sandbox)
	for name, v := range runner.Vars {
		if !v.Exported || !v.Set || denyEnvKey(name) {
			continue
		}
		val := v.Str
		if bv, ok := base[name]; ok && bv == val {
			continue
		}
		res.Env[name] = val
	}
	res.EnvKnown = true
	res.Files = snapshotFiles(sandbox)
	return res
}

// RunBash runs script through real bash in sandbox. A wrapper sources the script
// in the same shell so the final cwd and exported env can be probed to a side
// file; if the script exits/execs before the probe runs, those axes are marked
// unknown (and simply not compared).
func RunBash(script []byte, sandbox string) RunResult {
	res := RunResult{Env: map[string]string{}, Files: map[string]string{}}

	root := filepath.Dir(sandbox)
	scriptFile := filepath.Join(root, "script.sh")
	probeFile := filepath.Join(root, "probe.txt")
	_ = os.WriteFile(scriptFile, script, 0o600)
	_ = os.Remove(probeFile)

	// Capture the script/probe paths into named vars BEFORE sourcing: a script
	// that runs `set --`/`shift` would otherwise clobber the wrapper's own $1/$2.
	const wrapper = `__sk_script="$1"; __sk_probe="$2"; source "$__sk_script"; __st=$?; { printf '__CWD__%s\n' "$PWD"; export -p; } >"$__sk_probe" 2>/dev/null; exit $__st`
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", wrapper, "bash", scriptFile, probeFile)
	cmd.Dir = sandbox
	cmd.Env = baseEnv(sandbox)
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Exit = ee.ExitCode()
		} else {
			res.Exit = -1
			res.Stderr = err.Error() + "\n"
		}
	}
	res.Stdout = stdout.String()
	res.Stderr += stderr.String()

	if data, err := os.ReadFile(probeFile); err == nil && len(data) > 0 {
		parseProbe(string(data), sandbox, &res)
	}
	res.Files = snapshotFiles(sandbox)
	return res
}

// parseProbe reads the __CWD__ + `declare -x` block bash wrote to the side file.
func parseProbe(probe, sandbox string, res *RunResult) {
	base := baseEnvMap(sandbox)
	for _, line := range strings.Split(probe, "\n") {
		switch {
		case strings.HasPrefix(line, "__CWD__"):
			if rel, ok := relCwd(sandbox, strings.TrimPrefix(line, "__CWD__")); ok {
				res.Cwd = rel
				res.CwdKnown = true
			}
		case strings.HasPrefix(line, "declare -") || strings.HasPrefix(line, "export "):
			name, val, ok := parseDeclareX(line)
			if !ok || denyEnvKey(name) {
				continue
			}
			if bv, has := base[name]; has && bv == val {
				continue
			}
			res.Env[name] = val
			res.EnvKnown = true
		}
	}
	// A probe with a __CWD__ line but no exported deltas still means env was
	// captured (an empty delta is a valid observation).
	if strings.Contains(probe, "__CWD__") {
		res.EnvKnown = true
	}
}

// parseDeclareX extracts NAME and value from a `declare -x NAME="value"` line
// (also handles `declare -rx`, and a bare `declare -x NAME` with no value).
func parseDeclareX(line string) (name, val string, ok bool) {
	rest := line
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[i+1:] // drop "declare"
	}
	rest = strings.TrimSpace(rest)
	// drop the flag token (e.g. "-x", "-rx")
	if strings.HasPrefix(rest, "-") {
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			rest = rest[i+1:]
		} else {
			return "", "", false
		}
	}
	name, raw, hasEq := strings.Cut(rest, "=")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	if !hasEq {
		return name, "", true // exported but unset value
	}
	return name, unquoteShell(raw), true
}

// unquoteShell best-effort strips the quoting bash's `export -p` applies. Corpus
// env values are simple, so a light unquote keeps the env axis fair.
func unquoteShell(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\$`, `$`)
		inner = strings.ReplaceAll(inner, "\\`", "`")
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	if strings.HasPrefix(s, "$'") && strings.HasSuffix(s, "'") {
		return s[2 : len(s)-1]
	}
	return s
}

// snapshotFiles lists every regular file under dir as a sandbox-relative path
// mapped to a short content hash, so "files-touched" can be diffed.
func snapshotFiles(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		data, derr := os.ReadFile(path)
		if derr != nil {
			out[rel] = "unreadable"
			return nil
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}

// relCwd returns dir relative to sandbox with symlinks resolved on both sides
// (macOS /var/folders vs /private noise). ok is false if dir escapes sandbox in
// an unrepresentable way.
func relCwd(sandbox, dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	sb := sandbox
	if r, err := filepath.EvalSymlinks(sandbox); err == nil {
		sb = r
	}
	d := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		d = r
	}
	rel, err := filepath.Rel(sb, d)
	if err != nil {
		return "", false
	}
	return rel, true
}

// execLookBash reports whether a real bash is available on PATH (the corpus
// oracle needs it as the reference implementation).
func execLookBash() (string, error) {
	return exec.LookPath("bash")
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var st interp.ExitStatus
	if errors.As(err, &st) {
		return int(st)
	}
	return 1
}

// DiffAxes returns the names of every axis on which the two runs diverge.
func DiffAxes(b, i RunResult) []string {
	var ax []string
	if b.Stdout != i.Stdout {
		ax = append(ax, "stdout")
	}
	if b.Stderr != i.Stderr {
		ax = append(ax, "stderr")
	}
	if b.Exit != i.Exit {
		ax = append(ax, "exit")
	}
	if b.CwdKnown && i.CwdKnown && b.Cwd != i.Cwd {
		ax = append(ax, "cwd")
	}
	if b.EnvKnown && i.EnvKnown && !envEqual(b.Env, i.Env) {
		ax = append(ax, "env")
	}
	if !filesEqual(b.Files, i.Files) {
		ax = append(ax, "files")
	}
	if i.Panicked {
		ax = append(ax, "panic")
	}
	return ax
}

func envEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func filesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// axisDetail renders a compact, human diagnostic for a failing axis so the test
// output names the script and the diverging content.
func axisDetail(axis string, b, i RunResult) string {
	switch axis {
	case "stdout":
		return fmt.Sprintf("stdout bash=%q interp=%q", b.Stdout, i.Stdout)
	case "stderr":
		return fmt.Sprintf("stderr bash=%q interp=%q", b.Stderr, i.Stderr)
	case "exit":
		return fmt.Sprintf("exit bash=%d interp=%d", b.Exit, i.Exit)
	case "cwd":
		return fmt.Sprintf("cwd bash=%q interp=%q", b.Cwd, i.Cwd)
	case "env":
		return fmt.Sprintf("env bash=%v interp=%v", sortedEnv(b.Env), sortedEnv(i.Env))
	case "files":
		return fmt.Sprintf("files bash=%v interp=%v", sortedKeys(b.Files), sortedKeys(i.Files))
	case "panic":
		return "interp panicked (recovered)"
	}
	return axis
}

func sortedEnv(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
