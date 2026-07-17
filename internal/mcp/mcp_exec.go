package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/interp"
	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/rundaemon"
	"github.com/caoer/shellkit/internal/sshconn"
)

const (
	defaultTimeout = 360
)

var allowedEntrypoints = map[string]bool{
	"bash": true, "sh": true, "zsh": true,
	"python3": true, "python": true,
	"node": true, "deno": true, "bun": true,
	"ruby": true, "perl": true,
}

func validateEntrypoint(ep string) error {
	if allowedEntrypoints[ep] {
		return nil
	}
	return fmt.Errorf("unsupported entrypoint %q (allowed: bash, sh, zsh, python3, python, node, deno, bun, ruby, perl)", ep)
}

func execNonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func outputMarkerFor(nonce string) string {
	return "---SHELLKIT-OUTPUTS-" + nonce + "---"
}

func stderrMarkerFor(nonce string) string {
	return "---SHELLKIT-STDERR-" + nonce + "---"
}

func heredocDelimiter(nonce string) string {
	return "SHELLKIT_SCRIPT_" + nonce
}

func traceMarkerFor(nonce string) string {
	return "SHELLKIT_CMD_" + nonce
}

// shouldTrace returns whether the DEBUG-trap trace prelude is prepended to
// the body. Trace is now ALWAYS on for bash so the live dashboard can show the
// currently-executing command. The DSL's `trace` config field is retained but
// is purely a display knob handled by formatResults — it does not control
// collection.
func shouldTrace(config StepConfig, entrypoint string) bool {
	return entrypoint == "bash"
}

// eventStreamKey is the context key for the per-call EventStream.
type eventStreamKey struct{}

func eventStreamFromContext(ctx context.Context) *EventStream {
	if v, ok := ctx.Value(eventStreamKey{}).(*EventStream); ok {
		return v
	}
	return nil
}

// ContextWithEventStream returns a context carrying the per-call EventStream,
// which the Executor reads to emit live progress events.
func ContextWithEventStream(ctx context.Context, s *EventStream) context.Context {
	return context.WithValue(ctx, eventStreamKey{}, s)
}

// teeWith returns an io.Writer that writes to dst (the in-process buffer used
// for final result parsing) and, when an EventStream is in ctx, simultaneously
// emits live events for each line.
//
// stream="stdout" enables wrapper-protocol awareness when nonce is non-empty:
// trace markers become "executing" events, and the outputMarker line ends
// emission. stream="stderr" emits each line verbatim with no marker handling.
func teeWith(dst io.Writer, ctx context.Context, stepIdx int, stepName, host, nonce, stream string) io.Writer {
	es := eventStreamFromContext(ctx)
	if es == nil {
		return dst
	}
	var live io.Writer
	if stream == "stdout" {
		live = es.StdoutWriter(stepIdx, stepName, host, nonce)
	} else {
		live = es.StderrWriter(stepIdx, stepName, host)
	}
	return io.MultiWriter(dst, live)
}

// prependTrace inserts a DEBUG trap that emits one marker line per simple
// command bash is about to execute. Format:
//
//	<marker> <SECONDS> <LINENO> <BASH_COMMAND>
//
// LINENO lets the dashboard group multiple simple commands (pipes, &&, ||, ;)
// back to the source line they originated from, since one written line can
// fire the DEBUG trap multiple times.
func prependTrace(body, nonce string) string {
	marker := traceMarkerFor(nonce)
	return fmt.Sprintf("trap 'echo \"%s $SECONDS $LINENO $BASH_COMMAND\"' DEBUG\n", marker) + body
}

// parseTraceLine parses one trap-emitted line (with marker prefix already
// stripped). Returns elapsed seconds, source line number, and the command
// text. LineNo is 0 if the line predates the LINENO addition.
func parseTraceLine(rest string) (elapsed, lineNo int, cmd string, ok bool) {
	parts := strings.SplitN(rest, " ", 3)
	if len(parts) < 2 {
		return
	}
	elapsed, _ = strconv.Atoi(parts[0])
	if len(parts) == 2 {
		// Old format: <SECONDS> <CMD...>
		cmd = parts[1]
		ok = true
		return
	}
	// New format: <SECONDS> <LINENO> <CMD...>
	if n, err := strconv.Atoi(parts[1]); err == nil {
		lineNo = n
		cmd = parts[2]
		ok = true
		return
	}
	// parts[1] not numeric — fall back to old format, glue back together.
	cmd = parts[1] + " " + parts[2]
	ok = true
	return
}

func extractTrace(stdout, nonce string) (cleanStdout string, trace []TraceLine) {
	marker := traceMarkerFor(nonce) + " "
	bareMarker := traceMarkerFor(nonce)
	var clean []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, marker) {
			if elapsed, lineNo, cmd, ok := parseTraceLine(line[len(marker):]); ok {
				trace = append(trace, TraceLine{ElapsedSec: elapsed, LineNo: lineNo, Command: cmd})
			}
			continue
		}
		// Mid-line marker: preceding command used echo -n (no trailing
		// newline), so the trap output got appended to the same line.
		if idx := strings.Index(line, marker); idx > 0 {
			if elapsed, lineNo, cmd, ok := parseTraceLine(line[idx+len(marker):]); ok {
				trace = append(trace, TraceLine{ElapsedSec: elapsed, LineNo: lineNo, Command: cmd})
			}
			if prefix := line[:idx]; prefix != "" {
				clean = append(clean, prefix)
			}
			continue
		}
		// Bare marker without variables (e.g. empty $SECONDS expansion).
		if idx := strings.Index(line, bareMarker); idx >= 0 {
			if prefix := line[:idx]; prefix != "" {
				clean = append(clean, prefix)
			}
			continue
		}
		clean = append(clean, line)
	}
	cleanStdout = strings.Join(clean, "\n")
	return
}

// runnerDefaultOn is the U9 differential-gate flip point. FLIPPED 2026-07-17
// (ZT, decisions/runner-default-on-flip.md) after the bash-differential corpus
// went green: statically-screened bash scripts and non-bash entrypoints ride
// the runner by default; gap constructs auto-route to legacy real bash, and
// `"interp": false` still forces legacy. Reverting to opt-in is this ONE line.
const runnerDefaultOn = true

// runnerOptIn reports the step's runner posture from its DSL config, honoring
// runnerDefaultOn. It also gates whether interp.Preflight runs at all: a step
// that stays on the legacy path must behave byte-for-byte as today, including
// running syntactically-invalid bash remotely rather than being refused locally.
func runnerOptIn(cfg StepConfig) bool {
	if cfg.Interp != nil {
		return *cfg.Interp // explicit: true engages the runner, false forces legacy
	}
	return runnerDefaultOn // absent → the default posture
}

type Executor struct {
	store        *OutputStore
	servers      []inventory.Server
	bootstrapper *rundaemon.Bootstrapper
}

func NewExecutor(store *OutputStore, servers []inventory.Server) *Executor {
	return &Executor{
		store:        store,
		servers:      servers,
		bootstrapper: rundaemon.NewBootstrapper(),
	}
}

func (e *Executor) Execute(ctx context.Context, steps []Step) ([]StepResult, error) {
	var results []StepResult
	stream := eventStreamFromContext(ctx)

	for i := range steps {
		step := &steps[i]
		stepStart := time.Now()
		emitStepStart(stream, i, step)

		var stepResults []StepResult
		var err error

		switch step.Action {
		case ActionHelp:
			stepResults = []StepResult{{Name: step.Name, Stdout: helpText, ExitCode: 0}}
		case ActionList:
			stepResults = []StepResult{e.executeList(step)}
		case ActionSSH:
			stepResults, err = e.executeSSH(ctx, i, step)
		case ActionLocal:
			stepResults, err = e.executeLocal(ctx, i, step)
		case ActionTmux:
			stepResults, err = e.executeTmux(ctx, i, step)
		}

		if err != nil {
			res := StepResult{Name: step.Name, ExitCode: 1, Error: err.Error()}
			results = append(results, res)
			emitStepEnd(stream, i, step, []StepResult{res}, stepStart)
			if !step.Config.ContinueOnError {
				return results, nil
			}
			continue
		}

		for j := range stepResults {
			e.store.Store(&stepResults[j])
		}
		results = append(results, stepResults...)
		emitStepEnd(stream, i, step, stepResults, stepStart)

		if !step.Config.ContinueOnError {
			for _, r := range stepResults {
				if r.ExitCode != 0 {
					return results, nil
				}
			}
		}
	}
	return results, nil
}

func emitStepStart(stream *EventStream, idx int, step *Step) {
	if stream == nil {
		return
	}
	stream.Emit("step-start", map[string]any{
		"step":   idx,
		"name":   step.Name,
		"action": step.Action.String(),
		"hosts":  step.Hosts,
	})
}

func emitStepEnd(stream *EventStream, idx int, step *Step, results []StepResult, start time.Time) {
	if stream == nil {
		return
	}
	exit := 0
	for _, r := range results {
		if r.ExitCode != 0 || r.TimedOut || r.Error != "" {
			exit = r.ExitCode
			if exit == 0 {
				exit = 1
			}
			break
		}
	}
	stream.Emit("step-end", map[string]any{
		"step":        idx,
		"name":        step.Name,
		"exit_code":   exit,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (e *Executor) executeSSH(ctx context.Context, stepIdx int, step *Step) ([]StepResult, error) {
	hosts := step.Hosts
	if len(hosts) == 0 {
		return nil, fmt.Errorf("step %q: ssh requires at least one host", step.Name)
	}

	entrypoint := step.Config.Entrypoint
	if entrypoint == "" {
		entrypoint = "bash"
	}
	if err := validateEntrypoint(entrypoint); err != nil {
		return nil, fmt.Errorf("step %q: %w", step.Name, err)
	}

	var results []StepResult
	fanout := len(hosts) > 1
	for _, host := range hosts {
		srv := e.resolveSSHHost(host)
		if srv == nil {
			return nil, fmt.Errorf("step %q: unknown host %q", step.Name, host)
		}

		// DSL "jump" overrides the server's ProxyJump, letting agents hop
		// through any host without an inventory or ssh_config entry.
		if step.Config.Jump != "" {
			srv.ProxyJump = step.Config.Jump
		}
		// DSL "identity" overrides the server's identity file, letting
		// agents specify a key without an inventory or ssh_config entry.
		if step.Config.Identity != "" {
			srv.Identity = step.Config.Identity
			srv.IdentitiesOnly = true
		}

		body, err := e.store.Resolve(step.Body)
		if err != nil {
			return nil, fmt.Errorf("step %q: template: %w", step.Name, err)
		}

		timeout := step.Config.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}

		// U6b per-host routing: the runner path is selected iff the step opted in
		// (runnerOptIn), the preflight verdict is interp (bash bodies only —
		// non-bash entrypoints skip preflight and ride the runner as supervised
		// subprocesses with whole-step timing, hard constraint #4), and bootstrap
		// yields a usable runner. Any other outcome runs the legacy path —
		// verbatim today's behavior — carrying a RouteNote only when the runner
		// was opted into and fell back (U0 §2/§5).
		var routeNote string
		if runnerOptIn(step.Config) {
			runnerRoute := true
			if entrypoint == "bash" {
				verdict, perr := interp.Preflight([]byte(body))
				if perr != nil {
					// Unrecoverable syntax error: refuse BEFORE connecting, surfacing
					// the positioned teaching error (interp.PreflightError) instead of a
					// remote shell error. Aborts the whole step — an invalid body must
					// not run on any host.
					return nil, fmt.Errorf("step %q: %w", step.Name, perr)
				}
				if verdict.Route != interp.RouteInterp {
					// Static gap auto-route (U0 §5.1): interp can't faithfully run this
					// construct — legacy path, with the router's reason as the note.
					routeNote = verdict.Reason
					runnerRoute = false
				}
			}
			if runnerRoute {
				// Bootstrap counts against the step timeout: a hung smoke-test /
				// push / version-probe on a cold host must not block past the
				// step's configured budget. Bound it with a step-scoped context;
				// a timed-out bootstrap returns Fallback and we drop to legacy —
				// bootstrap itself never step-fails.
				bootCtx, cancelBoot := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
				boot := e.bootstrapper.Bootstrap(bootCtx, srv)
				cancelBoot()
				if boot.Fallback {
					// Bootstrap fallback (U0 §5.3): no usable runner on this host.
					routeNote = boot.Reason
				} else if result, ranBody := e.runRunnerSSH(ctx, srv, body, boot.RunnerPath, stepIdx, step, host, entrypoint, timeout); ranBody {
					// Runner path is authoritative — including a wire-cut/protocol
					// exit -1, which is NOT retried under legacy (the body may have
					// executed, and the script may not be idempotent).
					result.ShowTrace = traceRequested(step.Config)
					e.writeStepOutput(&result, step.Name, host, fanout)
					results = append(results, result)
					if !step.Config.ContinueOnError && result.ExitCode != 0 {
						break
					}
					continue
				} else {
					// Body provably never executed (spawn failure / proto mismatch
					// at handshake) — safe to fall back to legacy with the note.
					routeNote = result.RouteNote
				}
			}
		}

		// Legacy path — verbatim today's behavior. routeNote is "" unless the
		// runner was opted into above and fell back, so a step that never engaged
		// the runner renders byte-for-byte identically to today.
		nonce := execNonce()
		traced := shouldTrace(step.Config, entrypoint)
		if traced {
			body = prependTrace(body, nonce)
		}
		wrapped := wrapScript(body, entrypoint, nonce)

		tctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		result := e.runSSH(tctx, srv, wrapped, nonce, stepIdx, step.Name, host, fanout, traced, entrypoint, timeout)
		cancel()
		result.ShowTrace = traceRequested(step.Config)
		result.RouteNote = routeNote

		e.writeStepOutput(&result, step.Name, host, fanout)
		results = append(results, result)

		if !step.Config.ContinueOnError && result.ExitCode != 0 {
			break
		}
	}

	if len(hosts) > 1 {
		merged := e.mergeFanoutOutputs(step.Name, results)
		// append merged LAST so Execute's Store loop stores per-host first,
		// then merged wins the bare-name slot
		results = append(results, merged)
	}

	return results, nil
}

func (e *Executor) runSSH(ctx context.Context, srv *inventory.Server, script, nonce string, stepIdx int, stepName, host string, fanout bool, traced bool, entrypoint string, timeoutSec int) StepResult {
	remoteShell := wrapperShell(entrypoint)

	// resolveInvocation may call keyAuthWorks which already acquires a
	// rate-limit slot for its SSH probe. Resolve first, then acquire a slot
	// for the actual execution — avoids double-counting for password hosts.
	name, args, env, err := sshconn.ResolveInvocation(ctx, srv, remoteShell, "-s")
	if err != nil {
		return StepResult{
			Name:     stepName,
			Host:     host,
			ExitCode: 1,
			Error:    fmt.Sprintf("resolve password: %v", err),
		}
	}

	// Per-host rate limiting: prevents connection storms that trigger provider
	// abuse detection (e.g., BandwagonHost "too many SSH attempts in 180s").
	if err := sshconn.SSHRateLimit.Acquire(ctx, sshconn.SSHRateLimitKey(srv)); err != nil {
		return StepResult{
			Name:     stepName,
			Host:     host,
			ExitCode: 1,
			Error:    fmt.Sprintf("ssh rate limit: %v", err),
		}
	}

	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = teeWith(&stdout, ctx, stepIdx, stepName, host, nonce, "stdout")
	cmd.Stderr = teeWith(&stderr, ctx, stepIdx, stepName, host, "", "stderr")

	err = cmd.Run()
	timedOut := ctx.Err() == context.DeadlineExceeded
	exitCode := 0
	if err != nil {
		if timedOut {
			exitCode = 137
		} else if e, ok := err.(*exec.ExitError); ok {
			exitCode = e.ExitCode()
		} else {
			return StepResult{
				Name:     stepName,
				Host:     host,
				ExitCode: 1,
				Error:    err.Error(),
				Stderr:   stderr.String(),
			}
		}
	}

	rawOut := stdout.String()

	var trace []TraceLine
	if traced {
		rawOut, trace = extractTrace(rawOut, nonce)
	}

	scriptStdout, outputs, scriptStderr := parseWrappedOutputNonce(rawOut, nonce)

	result := StepResult{
		Name:       stepName,
		Host:       host,
		ExitCode:   exitCode,
		Stdout:     scriptStdout,
		Stderr:     scriptStderr + stderr.String(),
		Outputs:    ParseOutputs(outputs),
		Trace:      trace,
		TimedOut:   timedOut,
		TimeoutSec: timeoutSec,
	}
	return result
}

// traceRequested reports whether the user explicitly set trace: true.
func traceRequested(cfg StepConfig) bool {
	return cfg.Trace != nil && *cfg.Trace
}

// writeStepOutput writes the step's stdout to its per-step (fan-out: per-host)
// output file and records FilePath, so the {{step.output}} / {{step.host.output}}
// machinery and the fan-out merge behave identically whichever route produced the
// result. Shared verbatim by the runner and legacy SSH paths.
func (e *Executor) writeStepOutput(result *StepResult, stepName, host string, fanout bool) {
	outPath := e.store.StepFilePath(stepName)
	if fanout {
		outPath = e.store.StepFilePathForHost(stepName, host)
	}
	if wErr := os.WriteFile(outPath, []byte(result.Stdout), 0644); wErr != nil {
		if result.Error != "" {
			result.Error += "; "
		}
		result.Error += fmt.Sprintf("output write failed: %v", wErr)
	} else {
		result.FilePath = outPath
	}
}

// runRunnerSSH drives one step on srv through the mvdan/sh runner: spawn the
// bootstrapped runner over the SAME ssh invocation the legacy path uses
// (rundaemon.SpawnSSH), drive it with the runner protocol (Client.RunStep), then
// map the rundaemon.StepOutcome onto an mcp.StepResult (exit / $OUTPUT / trace /
// stderr), preserving the timeout exit-137 and wire-cut exit -1 signatures.
//
// ranBody is false ONLY when the body provably never executed — a spawn failure,
// a handshake rejection (proto/role/version mismatch) before the run frame, or a
// run/file frame refused as oversize before being sent — so the caller may safely
// fall back to the legacy path; the returned result then carries just the
// RouteNote. Otherwise ranBody is true and the result is authoritative, INCLUDING
// a wire-cut/protocol-error exit -1 (the body may have run; re-running under
// legacy could double-apply a non-idempotent script).
func (e *Executor) runRunnerSSH(ctx context.Context, srv *inventory.Server, body, runnerPath string, stepIdx int, step *Step, host, entrypoint string, timeoutSec int) (StepResult, bool) {
	// Same per-host connection-storm guard the legacy path applies before its
	// exec (provider abuse detection); a saturated limiter falls back to legacy.
	if err := sshconn.SSHRateLimit.Acquire(ctx, sshconn.SSHRateLimitKey(srv)); err != nil {
		return StepResult{RouteNote: fmt.Sprintf("runner ssh rate limit on %s (%v) — ran under legacy path", srv.Name, err)}, false
	}

	stepTimeout := time.Duration(timeoutSec) * time.Second
	// The transport ctx outlives the step ctx so a step timeout cancels the step
	// in-band (TERM→KILL over the wire → clean exit 137 result frame) while the
	// ssh process stays alive long enough to read that result and reap cleanly,
	// rather than being SIGKILLed into a wire cut (exit -1).
	transportCtx, cancelTransport := context.WithTimeout(ctx, stepTimeout+15*time.Second)
	defer cancelTransport()

	proc, err := rundaemon.SpawnSSH(transportCtx, srv, runnerPath)
	if err != nil {
		return StepResult{RouteNote: fmt.Sprintf("runner spawn failed on %s (%v) — ran under legacy path", srv.Name, err)}, false
	}

	// Point the live 3s ticker at this Client's ProgressSummary for the step's
	// lifetime (U0 §3: phase:bootstrap during handshake, phase:run once live);
	// cleared on return so the legacy ticker payload resumes. SetProgressDelegate
	// tolerates a nil stream.
	stream := eventStreamFromContext(ctx)
	stream.SetProgressDelegate(proc.Client.ProgressSummary)
	defer stream.SetProgressDelegate(nil)

	stepCtx, cancelStep := context.WithTimeout(ctx, stepTimeout)
	defer cancelStep()

	// Client Step.Entrypoint semantics: empty runs the default bash interp path;
	// a non-empty value makes the runner exec it as a supervised subprocess.
	runnerEntrypoint := entrypoint
	if runnerEntrypoint == "bash" {
		runnerEntrypoint = ""
	}
	outcome, rerr := proc.Client.RunStep(stepCtx, rundaemon.Step{
		Program:    []byte(body),
		Timeout:    stepTimeout,
		Name:       step.Name,
		Host:       host,
		Entrypoint: runnerEntrypoint,
		// Pin the handshake to the bootstrapped runner: bootstrap verified this
		// binary's digest/version on-host, so a hello advertising any other
		// version is a stale/wrong endpoint — reject it and fall back to legacy.
		ExpectVersion: rundaemon.RunnerVersion,
	})
	_ = proc.Wait()

	if rerr != nil {
		// Client misuse (nil stdio) — the body never ran; fall back to legacy.
		return StepResult{RouteNote: fmt.Sprintf("runner drive error on %s (%v) — ran under legacy path", srv.Name, rerr)}, false
	}
	if outcome.ProtoMismatch {
		// Rejected at the handshake, before the run frame — the body never ran.
		return StepResult{RouteNote: outcome.Error + " — ran under legacy path for this step"}, false
	}
	if outcome.UnsentOversize {
		// The run/file frame exceeded the wire limit and was NEVER sent, so the
		// body provably did not execute — fall back to legacy (which streams the
		// body over ssh stdin with no per-line ceiling) rather than reporting a
		// false exit -1.
		return StepResult{RouteNote: outcome.Error + " — ran under legacy path for this step"}, false
	}

	result := StepResult{
		Name:       step.Name,
		Host:       host,
		ExitCode:   outcome.Exit,
		Stdout:     outcome.Stdout,
		Stderr:     outcome.Stderr,
		Outputs:    outcome.Outputs,
		Trace:      adaptRunnerTrace(outcome.Trace),
		TimedOut:   stepCtx.Err() == context.DeadlineExceeded,
		TimeoutSec: timeoutSec,
		RunnerPath: true,
		Error:      outcome.Error,
	}
	return result, true
}

// adaptRunnerTrace maps rundaemon's render-agnostic TraceLine (its own type, so
// rundaemon never imports mcp) onto mcp's TraceLine, carrying the ns-precision
// timing fields the runner-path renderer reads (U0 §1.1). ElapsedSec/LineNo stay
// zero — the runner path renders from ElapsedNS/DurationNS, not the legacy ints.
func adaptRunnerTrace(in []rundaemon.TraceLine) []TraceLine {
	if len(in) == 0 {
		return nil
	}
	out := make([]TraceLine, len(in))
	for i, t := range in {
		out[i] = TraceLine{
			Command:    t.Command,
			ElapsedNS:  t.ElapsedNS,
			DurationNS: t.DurationNS,
			Exit:       t.Exit,
		}
	}
	return out
}

func (e *Executor) executeLocal(ctx context.Context, stepIdx int, step *Step) ([]StepResult, error) {
	body, err := e.store.Resolve(step.Body)
	if err != nil {
		return nil, fmt.Errorf("step %q: template: %w", step.Name, err)
	}

	entrypoint := step.Config.Entrypoint
	if entrypoint == "" {
		entrypoint = "bash"
	}
	if err := validateEntrypoint(entrypoint); err != nil {
		return nil, fmt.Errorf("step %q: %w", step.Name, err)
	}

	nonce := execNonce()
	traced := shouldTrace(step.Config, entrypoint)
	if traced {
		body = prependTrace(body, nonce)
	}

	timeout := step.Config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	outputFile := e.store.StepFilePath(step.Name)
	outputsFile := outputFile + ".kv"

	scriptFile, err := os.CreateTemp("", "shellkit-local-*"+scriptExt(entrypoint))
	if err != nil {
		return nil, fmt.Errorf("step %q: create temp script: %w", step.Name, err)
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	if _, err := scriptFile.WriteString(body); err != nil {
		scriptFile.Close()
		return nil, fmt.Errorf("step %q: write temp script: %w", step.Name, err)
	}
	scriptFile.Close()

	tctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(tctx, entrypoint, scriptPath)
	if cwd := clientCwdFromContext(ctx); cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(), "OUTPUT="+outputsFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	var stdout, stderr bytes.Buffer
	// Local exec has no wrapper protocol — pass nonce only when traced so the
	// emitter recognises trace lines and converts them to "executing" events.
	emitNonce := ""
	if traced {
		emitNonce = nonce
	}
	cmd.Stdout = teeWith(&stdout, ctx, stepIdx, step.Name, "", emitNonce, "stdout")
	cmd.Stderr = teeWith(&stderr, ctx, stepIdx, step.Name, "", "", "stderr")

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			exitCode = e.ExitCode()
		} else if tctx.Err() != nil {
			exitCode = 137
		} else {
			return nil, fmt.Errorf("step %q: exec: %w", step.Name, err)
		}
	}
	timedOut := tctx.Err() == context.DeadlineExceeded
	if timedOut {
		exitCode = 137
	}

	rawStdout := stdout.String()
	var trace []TraceLine
	if traced {
		rawStdout, trace = extractTrace(rawStdout, nonce)
	}

	var outFilePath string
	var outWriteErr string
	if wErr := os.WriteFile(outputFile, []byte(rawStdout), 0644); wErr != nil {
		outWriteErr = fmt.Sprintf("output write failed: %v", wErr)
	} else {
		outFilePath = outputFile
	}

	outputs := make(map[string]string)
	if data, err := os.ReadFile(outputsFile); err == nil {
		outputs = ParseOutputs(string(data))
		os.Remove(outputsFile)
	}

	result := StepResult{
		Name:       step.Name,
		ExitCode:   exitCode,
		Stdout:     rawStdout,
		Stderr:     stderr.String(),
		Outputs:    outputs,
		FilePath:   outFilePath,
		Error:      outWriteErr,
		Trace:      trace,
		TimedOut:   timedOut,
		TimeoutSec: timeout,
		ShowTrace:  traceRequested(step.Config),
	}
	return []StepResult{result}, nil
}

func (e *Executor) executeList(step *Step) StepResult {
	filter := step.Config.Filter

	var filtered []inventory.Server
	for _, s := range e.servers {
		if filter != "" && !matchFilter(s, filter) {
			continue
		}
		filtered = append(filtered, s)
	}

	var b strings.Builder
	for _, s := range filtered {
		alias := s.SSHAlias
		if alias == "" {
			alias = s.Name
		}
		fmt.Fprintf(&b, "%-25s %-15s %-18s %-10s %s\n",
			alias, s.Provider, s.IP, s.Role, s.Location)
	}

	return StepResult{
		Name:     step.Name,
		ExitCode: 0,
		Stdout:   b.String(),
		Outputs:  map[string]string{"count": fmt.Sprintf("%d", len(filtered))},
	}
}

func (e *Executor) findServer(name string) *inventory.Server {
	return e.store.findServer(name)
}

// resolveSSHHost looks up a name in the inventory first. On a miss it falls
// back to two synthesizers so non-inventory targets still work:
//  1. parseRawSSHTarget for "user@host[:port]" (Daytona-style ad-hoc creds);
//  2. parseSSHAlias for a bare hostname or ~/.ssh/config alias — ssh resolves
//     user/port/identity via its own config, no inventory entry required.
//
// If none match, returns nil and the caller errors with "unknown host".
func (e *Executor) resolveSSHHost(name string) *inventory.Server {
	if srv := e.findServer(name); srv != nil {
		return srv
	}
	if srv, ok := parseRawSSHTarget(name); ok {
		return srv
	}
	if srv, ok := parseSSHAlias(name); ok {
		return srv
	}
	return nil
}

// parseRawSSHTarget parses "user@host" or "user@host:port" into a synthetic
// Server suitable for sshArgs. Returns false when the input doesn't look like
// a raw SSH target, so callers can fall through to a normal "unknown host"
// error path.
func parseRawSSHTarget(target string) (*inventory.Server, bool) {
	at := strings.LastIndex(target, "@")
	if at <= 0 || at == len(target)-1 {
		return nil, false
	}
	user := target[:at]
	rest := target[at+1:]
	if user == "" || rest == "" {
		return nil, false
	}

	host := rest
	port := 0
	if colon := strings.LastIndex(rest, ":"); colon > 0 && colon < len(rest)-1 {
		if p, err := strconv.Atoi(rest[colon+1:]); err == nil && p > 0 && p < 65536 {
			host = rest[:colon]
			port = p
		}
	}

	if !looksLikeHostname(host) {
		return nil, false
	}

	return &inventory.Server{
		Name: target,
		User: user,
		IP:   host,
		Port: port,
	}, true
}

// parseSSHAlias treats `name` as a plain hostname or ~/.ssh/config alias and
// returns a synthetic Server with only SSHAlias set. sshArgs short-circuits
// on SSHAlias under AddrAuto and emits `ssh <alias>`, so ssh resolves user,
// port, identity, ProxyJump, etc. from its own config. Connection failure
// surfaces as the underlying ssh exit code/stderr rather than a shellkit
// "unknown host" error.
func parseSSHAlias(name string) (*inventory.Server, bool) {
	if !looksLikeHostname(name) {
		return nil, false
	}
	return &inventory.Server{
		Name:     name,
		SSHAlias: name,
	}, true
}

func looksLikeHostname(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func (e *Executor) mergeFanoutOutputs(stepName string, perHost []StepResult) StepResult {
	var b strings.Builder
	merged := StepResult{
		Name:    stepName,
		Outputs: make(map[string]string),
	}
	for _, r := range perHost {
		fmt.Fprintf(&b, "=== %s ===\n%s\n", r.Host, r.Stdout)
		for k, v := range r.Outputs {
			merged.Outputs[k] = v
		}
	}
	mergedPath := e.store.StepFilePath(stepName)
	if wErr := os.WriteFile(mergedPath, []byte(b.String()), 0644); wErr != nil {
		merged.Error = fmt.Sprintf("output write failed: %v", wErr)
	} else {
		merged.FilePath = mergedPath
	}
	merged.Stdout = b.String()
	return merged
}

// wrapperShell picks the remote interpreter that executes the wrapScript
// wrapper itself. The wrapper is shell code — the step's entrypoint is applied
// INSIDE it (`$_UNBUF <entrypoint> $_SSH_SCRIPT`) — so feeding the wrapper to a
// non-shell interpreter (python3 -s, node -s) breaks: bash setup lines hit the
// wrong parser. Shell entrypoints keep running their own wrapper (byte-for-byte
// legacy behavior); everything else runs the wrapper under bash.
func wrapperShell(entrypoint string) string {
	switch entrypoint {
	case "bash", "sh", "zsh":
		return entrypoint
	}
	return "bash"
}

func wrapScript(body, entrypoint, nonce string) string {
	outMarker := outputMarkerFor(nonce)
	errMarker := stderrMarkerFor(nonce)
	delim := heredocDelimiter(nonce)

	if entrypoint == "" {
		entrypoint = "bash"
	}

	return fmt.Sprintf(`_SSH_OUTPUT=$(mktemp)
_SSH_STDERR=$(mktemp)
_SSH_SCRIPT=$(mktemp)
cat > $_SSH_SCRIPT << '%s'
%s
%s
export OUTPUT=$_SSH_OUTPUT
_UNBUF=""
command -v stdbuf >/dev/null 2>&1 && _UNBUF="stdbuf -oL"
$_UNBUF %s $_SSH_SCRIPT 2>$_SSH_STDERR
_exit=$?
echo ""
echo "%s"
cat $_SSH_OUTPUT 2>/dev/null
echo "%s"
cat $_SSH_STDERR 2>/dev/null
rm -f $_SSH_OUTPUT $_SSH_STDERR $_SSH_SCRIPT
exit $_exit
`, delim, body, delim, entrypoint, outMarker, errMarker)
}

func parseWrappedOutputNonce(raw, nonce string) (stdout, outputs, stderr string) {
	outMarker := outputMarkerFor(nonce)
	errMarker := stderrMarkerFor(nonce)

	outIdx := strings.Index(raw, outMarker)
	if outIdx < 0 {
		return raw, "", ""
	}

	stdout = strings.TrimSuffix(raw[:outIdx], "\n")

	rest := raw[outIdx+len(outMarker):]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}

	errIdx := strings.Index(rest, errMarker)
	if errIdx < 0 {
		return stdout, strings.TrimSpace(rest), ""
	}

	outputs = strings.TrimSpace(rest[:errIdx])
	afterErr := rest[errIdx+len(errMarker):]
	if len(afterErr) > 0 && afterErr[0] == '\n' {
		afterErr = afterErr[1:]
	}
	stderr = strings.TrimSpace(afterErr)
	return
}

func scriptExt(ep string) string {
	switch ep {
	case "node", "deno", "bun":
		return ".js"
	case "python3", "python":
		return ".py"
	case "ruby":
		return ".rb"
	case "perl":
		return ".pl"
	default:
		return ".sh"
	}
}

func buildTmuxBody(session, nonce string, verbs []TmuxVerb) (string, error) {
	var wireLines []string
	for _, v := range verbs {
		wireLines = append(wireLines, v.Wire)
	}
	verbStream := strings.Join(wireLines, "\n")

	noncePrefix := nonce
	if len(noncePrefix) > 8 {
		noncePrefix = noncePrefix[:8]
	}
	delim := "SHELLKIT_VERBS_" + noncePrefix
	interp := GenerateInterp(session, nonce, delim)
	return interp + verbStream + "\n" + delim + "\n", nil
}

type tmuxTargetRunner func(ctx context.Context, target string) (StepResult, error)

func (e *Executor) executeTmux(ctx context.Context, stepIdx int, step *Step) ([]StepResult, error) {
	targets := step.Hosts
	if len(targets) == 0 {
		return nil, fmt.Errorf("step %q: tmux requires at least one target", step.Name)
	}

	verbs, err := ParseVerbScript(step.Body)
	if err != nil {
		return nil, fmt.Errorf("step %q: %w", step.Name, err)
	}

	timeout := step.Config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	traced := shouldTrace(step.Config, "bash")
	fanout := len(targets) > 1

	runOne := func(tctx context.Context, target string) (StepResult, error) {
		host, session, err := validateTmuxTarget(target)
		if err != nil {
			return StepResult{}, err
		}
		srv := e.resolveSSHHost(host)
		if srv == nil {
			return StepResult{}, fmt.Errorf("unknown host %q", host)
		}
		if step.Config.Jump != "" {
			srv.ProxyJump = step.Config.Jump
		}
		if step.Config.Identity != "" {
			srv.Identity = step.Config.Identity
			srv.IdentitiesOnly = true
		}

		nonce := execNonce()
		body, err := buildTmuxBody(session, nonce, verbs)
		if err != nil {
			return StepResult{}, err
		}
		if traced {
			body = prependTrace(body, nonce)
		}
		wrapped := wrapScript(body, "bash", nonce)

		result := e.runSSH(tctx, srv, wrapped, nonce, stepIdx, step.Name, target, fanout, traced, "bash", timeout)
		result.ShowTrace = traceRequested(step.Config)
		return result, nil
	}

	if !fanout {
		tctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		result, err := runOne(tctx, targets[0])
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.Name, err)
		}
		outPath := e.store.StepFilePath(step.Name)
		if wErr := os.WriteFile(outPath, []byte(result.Stdout), 0644); wErr != nil {
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += fmt.Sprintf("output write failed: %v", wErr)
		} else {
			result.FilePath = outPath
		}
		return []StepResult{result}, nil
	}

	return e.executeTmuxFanout(ctx, step, targets, runOne)
}

func (e *Executor) executeTmuxFanout(ctx context.Context, step *Step, targets []string, runner tmuxTargetRunner) ([]StepResult, error) {
	timeout := step.Config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	const maxConcurrency = 32
	sem := make(chan struct{}, maxConcurrency)
	results := make([]StepResult, len(targets))
	var wg sync.WaitGroup

	var fctx context.Context
	var fcancel context.CancelFunc
	if step.Config.ContinueOnError {
		fctx, fcancel = ctx, func() {}
	} else {
		fctx, fcancel = context.WithCancel(ctx)
	}
	defer fcancel()

	for i, target := range targets {
		wg.Add(1)
		go func(idx int, tgt string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = StepResult{
						Name: step.Name, Host: tgt, ExitCode: 1,
						Error: fmt.Sprintf("panic: %v", r),
					}
					if !step.Config.ContinueOnError {
						fcancel()
					}
				}
			}()

			select {
			case sem <- struct{}{}:
			case <-fctx.Done():
				results[idx] = StepResult{
					Name: step.Name, Host: tgt, ExitCode: 1,
					Error: fctx.Err().Error(),
				}
				return
			}
			defer func() { <-sem }()

			tctx, tcancel := context.WithTimeout(fctx, time.Duration(timeout)*time.Second)
			defer tcancel()

			result, err := runner(tctx, tgt)
			if err != nil {
				results[idx] = StepResult{
					Name: step.Name, Host: tgt, ExitCode: 1, Error: err.Error(),
				}
			} else {
				results[idx] = result
			}

			if results[idx].ExitCode != 0 && !step.Config.ContinueOnError {
				fcancel()
			}
		}(i, target)
	}

	wg.Wait()

	for i := range results {
		if results[i].Host != "" {
			outPath := e.store.StepFilePathForHost(step.Name, results[i].Host)
			if wErr := os.WriteFile(outPath, []byte(results[i].Stdout), 0644); wErr != nil {
				if results[i].Error != "" {
					results[i].Error += "; "
				}
				results[i].Error += fmt.Sprintf("output write failed: %v", wErr)
			} else {
				results[i].FilePath = outPath
			}
		}
	}

	merged := e.mergeFanoutOutputs(step.Name, results)
	results = append(results, merged)

	return results, nil
}

func matchFilter(s inventory.Server, filter string) bool {
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return strings.Contains(strings.ToLower(s.Name), strings.ToLower(filter)) ||
			strings.Contains(strings.ToLower(s.SSHAlias), strings.ToLower(filter))
	}
	key, val := parts[0], parts[1]
	switch key {
	case "provider":
		return s.Provider == val
	case "role":
		return s.Role == val
	case "project":
		return s.Project == val
	case "location":
		return s.Location == val
	case "state":
		return s.State == val
	case "group":
		return s.Group == val
	default:
		return false
	}
}
