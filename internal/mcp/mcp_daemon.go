package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultMCPPort = 19222

func StateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "shellkit")
}

func mcpPidFile() string {
	return filepath.Join(StateDir(), "mcp.pid")
}

func mcpLogFile() string {
	return filepath.Join(StateDir(), "mcp.log")
}

func Port() int {
	if env := os.Getenv("SHELLKIT_MCP_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			return p
		}
	}
	return defaultMCPPort
}

func mcpTokenFile() string {
	return filepath.Join(StateDir(), "token")
}

func mcpToken() string {
	if env := os.Getenv("SHELLKIT_MCP_TOKEN"); env != "" {
		return env
	}
	data, err := os.ReadFile(mcpTokenFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func mcpEnsureToken() (string, error) {
	if tok := mcpToken(); tok != "" {
		return tok, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(b)
	os.MkdirAll(StateDir(), 0755)
	if err := os.WriteFile(mcpTokenFile(), []byte(tok+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return tok, nil
}

func mcpURL(port int) string {
	return fmt.Sprintf("http://localhost:%d/mcp", port)
}

func readPid() (int, error) {
	data, err := os.ReadFile(mcpPidFile())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func writePid(pid int) error {
	os.MkdirAll(StateDir(), 0755)
	return os.WriteFile(mcpPidFile(), []byte(strconv.Itoa(pid)), 0644)
}

func isRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func mcpHealthCheck(url, token string) (*http.Response, error) {
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"healthcheck","version":"1.0.0"}}}`)
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// serveInvocation builds the argv (after the executable) and working directory
// for the `mcp serve` child that `mcp start` daemonizes.
//
// With an inventory the child is pinned to its absolute path and started in
// that file's directory, so relative "sops:" password_ref paths resolve.
//
// With NO inventory the child gets no -f at all. filepath.Abs("") returns the
// current directory, so forwarding it would hand the child `-f <cwd>` — a
// directory, which LoadInventory rejects as an unsupported format — and the
// daemon would exit immediately on every host without an inventory. The child
// then serves from the home directory with zero inventory hosts.
func serveInvocation(inventoryPath string, port int) (args []string, workDir string, err error) {
	tail := []string{"mcp", "serve", "-p", strconv.Itoa(port)}

	if inventoryPath == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, "", fmt.Errorf("resolve home directory: %w", herr)
		}
		return tail, home, nil
	}

	abs, aerr := filepath.Abs(inventoryPath)
	if aerr != nil {
		return nil, "", fmt.Errorf("resolve inventory path: %w", aerr)
	}
	return append([]string{"-f", abs}, tail...), filepath.Dir(abs), nil
}

func Start(inventoryPath string, port int) error {
	if pid, err := readPid(); err == nil && isRunning(pid) {
		return fmt.Errorf("already running (pid %d) on %s\nuse 'shellkit mcp restart' to restart", pid, mcpURL(port))
	}

	token, err := mcpEnsureToken()
	if err != nil {
		return err
	}

	serveArgs, serveDir, err := serveInvocation(inventoryPath, port)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	os.MkdirAll(StateDir(), 0755)
	logF, err := os.OpenFile(mcpLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}

	cmd := exec.Command(self, serveArgs...)
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = serveDir

	if err := cmd.Start(); err != nil {
		logF.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	logF.Close()

	url := mcpURL(port)
	ready := false
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if !isRunning(cmd.Process.Pid) {
			return fmt.Errorf("daemon exited immediately — check log: %s", mcpLogFile())
		}
		resp, err := mcpHealthCheck(url, token)
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
	}

	if !ready {
		fmt.Fprintf(os.Stderr, "started (pid %d) but not yet responding on %s\n", cmd.Process.Pid, url)
	} else {
		fmt.Fprintf(os.Stderr, "started (pid %d) on %s\n", cmd.Process.Pid, url)
	}
	fmt.Fprintf(os.Stderr, "log: %s\n", mcpLogFile())
	fmt.Fprintf(os.Stderr, "token: %s\n", mcpTokenFile())
	return nil
}

func Stop() error {
	pid, err := readPid()
	if err != nil {
		return fmt.Errorf("not running (no pid file)")
	}

	if !isRunning(pid) {
		os.Remove(mcpPidFile())
		return fmt.Errorf("not running (stale pid %d removed)", pid)
	}

	proc, _ := os.FindProcess(pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}

	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isRunning(pid) {
			os.Remove(mcpPidFile())
			fmt.Fprintf(os.Stderr, "stopped (pid %d)\n", pid)
			return nil
		}
	}

	proc.Signal(syscall.SIGKILL)
	os.Remove(mcpPidFile())
	fmt.Fprintf(os.Stderr, "killed (pid %d)\n", pid)
	return nil
}

func Restart(inventoryPath string, port int) error {
	_ = Stop()
	return Start(inventoryPath, port)
}

func Status(port int) {
	pid, err := readPid()
	if err != nil || !isRunning(pid) {
		fmt.Println("stopped")
		if err == nil {
			os.Remove(mcpPidFile())
		}
		os.Exit(1)
	}

	url := mcpURL(port)
	token := mcpToken()
	resp, err := mcpHealthCheck(url, token)
	healthy := err == nil && resp.StatusCode < 500
	if resp != nil {
		resp.Body.Close()
	}

	status := "running"
	if !healthy {
		status = "running (unhealthy)"
	}
	fmt.Printf("%s (pid %d) on %s\n", status, pid, url)
}
