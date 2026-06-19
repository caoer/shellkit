package sshconn

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

type ProbeStatus int

const (
	StatusPending ProbeStatus = iota
	StatusUnreachable
	StatusSSHOK
	StatusAuthOK
	StatusAuthFail
	StatusStopped
)

func (s ProbeStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusUnreachable:
		return "down"
	case StatusSSHOK:
		return "ssh-ok"
	case StatusAuthOK:
		return "auth-ok"
	case StatusAuthFail:
		return "auth-fail"
	case StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func (s ProbeStatus) Symbol() string {
	switch s {
	case StatusPending:
		return "..."
	case StatusUnreachable:
		return "x"
	case StatusSSHOK:
		return "~"
	case StatusAuthOK:
		return "OK"
	case StatusAuthFail:
		return "key"
	case StatusStopped:
		return "off"
	default:
		return "?"
	}
}

type ProbeResult struct {
	Server  *inventory.Server
	Status  ProbeStatus
	Latency time.Duration
	KeyUsed string
	Error   string
}

// loadPublicKey tries <path>.pub for each path and returns the first valid
// public key. Used to match encrypted private keys against agent signers.
func loadPublicKey(paths []string) ssh.PublicKey {
	for _, p := range paths {
		data, err := os.ReadFile(p + ".pub")
		if err != nil {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		return pub
	}
	return nil
}

func loadSigners(paths []string) []ssh.Signer {
	var signers []ssh.Signer
	seen := make(map[string]bool)
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		signers = append(signers, signer)
	}
	return signers
}

func agentSigners() []ssh.Signer {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	signers, err := sshagent.NewClient(conn).Signers()
	if err != nil {
		conn.Close()
		return nil
	}
	// conn intentionally not closed — signers reference the agent connection
	return signers
}

type namedSigner struct {
	ssh.Signer
	label string
}

func ProbeServer(s *inventory.Server, timeout time.Duration) ProbeResult {
	result := ProbeResult{Server: s, Status: StatusPending}

	if s.ResolvedIP() == "" || s.State == "stopped" {
		result.Status = StatusStopped
		return result
	}

	addr := s.ConnectAddr()
	start := time.Now()

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		result.Status = StatusUnreachable
		result.Error = err.Error()
		result.Latency = time.Since(start)
		return result
	}
	result.Latency = time.Since(start)
	conn.Close()

	// Password hosts authenticate by password only — mirror the exec path
	// (which forces PubkeyAuthentication=no) so probe validates exactly what a
	// real connection will do. Offering keys here would let a stray key report
	// auth-ok while password-only exec fails.
	if s.HasPassword() {
		return probePassword(s, addr, result, timeout)
	}

	keyPaths := s.KeyPaths()
	fileSigners := loadSigners(keyPaths)

	var named []namedSigner
	for i, signer := range fileSigners {
		named = append(named, namedSigner{signer, keyPaths[i]})
	}

	// Mirror IdentitiesOnly: when file keys load (unencrypted), skip agent.
	// When file keys are encrypted (ParsePrivateKey fails), match the .pub
	// fingerprint against agent keys to identify the correct one.
	if len(named) == 0 {
		agents := agentSigners()
		if pub := loadPublicKey(keyPaths); pub != nil {
			pubBytes := pub.Marshal()
			for _, signer := range agents {
				if string(signer.PublicKey().Marshal()) == string(pubBytes) {
					named = append(named, namedSigner{signer, keyPaths[0]})
					break
				}
			}
		}
		// No .pub match — fall back to all agent keys
		if len(named) == 0 {
			for _, signer := range agents {
				named = append(named, namedSigner{signer, "(agent)"})
			}
		}
	}

	if len(named) == 0 {
		result.Status = StatusSSHOK
		return result
	}

	allSigners := make([]ssh.Signer, len(named))
	for i := range named {
		allSigners[i] = named[i].Signer
	}

	// Record which key(s) were tried before dialing — on auth-fail we still
	// want to know what was offered so the operator can diagnose mismatches.
	if len(named) == 1 {
		result.KeyUsed = named[0].label
	} else {
		result.KeyUsed = "(multi-key)"
	}

	config := &ssh.ClientConfig{
		User:            s.DisplayUser(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(allSigners...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		result.Status = StatusAuthFail
		result.Error = err.Error()
		return result
	}

	result.Status = StatusAuthOK

	rttStart := time.Now()
	_, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
	if err == nil {
		result.Latency = time.Since(rttStart)
	}
	client.Close()

	return result
}

// probePassword validates password-only auth for a password_ref host, matching
// the exec path's PubkeyAuthentication=no behaviour.
func probePassword(s *inventory.Server, addr string, result ProbeResult, timeout time.Duration) ProbeResult {
	pw, err := resolvePassword(s)
	if err != nil {
		result.Status = StatusAuthFail
		result.Error = err.Error()
		return result
	}

	config := &ssh.ClientConfig{
		User:            s.DisplayUser(),
		Auth:            []ssh.AuthMethod{ssh.Password(pw)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		result.Status = StatusAuthFail
		result.Error = err.Error()
		return result
	}

	result.Status = StatusAuthOK
	result.KeyUsed = "(password)"

	rttStart := time.Now()
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
		result.Latency = time.Since(rttStart)
	}
	client.Close()

	return result
}

type ProbeCallback func(result ProbeResult, done int, total int)

func ProbeAll(servers []inventory.Server, concurrency int, timeout time.Duration, cb ProbeCallback) []ProbeResult {
	results := make([]ProbeResult, len(servers))
	var mu sync.Mutex
	done := 0

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := range servers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := ProbeServer(&servers[idx], timeout)
			mu.Lock()
			results[idx] = r
			done++
			current := done
			mu.Unlock()

			if cb != nil {
				cb(r, current, len(servers))
			}
		}(i)
	}

	wg.Wait()
	return results
}

func FormatLatency(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	ms := d.Milliseconds()
	if ms == 0 {
		return "<1ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
