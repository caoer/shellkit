package sshconn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
)

// dockerSSHEnv provisions Docker containers for SSH integration tests.
// Call setup() to start, teardown() when done.
type dockerSSHEnv struct {
	t          *testing.T
	network    string
	proxy      string // container name
	target     string // container name
	proxyPort  string // host port for proxy
	targetPort string // host port for target
	targetIP   string // internal IP of target on docker network
	keyPath    string // ephemeral test key
}

func newDockerSSHEnv(t *testing.T) *dockerSSHEnv {
	return &dockerSSHEnv{
		t:          t,
		network:    "shellkit-test-net",
		proxy:      "shellkit-test-proxy",
		target:     "shellkit-test-target",
		proxyPort:  "2222",
		targetPort: "2223",
	}
}

func (e *dockerSSHEnv) setup() {
	t := e.t

	// Generate ephemeral test key.
	keyFile, err := os.CreateTemp("", "shellkit-test-key-*")
	if err != nil {
		t.Fatalf("create temp key file: %v", err)
	}
	keyFile.Close()
	os.Remove(keyFile.Name())
	e.keyPath = keyFile.Name()
	run(t, "ssh-keygen", "-t", "ed25519", "-f", e.keyPath, "-N", "", "-q")

	pubKey, err := os.ReadFile(e.keyPath + ".pub")
	if err != nil {
		t.Fatalf("read pub key: %v", err)
	}

	// Docker network.
	runIgnoreErr("docker", "network", "create", e.network)

	// Proxy container: key auth only.
	run(t, "docker", "run", "-d",
		"--name", e.proxy,
		"--network", e.network,
		"-p", e.proxyPort+":22",
		"ubuntu:22.04", "bash", "-c", fmt.Sprintf(`
			apt-get update -qq && apt-get install -y -qq openssh-server >/dev/null 2>&1
			mkdir -p /run/sshd /root/.ssh
			echo '%s' > /root/.ssh/authorized_keys
			chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
			sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
			echo 'PasswordAuthentication no' >> /etc/ssh/sshd_config
			echo 'MaxAuthTries 20' >> /etc/ssh/sshd_config
			/usr/sbin/sshd -D
		`, strings.TrimSpace(string(pubKey))))

	// Target container: password auth only.
	run(t, "docker", "run", "-d",
		"--name", e.target,
		"--network", e.network,
		"-p", e.targetPort+":22",
		"ubuntu:22.04", "bash", "-c", `
			apt-get update -qq && apt-get install -y -qq openssh-server >/dev/null 2>&1
			mkdir -p /run/sshd
			echo 'root:targetpass456' | chpasswd
			sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
			echo 'PasswordAuthentication yes' >> /etc/ssh/sshd_config
			echo 'MaxAuthTries 20' >> /etc/ssh/sshd_config
			/usr/sbin/sshd -D
		`)

	// Wait for sshd in both containers.
	t.Log("waiting for sshd in proxy and target...")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		outP, _ := exec.Command("docker", "exec", e.proxy, "pgrep", "-a", "sshd").CombinedOutput()
		outT, _ := exec.Command("docker", "exec", e.target, "pgrep", "-a", "sshd").CombinedOutput()
		if strings.Contains(string(outP), "/usr/sbin/sshd") && strings.Contains(string(outT), "/usr/sbin/sshd") {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Get target's internal IP.
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", e.target).Output()
	if err != nil {
		t.Fatalf("get target IP: %v", err)
	}
	e.targetIP = strings.TrimSpace(string(out))
	if e.targetIP == "" {
		t.Fatal("target container has no IP on docker network")
	}
	t.Logf("proxy=%s:%s  target=%s (internal %s)", e.proxy, e.proxyPort, e.target, e.targetIP)

	// Verify direct connectivity.
	e.verifyDirect(t)
}

func (e *dockerSSHEnv) verifyDirect(t *testing.T) {
	// Proxy: key auth
	out, err := exec.Command("ssh",
		"-i", e.keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-p", e.proxyPort, "root@127.0.0.1",
		"echo proxy_ok").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "proxy_ok") {
		t.Fatalf("proxy direct SSH failed: %v\n%s", err, out)
	}

	// Target: password auth
	out, err = exec.Command("sshpass", "-p", "targetpass456", "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "PreferredAuthentications=password",
		"-o", "PubkeyAuthentication=no",
		"-p", e.targetPort, "root@127.0.0.1",
		"echo target_ok").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "target_ok") {
		t.Fatalf("target direct SSH failed: %v\n%s", err, out)
	}
}

func (e *dockerSSHEnv) teardown() {
	runIgnoreErr("docker", "rm", "-f", e.proxy)
	runIgnoreErr("docker", "rm", "-f", e.target)
	runIgnoreErr("docker", "network", "rm", e.network)
	os.Remove(e.keyPath)
	os.Remove(e.keyPath + ".pub")
}

func run(t *testing.T, name string, args ...string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func runIgnoreErr(name string, args ...string) {
	exec.Command(name, args...).Run()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRateLimit_DockerSSH exercises the rate limiter and ProxyJump + password
// auth against real SSH servers in Docker containers.
//
// Run with: SHELLKIT_DOCKER_TEST=1 go test -run TestRateLimit_DockerSSH -v -timeout 120s
func TestRateLimit_DockerSSH(t *testing.T) {
	if os.Getenv("SHELLKIT_DOCKER_TEST") == "" {
		t.Skip("set SHELLKIT_DOCKER_TEST=1 to run (requires Docker)")
	}

	env := newDockerSSHEnv(t)
	env.setup()
	defer env.teardown()

	t.Run("RateLimiterThrottles", func(t *testing.T) {
		rl := &hostRateLimiter{
			records: make(map[string][]time.Time),
			limit:   3,
			window:  500 * time.Millisecond,
		}
		ctx := context.Background()
		host := "127.0.0.1:2222"

		start := time.Now()
		for i := 0; i < 6; i++ {
			if err := rl.Acquire(ctx, host); err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
		}
		elapsed := time.Since(start)
		if elapsed < 400*time.Millisecond {
			t.Errorf("expected throttling (~500ms), completed in %v", elapsed)
		}
		t.Logf("6 acquires with limit=3/500ms took %v", elapsed)
	})

	t.Run("DirectPasswordAuth", func(t *testing.T) {
		rl := &hostRateLimiter{
			records: make(map[string][]time.Time),
			limit:   5,
			window:  2 * time.Second,
		}
		ctx := context.Background()
		host := "127.0.0.1:" + env.targetPort
		successes := 0

		for i := 0; i < 5; i++ {
			if err := rl.Acquire(ctx, host); err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
			cmd := exec.CommandContext(ctx, "sshpass", "-p", "targetpass456", "ssh",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "PreferredAuthentications=password",
				"-o", "PubkeyAuthentication=no",
				"-o", "ConnectTimeout=5",
				"-p", env.targetPort, "root@127.0.0.1",
				"echo ok")
			out, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(out), "ok") {
				successes++
			}
		}
		if successes < 4 {
			t.Errorf("expected >=4/5 successes, got %d", successes)
		}
		t.Logf("direct password auth: %d/5 success", successes)
	})

	t.Run("ProxyJump_KeyAuthWorksProbe", func(t *testing.T) {
		// Replicate keyAuthWorks: publickey-only probe through proxy to
		// password-only target. Must fail (target doesn't accept keys).
		cmd := exec.Command("ssh",
			"-o", "PreferredAuthentications=publickey",
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=8",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-i", env.keyPath,
			"-o", fmt.Sprintf("ProxyCommand=ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -W %%h:%%p -p %s root@127.0.0.1",
				env.keyPath, env.proxyPort),
			"-l", "root", "-p", "22",
			env.targetIP,
			"true")
		err := cmd.Run()
		if err == nil {
			t.Fatal("keyAuthWorks probe should FAIL against password-only target")
		}
		t.Log("keyAuthWorks probe correctly returned false (target is password-only)")
	})

	t.Run("ProxyJump_PasswordAuth", func(t *testing.T) {
		// Replicate sshPasswordInvocation through ProxyJump:
		// sshpass -e ssh <passwordSSHOpts> -J proxy target
		// Proxy: key auth via -i flag (inherited by ProxyCommand)
		// Target: password auth via sshpass

		successes := 0
		for i := 0; i < 5; i++ {
			cmd := exec.Command("sshpass", "-p", "targetpass456", "ssh",
				// passwordSSHOpts
				"-o", "PreferredAuthentications=password",
				"-o", "PubkeyAuthentication=no",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "NumberOfPasswordPrompts=1",
				// ProxyCommand instead of -J so we can pass the test key to proxy
				"-o", fmt.Sprintf("ProxyCommand=ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -W %%h:%%p -p %s root@127.0.0.1",
					env.keyPath, env.proxyPort),
				"-l", "root", "-p", "22",
				env.targetIP,
				fmt.Sprintf("echo proxyjump_conn_%d_ok && hostname", i))
			out, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(out), fmt.Sprintf("proxyjump_conn_%d_ok", i)) {
				successes++
			} else {
				t.Logf("conn %d failed: %v %s", i, err, out)
			}
		}
		if successes < 4 {
			t.Errorf("expected >=4/5 ProxyJump+password successes, got %d", successes)
		}
		t.Logf("ProxyJump+password auth: %d/5 success", successes)
	})

	t.Run("ProxyJump_RateLimited", func(t *testing.T) {
		// Verify rate limiter works with ProxyJump connections.
		rl := &hostRateLimiter{
			records: make(map[string][]time.Time),
			limit:   3,
			window:  1 * time.Second,
		}
		ctx := context.Background()
		targetKey := env.targetIP + ":22"

		// Fire 6 connections — first 3 immediate, next 3 throttled.
		start := time.Now()
		successes := 0
		for i := 0; i < 6; i++ {
			if err := rl.Acquire(ctx, targetKey); err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
			cmd := exec.CommandContext(ctx, "sshpass", "-p", "targetpass456", "ssh",
				"-o", "PreferredAuthentications=password",
				"-o", "PubkeyAuthentication=no",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "NumberOfPasswordPrompts=1",
				"-o", "ConnectTimeout=10",
				"-o", fmt.Sprintf("ProxyCommand=ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -W %%h:%%p -p %s root@127.0.0.1",
					env.keyPath, env.proxyPort),
				"-l", "root", "-p", "22",
				env.targetIP,
				"echo ok")
			out, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(out), "ok") {
				successes++
			}
		}
		elapsed := time.Since(start)
		if elapsed < 800*time.Millisecond {
			t.Errorf("expected throttling (~1s for second batch), completed in %v", elapsed)
		}
		if successes < 5 {
			t.Errorf("expected >=5/6 successes, got %d", successes)
		}
		t.Logf("ProxyJump+rate-limit: %d/6 success in %v", successes, elapsed)
	})

	t.Run("SshRateLimitKey_RawTarget", func(t *testing.T) {
		// A raw user@host:port target resolves to a Server with an explicit
		// IP+port (what parseRawSSHTarget produces); its rate-limit key must be
		// the resolved address, not an empty/default placeholder.
		srv := &inventory.Server{Name: "root@127.0.0.1:2222", User: "root", IP: "127.0.0.1", Port: 2222}
		key := SSHRateLimitKey(srv)
		if key == "" || key == ":22" || key == ":0" {
			t.Fatalf("raw target key should not be empty/default, got %q", key)
		}
		t.Logf("raw target key: %q", key)
	})

	t.Run("SshRateLimitKey_AliasNeverCollides", func(t *testing.T) {
		a := &inventory.Server{Name: "host-alpha", SSHAlias: "host-alpha"}
		b := &inventory.Server{Name: "host-beta", SSHAlias: "host-beta"}
		if SSHRateLimitKey(a) == SSHRateLimitKey(b) {
			t.Fatalf("alias-only servers should have different keys: both got %q", SSHRateLimitKey(a))
		}
	})

	t.Run("SshArgs_ProxyJumpInPasswordPath", func(t *testing.T) {
		// Verify sshArgs includes -J when ProxyJump is set and addr pref
		// bypasses the alias (non-Auto).
		srv := inventory.Server{
			Name:      "test-target",
			SSHAlias:  "test-target",
			IP:        env.targetIP,
			User:      "root",
			Port:      22,
			ProxyJump: fmt.Sprintf("root@127.0.0.1:%s", env.proxyPort),
		}
		args := SSHArgs(&srv, inventory.AddrWan)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-J") {
			t.Fatalf("sshArgs with ProxyJump should include -J, got: %s", joined)
		}
		if !strings.Contains(joined, env.targetIP) {
			t.Fatalf("sshArgs should target %s, got: %s", env.targetIP, joined)
		}
		t.Logf("sshArgs with ProxyJump: %s", joined)

		// Verify passwordSSHOpts don't include -J (it comes from sshArgs).
		for _, opt := range passwordSSHOpts {
			if strings.Contains(opt, "ProxyJump") || strings.Contains(opt, "-J") {
				t.Errorf("passwordSSHOpts should not contain ProxyJump, found: %s", opt)
			}
		}
	})
}
