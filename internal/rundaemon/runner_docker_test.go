package rundaemon

// runner_docker_test.go — the U8 integration proof: the runner bootstrap + the
// ndjson protocol client driven against a REAL sshd in Docker, pushing the REAL
// embedded linux binary and exercising the failure paths a local unit test can
// only fake (real process kill, real sshpass auth, real remote digest guard,
// real fd-separation over ssh).
//
// It reuses internal/sshconn/ratelimit_docker_test.go's container pattern (spin a
// real sshd container, SHELLKIT_DOCKER_TEST=1 gate, key + password auth) but does
// NOT touch that package's containers/ports (own names + ports, no shared net).
//
// Boundary vs. production transport: the real ssh arg-assembly
// (sshconn.ResolveInvocation) is covered by sshconn's own docker test. Here the
// thin docker transport supplies the ssh argv the way ResolveInvocation would, so
// U8 proves the layers ResolveInvocation feeds — the bootstrap ALGORITHM, the
// protocol Client, the real embedded binary, and the runner — end to end over a
// real ssh exec channel.
//
// Run: SHELLKIT_DOCKER_TEST=1 go test ./internal/rundaemon/... -run Docker -v -timeout 600s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/interp"
	"github.com/caoer/shellkit/internal/inventory"
)

// ---------------------------------------------------------------------------
// Docker environment (real sshd containers)
// ---------------------------------------------------------------------------

// dockerRunnerEnv provisions two ubuntu sshd containers for the runner
// integration tests: c1 accepts BOTH key and password auth (so the F4 sshpass
// path gets real coverage), c2 accepts key auth only (the cold second host for
// the mixed fan-out). Both carry procps (pgrep) and coreutils (sha256sum), which
// the kill and digest scenarios need.
type dockerRunnerEnv struct {
	t        *testing.T
	keyPath  string
	password string

	c1name, c1port string // key + password
	c2name, c2port string // key only
}

func newDockerRunnerEnv(t *testing.T) *dockerRunnerEnv {
	return &dockerRunnerEnv{
		t:        t,
		password: "rdtestpass789",
		c1name:   "shellkit-rdtest-c1",
		c1port:   "2301",
		c2name:   "shellkit-rdtest-c2",
		c2port:   "2302",
	}
}

func (e *dockerRunnerEnv) setup() {
	t := e.t

	// Ephemeral test key.
	keyFile, err := os.CreateTemp("", "shellkit-rdtest-key-*")
	if err != nil {
		t.Fatalf("create temp key file: %v", err)
	}
	keyFile.Close()
	os.Remove(keyFile.Name())
	e.keyPath = keyFile.Name()
	dockerRun(t, "ssh-keygen", "-t", "ed25519", "-f", e.keyPath, "-N", "", "-q")
	pubKey, err := os.ReadFile(e.keyPath + ".pub")
	if err != nil {
		t.Fatalf("read pub key: %v", err)
	}
	pub := strings.TrimSpace(string(pubKey))

	// Clear any leftovers from an aborted run, then start fresh.
	dockerIgnore("docker", "rm", "-f", e.c1name)
	dockerIgnore("docker", "rm", "-f", e.c2name)

	// c1: key + password. procps for pgrep (kill scenario).
	dockerRun(t, "docker", "run", "-d", "--name", e.c1name, "-p", e.c1port+":22",
		"ubuntu:22.04", "bash", "-c", fmt.Sprintf(`
			apt-get update -qq && apt-get install -y -qq openssh-server procps >/dev/null 2>&1
			mkdir -p /run/sshd /root/.ssh
			echo '%s' > /root/.ssh/authorized_keys
			chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
			echo 'root:%s' | chpasswd
			sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
			echo 'PasswordAuthentication yes' >> /etc/ssh/sshd_config
			echo 'MaxAuthTries 20' >> /etc/ssh/sshd_config
			/usr/sbin/sshd -D
		`, pub, e.password))

	// c2: key only.
	dockerRun(t, "docker", "run", "-d", "--name", e.c2name, "-p", e.c2port+":22",
		"ubuntu:22.04", "bash", "-c", fmt.Sprintf(`
			apt-get update -qq && apt-get install -y -qq openssh-server procps >/dev/null 2>&1
			mkdir -p /run/sshd /root/.ssh
			echo '%s' > /root/.ssh/authorized_keys
			chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
			sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
			echo 'PasswordAuthentication no' >> /etc/ssh/sshd_config
			echo 'MaxAuthTries 20' >> /etc/ssh/sshd_config
			/usr/sbin/sshd -D
		`, pub))

	// Wait for sshd in both (apt install finishes before sshd -D runs, so a live
	// sshd implies procps/coreutils are present too).
	t.Log("waiting for sshd in c1 and c2 (apt install runs first)...")
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		out1, _ := exec.Command("docker", "exec", e.c1name, "pgrep", "-a", "sshd").CombinedOutput()
		out2, _ := exec.Command("docker", "exec", e.c2name, "pgrep", "-a", "sshd").CombinedOutput()
		if strings.Contains(string(out1), "/usr/sbin/sshd") && strings.Contains(string(out2), "/usr/sbin/sshd") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	e.verifyDirect()
}

// verifyDirect proves the auth setup independently before any runner code runs.
func (e *dockerRunnerEnv) verifyDirect() {
	t := e.t
	// c1 key auth.
	out, err := exec.Command("ssh", "-i", e.keyPath,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "PreferredAuthentications=publickey", "-o", "ConnectTimeout=10",
		"-p", e.c1port, "root@127.0.0.1", "echo c1_key_ok").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "c1_key_ok") {
		t.Fatalf("c1 key SSH failed: %v\n%s", err, out)
	}
	// c1 password auth.
	out, err = exec.Command("sshpass", "-p", e.password, "ssh",
		"-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "NumberOfPasswordPrompts=1", "-o", "ConnectTimeout=10",
		"-p", e.c1port, "root@127.0.0.1", "echo c1_pw_ok").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "c1_pw_ok") {
		t.Fatalf("c1 password SSH failed: %v\n%s", err, out)
	}
	// c2 key auth.
	out, err = exec.Command("ssh", "-i", e.keyPath,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "PreferredAuthentications=publickey", "-o", "ConnectTimeout=10",
		"-p", e.c2port, "root@127.0.0.1", "echo c2_key_ok").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "c2_key_ok") {
		t.Fatalf("c2 key SSH failed: %v\n%s", err, out)
	}
	t.Logf("direct SSH verified: c1=%s (key+password) c2=%s (key)", e.c1port, e.c2port)
}

func (e *dockerRunnerEnv) teardown() {
	dockerIgnore("docker", "rm", "-f", e.c1name)
	dockerIgnore("docker", "rm", "-f", e.c2name)
	os.Remove(e.keyPath)
	os.Remove(e.keyPath + ".pub")
}

// sshArgv builds the ssh (key) or sshpass (password) invocation for a container
// port, mirroring the options sshconn uses so the runner exec inherits the same
// non-interactive posture. remoteCmd is the single remote command string.
func (e *dockerRunnerEnv) sshArgv(port, mode, remoteCmd string) (name string, args, env []string) {
	common := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-p", port, "root@127.0.0.1",
	}
	if mode == "password" {
		args = append([]string{"-e", "ssh",
			"-o", "PreferredAuthentications=password",
			"-o", "PubkeyAuthentication=no",
			"-o", "NumberOfPasswordPrompts=1"}, common...)
		args = append(args, remoteCmd)
		return "sshpass", args, []string{"SSHPASS=" + e.password}
	}
	args = append([]string{
		"-i", e.keyPath,
		"-o", "PreferredAuthentications=publickey",
		"-o", "IdentitiesOnly=yes"}, common...)
	args = append(args, remoteCmd)
	return "ssh", args, nil
}

// runSSH runs one remote command over ssh, feeding stdin (nil for none) and
// mapping the outcome to an execResult exactly as resolveExec does.
func (e *dockerRunnerEnv) runSSH(ctx context.Context, port, mode, remoteCmd string, stdin []byte) execResult {
	name, args, env := e.sshArgv(port, mode, remoteCmd)
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := execResult{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = err
		}
	}
	return res
}

// spawnRunner starts the runner over a persistent ssh exec channel and wraps its
// stdio in the protocol Client — the docker analog of SpawnSSH (which resolves
// its argv through ResolveInvocation; here the argv comes from sshArgv). The
// transport ctx keeps the ssh process alive; the RunStep ctx is separate so a
// step cancel rides the wire as an in-band signal rather than killing ssh.
func (e *dockerRunnerEnv) spawnRunner(ctx context.Context, port, mode, runnerPath string) (*Client, *exec.Cmd) {
	t := e.t
	name, args, env := e.sshArgv(port, mode, runnerPath)
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("runner stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("runner stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("runner stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runner over ssh: %v", err)
	}
	return NewClient(stdin, stdout, stderr), cmd
}

// platform reports the container's mapped goos/goarch (via the package's own
// mapUname), so path/digest assertions match whatever arch docker is running.
func (e *dockerRunnerEnv) platform(container string) (goos, goarch string) {
	t := e.t
	out, err := exec.Command("docker", "exec", container, "uname", "-s", "-m").Output()
	if err != nil {
		t.Fatalf("uname in %s: %v", container, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		t.Fatalf("unexpected uname output %q", out)
	}
	goos, goarch, ok := mapUname(f[0], f[1])
	if !ok {
		t.Fatalf("container %s uname %s/%s not mappable to a Go platform", container, f[0], f[1])
	}
	return goos, goarch
}

// pgrepMatches reports whether any process in container has pat in its argv.
func (e *dockerRunnerEnv) pgrepMatches(container, pat string) bool {
	out, _ := exec.Command("docker", "exec", container, "pgrep", "-f", pat).CombinedOutput()
	return strings.TrimSpace(string(out)) != ""
}

// dockerSh runs a /bin/sh command inside container (login shell expands $HOME).
func dockerSh(container, script string) (string, error) {
	out, err := exec.Command("docker", "exec", container, "sh", "-c", script).CombinedOutput()
	return string(out), err
}

func dockerRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func dockerIgnore(name string, args ...string) { _ = exec.Command(name, args...).Run() }

// ---------------------------------------------------------------------------
// dockerExec: a real-ssh sshExec for the Bootstrapper (records calls like
// bootstrap_test.go's hostSim, but runs against a real container).
// ---------------------------------------------------------------------------

type dockerExec struct {
	env  *dockerRunnerEnv
	port string
	mode string

	mu    sync.Mutex
	calls []simCall
}

func (d *dockerExec) run(ctx context.Context, srv *inventory.Server, remoteCmd string, stdin []byte) execResult {
	d.mu.Lock()
	d.calls = append(d.calls, simCall{cmdKind(remoteCmd), remoteCmd, append([]byte(nil), stdin...), srv})
	d.mu.Unlock()
	return d.env.runSSH(ctx, d.port, d.mode, remoteCmd, stdin)
}

func (d *dockerExec) countOf(kind string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The integration test
// ---------------------------------------------------------------------------

func TestRunner_DockerSSH(t *testing.T) {
	if os.Getenv("SHELLKIT_DOCKER_TEST") == "" {
		t.Skip("set SHELLKIT_DOCKER_TEST=1 to run (requires Docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not usable: %v", err)
	}

	env := newDockerRunnerEnv(t)
	env.setup()
	defer env.teardown()

	ctx := context.Background()
	srv1 := testServer("rdtest-c1")
	srv2 := testServer("rdtest-c2")

	// Scenario 1 — framing round-trip: bootstrap the real linux binary into c1,
	// drive a step over the real ssh exec channel, assert the output/result/io
	// frames and the runner's hello handshake all come back correct.
	t.Run("FramingRoundTrip", func(t *testing.T) {
		dx := &dockerExec{env: env, port: env.c1port, mode: "key"}
		bs := newTestBootstrapper(dx, os.Stderr)
		res := bs.Bootstrap(ctx, srv1)
		if res.Fallback {
			t.Fatalf("bootstrap fell back: %s", res.Reason)
		}
		t.Logf("bootstrapped runner at %s", res.RunnerPath)

		client, cmd := env.spawnRunner(ctx, env.c1port, "key", res.RunnerPath)
		out, err := client.RunStep(ctx, Step{
			Name: "roundtrip", Host: "c1",
			Program: []byte(`echo hello-stdout; echo err-line 1>&2; printf 'FOO=bar\n' >> "$OUTPUT"`),
		})
		_ = cmd.Wait()
		if err != nil {
			t.Fatalf("RunStep misuse error: %v", err)
		}
		if out.Exit != 0 {
			t.Fatalf("exit = %d (%s), want 0", out.Exit, out.Error)
		}
		if !strings.Contains(out.Stdout, "hello-stdout") {
			t.Errorf("stdout = %q, want it to contain hello-stdout", out.Stdout)
		}
		if !strings.Contains(out.Stderr, "err-line") {
			t.Errorf("stderr = %q, want it to contain err-line", out.Stderr)
		}
		if out.Outputs["FOO"] != "bar" {
			t.Errorf("outputs = %v, want FOO=bar", out.Outputs)
		}
		if out.RunnerHello.OS != "linux" || !strings.Contains(out.RunnerHello.Version, RunnerVersion) {
			t.Errorf("runner hello = %+v, want linux + version %s", out.RunnerHello, RunnerVersion)
		}
		t.Logf("hello os/arch/version = %s/%s/%s", out.RunnerHello.OS, out.RunnerHello.Arch, out.RunnerHello.Version)
	})

	// Scenario 2 — cold → warm: the first bootstrap pushes (cold), a fresh
	// bootstrapper on the same disk finds it via --version (warm) and does NOT
	// re-push; the pushed file lives at the hash-named path.
	t.Run("ColdThenWarm", func(t *testing.T) {
		// Start from a clean cache so "cold" is real.
		if _, err := dockerSh(env.c1name, `rm -rf "$HOME/.cache/shellkit"`); err != nil {
			t.Fatalf("clean cache: %v", err)
		}

		cold := &dockerExec{env: env, port: env.c1port, mode: "key"}
		bsCold := newTestBootstrapper(cold, os.Stderr)
		r1 := bsCold.Bootstrap(ctx, srv1)
		if r1.Fallback {
			t.Fatalf("cold bootstrap fell back: %s", r1.Reason)
		}
		if cold.countOf("push") != 1 {
			t.Errorf("cold push count = %d, want 1", cold.countOf("push"))
		}
		if s, err := dockerSh(env.c1name, `test -x "`+r1.RunnerPath+`" && echo EXISTS`); err != nil || !strings.Contains(s, "EXISTS") {
			t.Fatalf("pushed runner not at %s: %v %s", r1.RunnerPath, err, s)
		}

		warm := &dockerExec{env: env, port: env.c1port, mode: "key"}
		bsWarm := newTestBootstrapper(warm, os.Stderr)
		r2 := bsWarm.Bootstrap(ctx, srv1)
		if r2.Fallback {
			t.Fatalf("warm bootstrap fell back: %s", r2.Reason)
		}
		if r2.RunnerPath != r1.RunnerPath {
			t.Errorf("warm path %q != cold path %q", r2.RunnerPath, r1.RunnerPath)
		}
		if warm.countOf("push") != 0 {
			t.Errorf("warm push count = %d, want 0 (must not re-push)", warm.countOf("push"))
		}
		// The warm hit is now digest-gated (security): it verifies the cached
		// binary's remote sha256 against the daemon-computed digest, NOT its
		// self-reported --version. A version-string match alone is forgeable.
		if warm.countOf("cacheddigest") == 0 {
			t.Errorf("warm hit must verify the cached binary's digest; cacheddigest calls = 0")
		}
		if warm.countOf("version") != 0 {
			t.Errorf("warm hit must NOT trust --version; version calls = %d, want 0", warm.countOf("version"))
		}
		t.Logf("cold pushed, warm hit at %s with 0 re-push (digest-verified)", r2.RunnerPath)
	})

	// Scenario 3 — version-mismatch re-push + remote digest guard.
	t.Run("VersionMismatchAndDigestGuard", func(t *testing.T) {
		goos, goarch := env.platform(env.c1name)
		root := `$HOME/.cache/shellkit`
		final := runnerPath(root, goos, goarch)

		// (a) Plant a runnable binary that reports the WRONG version, then
		// bootstrap: the version probe misses, one real push replaces it, the
		// exec-test then matches.
		if _, err := dockerSh(env.c1name, fmt.Sprintf(
			`mkdir -p "%s"; printf '#!/bin/sh\necho bogus-version\n' > "%s"; chmod +x "%s"`, root, final, final)); err != nil {
			t.Fatalf("plant bogus binary: %v", err)
		}
		if s, _ := dockerSh(env.c1name, `"`+final+`" --version`); !strings.Contains(s, "bogus-version") {
			t.Fatalf("bogus binary not planted: %q", s)
		}
		dx := &dockerExec{env: env, port: env.c1port, mode: "key"}
		bs := newTestBootstrapper(dx, os.Stderr)
		res := bs.Bootstrap(ctx, srv1)
		if res.Fallback {
			t.Fatalf("version-mismatch bootstrap fell back: %s", res.Reason)
		}
		if dx.countOf("push") != 1 {
			t.Errorf("push count = %d, want 1 (version miss → push once)", dx.countOf("push"))
		}
		if s, _ := dockerSh(env.c1name, `"`+final+`" --version`); !strings.Contains(s, RunnerVersion) {
			t.Errorf("after re-push, --version = %q, want %s", s, RunnerVersion)
		}
		t.Logf("version mismatch detected and re-pushed; runner now reports %s", RunnerVersion)

		// (b) Real remote digest guard: run the actual pushCmd with a deliberately
		// wrong `want` digest and the real bytes on stdin. The remote shell computes
		// the true sha256, sees the mismatch, aborts with exit 34, never commits.
		raw, err := gunzip(mustRunnerGz(t, goos, goarch))
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		guardFinal := final + ".digestguard"
		wrongWant := "00000000000000000000000000000000000000000000000000000000deadbeef"
		gr := env.runSSH(ctx, env.c1port, "key", pushCmd(root, guardFinal, wrongWant), raw)
		if gr.ExitCode != exitDigestMismatch {
			t.Errorf("digest-guard push exit = %d, want %d (SHELLKIT_DIGEST=... exit 34)", gr.ExitCode, exitDigestMismatch)
		}
		if got := parseDigest(string(gr.Stdout)); got == "" || got == wrongWant {
			t.Errorf("remote digest = %q, want the real (non-bogus) sha256", got)
		}
		if s, _ := dockerSh(env.c1name, `test -e "`+guardFinal+`" && echo EXISTS || echo ABSENT`); !strings.Contains(s, "ABSENT") {
			t.Errorf("digest-mismatch must NOT commit the final file; got %q", s)
		}
		t.Logf("remote digest guard aborted the commit (exit %d, no final file)", exitDigestMismatch)
	})

	// Scenario 4 — kill mid-step, no remote orphan. Start a step whose child is a
	// long-lived sleep, confirm it is running in the container, cancel the step
	// (in-band TERM over the wire), then assert the remote process is GONE.
	t.Run("KillMidStepNoOrphan", func(t *testing.T) {
		dx := &dockerExec{env: env, port: env.c1port, mode: "key"}
		res := newTestBootstrapper(dx, os.Stderr).Bootstrap(ctx, srv1)
		if res.Fallback {
			t.Fatalf("bootstrap fell back: %s", res.Reason)
		}

		transportCtx, cancelT := context.WithTimeout(ctx, 90*time.Second)
		defer cancelT()
		client, cmd := env.spawnRunner(transportCtx, env.c1port, "key", res.RunnerPath)

		marker := fmt.Sprintf("31415926535%d", time.Now().UnixNano())
		stepCtx, cancelStep := context.WithCancel(ctx)
		outCh := make(chan StepOutcome, 1)
		go func() {
			o, _ := client.RunStep(stepCtx, Step{
				Name: "killme", Host: "c1",
				Program: []byte("sleep " + marker + "\n"),
			})
			outCh <- o
		}()

		// Prove the child actually started (real-process proof the unit test faked).
		running := false
		for i := 0; i < 50; i++ {
			if env.pgrepMatches(env.c1name, marker) {
				running = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !running {
			cancelStep()
			<-outCh
			_ = cmd.Wait()
			t.Fatal("child sleep never started in the container")
		}

		cancelStep() // in-band TERM → runner cancels step → kills the process group
		out := <-outCh
		_ = cmd.Wait()

		if out.Exit != 137 {
			t.Errorf("killed step exit = %d, want 137 (clean in-band kill, not wire cut: %+v)", out.Exit, out)
		}
		gone := false
		for i := 0; i < 30; i++ {
			if !env.pgrepMatches(env.c1name, marker) {
				gone = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !gone {
			t.Error("remote sleep survived the kill — orphan left behind")
		}
		t.Logf("child started then killed; no remote orphan (exit %d)", out.Exit)
	})

	// Scenario 5 — password-auth host runner exec (F4: container-only coverage).
	// Bootstrap + run through c1's password auth; assert it works like key auth.
	t.Run("PasswordAuthRunnerExec", func(t *testing.T) {
		dx := &dockerExec{env: env, port: env.c1port, mode: "password"}
		res := newTestBootstrapper(dx, os.Stderr).Bootstrap(ctx, srv1)
		if res.Fallback {
			t.Fatalf("password-auth bootstrap fell back: %s", res.Reason)
		}
		client, cmd := env.spawnRunner(ctx, env.c1port, "password", res.RunnerPath)
		out, _ := client.RunStep(ctx, Step{
			Name: "pw", Host: "c1",
			Program: []byte(`echo pw-auth-ok; printf 'AUTH=password\n' >> "$OUTPUT"`),
		})
		_ = cmd.Wait()
		if out.Exit != 0 {
			t.Fatalf("password-auth step exit = %d (%s)", out.Exit, out.Error)
		}
		if !strings.Contains(out.Stdout, "pw-auth-ok") || out.Outputs["AUTH"] != "password" {
			t.Errorf("password-auth step: stdout=%q outputs=%v", out.Stdout, out.Outputs)
		}
		t.Logf("F4: runner bootstrapped + executed over sshpass password auth")
	})

	// Scenario 6 — gap-route fallback (routing boundary). The step-BODY gap router
	// is interp.Preflight (daemon-side, pre-ssh): a `jobs` construct routes to
	// realbash so the runner is never involved. rundaemon consumes only its OWN
	// fallback signals — Bootstrap Result.Fallback and RunStep Exit -1 (wire
	// cut / protocol error / proto mismatch); wiring the Preflight verdict into
	// dispatch (skip SpawnSSH → legacy) is U6b, out of rundaemon's surface.
	t.Run("GapRouteFallbackBoundary", func(t *testing.T) {
		v, err := interp.Preflight([]byte("jobs\n"))
		if err != nil {
			t.Fatalf("preflight(jobs): %v", err)
		}
		if v.Route != interp.RouteRealbash {
			t.Errorf("preflight(jobs) route = %q, want %q (gap → legacy)", v.Route, interp.RouteRealbash)
		}
		// A clean body routes to the interp/runner path (the complement).
		vClean, err := interp.Preflight([]byte(`echo ok`))
		if err != nil {
			t.Fatalf("preflight(clean): %v", err)
		}
		if vClean.Route != interp.RouteInterp {
			t.Errorf("preflight(clean) route = %q, want %q", vClean.Route, interp.RouteInterp)
		}
		t.Logf("boundary: interp.Preflight routes gap→realbash / clean→interp; rundaemon dispatch wiring is U6b (reason=%q)", v.Reason)
	})

	// Scenario 7 — mixed fan-out: c1 warm, c2 cold. Bootstrap + run on both
	// concurrently, assert per-host outcomes merge (both exit 0, distinct
	// hostnames) and only the cold host pushed.
	t.Run("MixedFanOut", func(t *testing.T) {
		// Warm c1 first (idempotent), leave c2 cold.
		if r := newTestBootstrapper(&dockerExec{env: env, port: env.c1port, mode: "key"}, os.Stderr).Bootstrap(ctx, srv1); r.Fallback {
			t.Fatalf("pre-warm c1 fell back: %s", r.Reason)
		}
		if _, err := dockerSh(env.c2name, `rm -rf "$HOME/.cache/shellkit"`); err != nil {
			t.Fatalf("clean c2 cache: %v", err)
		}

		type hostJob struct {
			name, port string
			srv        *inventory.Server
			dx         *dockerExec
		}
		jobs := []*hostJob{
			{"c1-warm", env.c1port, srv1, &dockerExec{env: env, port: env.c1port, mode: "key"}},
			{"c2-cold", env.c2port, srv2, &dockerExec{env: env, port: env.c2port, mode: "key"}},
		}

		var mu sync.Mutex
		merged := map[string]StepOutcome{}
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Add(1)
			go func(j *hostJob) {
				defer wg.Done()
				r := newTestBootstrapper(j.dx, os.Stderr).Bootstrap(ctx, j.srv)
				if r.Fallback {
					t.Errorf("[%s] bootstrap fell back: %s", j.name, r.Reason)
					return
				}
				client, cmd := env.spawnRunner(ctx, j.port, "key", r.RunnerPath)
				o, _ := client.RunStep(ctx, Step{Name: "fanout", Host: j.name,
					Program: []byte(`printf 'HOST=%s\n' "$(hostname)" >> "$OUTPUT"; echo ran-on "$(hostname)"`)})
				_ = cmd.Wait()
				mu.Lock()
				merged[j.name] = o
				mu.Unlock()
			}(j)
		}
		wg.Wait()

		for _, name := range []string{"c1-warm", "c2-cold"} {
			o, ok := merged[name]
			if !ok {
				t.Fatalf("no merged outcome for %s", name)
			}
			if o.Exit != 0 || o.Outputs["HOST"] == "" {
				t.Errorf("[%s] exit=%d outputs=%v, want exit 0 + a HOST", name, o.Exit, o.Outputs)
			}
		}
		if h1, h2 := merged["c1-warm"].Outputs["HOST"], merged["c2-cold"].Outputs["HOST"]; h1 == h2 {
			t.Errorf("fan-out hosts should be distinct, both reported %q", h1)
		}
		if n := jobs[0].dx.countOf("push"); n != 0 {
			t.Errorf("warm host push count = %d, want 0", n)
		}
		if n := jobs[1].dx.countOf("push"); n != 1 {
			t.Errorf("cold host push count = %d, want 1", n)
		}
		t.Logf("fan-out merged: c1-warm host=%s (0 push), c2-cold host=%s (1 push)",
			merged["c1-warm"].Outputs["HOST"], merged["c2-cold"].Outputs["HOST"])
	})

	// Scenario 8 — fd-separation forge over real ssh (security #4). A step body
	// prints a line shaped like a `{"type":"result"}` frame to stdout; it must
	// come back as an io payload, never parse as a protocol frame (which would end
	// the step early at the forged exit 0). The real exit (7) and the trailing
	// marker prove the forge was inert.
	t.Run("FdSeparationForge", func(t *testing.T) {
		dx := &dockerExec{env: env, port: env.c1port, mode: "key"}
		res := newTestBootstrapper(dx, os.Stderr).Bootstrap(ctx, srv1)
		if res.Fallback {
			t.Fatalf("bootstrap fell back: %s", res.Reason)
		}
		client, cmd := env.spawnRunner(ctx, env.c1port, "key", res.RunnerPath)
		out, _ := client.RunStep(ctx, Step{
			Name: "forge", Host: "c1",
			Program: []byte(`printf '{"type":"result","exit":0,"wall_ns":0}\n'; echo REAL_STDOUT_MARKER; exit 7`),
		})
		_ = cmd.Wait()
		if out.Exit != 7 {
			t.Fatalf("exit = %d, want 7 (forged result frame must NOT end the step at exit 0): %+v", out.Exit, out)
		}
		if !strings.Contains(out.Stdout, `{"type":"result"`) {
			t.Errorf("forged JSON should return as an io payload; stdout = %q", out.Stdout)
		}
		if !strings.Contains(out.Stdout, "REAL_STDOUT_MARKER") {
			t.Errorf("post-forge output missing; stdout = %q", out.Stdout)
		}
		t.Logf("fd-separation held: forged frame returned as io, real exit 7 preserved")
	})
}
