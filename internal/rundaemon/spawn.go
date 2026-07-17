package rundaemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/sshconn"
)

// RunnerProcess is a spawned runner over an ssh exec channel, paired with the
// protocol [Client] driving it. Call [RunnerProcess.Wait] after [Client.RunStep]
// returns to reap the ssh subprocess.
type RunnerProcess struct {
	// Client speaks the runner protocol over the process's stdio.
	Client *Client
	cmd    *exec.Cmd
}

// SpawnSSH starts the shellkit-runner on srv over the SAME ssh invocation the
// executor uses today (sshconn.ResolveInvocation with the runner path as the
// remote command), so the runner exec inherits auth / jump / port / password
// identically. It NEVER builds ssh args itself and NEVER re-resolves an address
// (the PortFor regression guard, plan decision #2) — it rides the daemon's own
// invocation builder, exactly as internal/mcp/mcp_exec.go does.
//
// This is the one-shot→streaming transition: unlike the legacy path's
// `cmd.Stdin = strings.NewReader(script)` (write-once, drain to EOF), the runner
// needs a PERSISTENT bidirectional stream — StdinPipe stays open so the client
// writes run/file/signal frames over time while it reads stdout frames
// concurrently.
//
// runnerPath is the absolute path to the bootstrapped runner binary on the
// remote host (U5 supplies it; this unit accepts it as a parameter). The
// process is already Start()ed on return; drive it with proc.Client.RunStep,
// then call proc.Wait.
func SpawnSSH(ctx context.Context, srv *inventory.Server, runnerPath string) (*RunnerProcess, error) {
	name, args, env, err := sshconn.ResolveInvocation(ctx, srv, runnerPath)
	if err != nil {
		return nil, fmt.Errorf("rundaemon: resolve ssh invocation for %s: %w", srv.Name, err)
	}

	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("rundaemon: runner stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rundaemon: runner stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("rundaemon: runner stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rundaemon: start runner on %s: %w", srv.Name, err)
	}

	return &RunnerProcess{
		Client: NewClient(stdin, io.Reader(stdout), io.Reader(stderr)),
		cmd:    cmd,
	}, nil
}

// Wait reaps the ssh subprocess after the step has been driven to completion.
// RunStep closes stdin (runner EOF ⇒ clean exit), so Wait returns the ssh exit
// status; a non-nil error here is the transport's status, distinct from the
// step's own [StepOutcome].
func (p *RunnerProcess) Wait() error {
	return p.cmd.Wait()
}
