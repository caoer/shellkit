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
	Server    *inventory.Server
	Status    ProbeStatus
	Latency   time.Duration
	KeyUsed   string
	ExtraKeys []string // keys that also authenticate (--extra-keys mode)
	Error     string
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

// sshDialWithDeadline dials TCP, sets a hard deadline covering the entire
// SSH handshake + auth (not just the TCP connect), then returns an
// *ssh.Client. This prevents hangs when a server accepts TCP but stalls
// the handshake or auth exchange.
func sshDialWithDeadline(addr string, config *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	// Hard deadline: if handshake + auth aren't done in time, the
	// connection is forcibly closed.
	conn.SetDeadline(time.Now().Add(timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Clear deadline for normal operation after successful auth.
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

type namedSigner struct {
	ssh.Signer
	label string
}

// ProbeOptions controls optional probe behaviour.
type ProbeOptions struct {
	// DisableDefaultKey skips the default identity fallback. Hosts with no
	// explicit identity and no password report no-key (StatusAuthFail)
	// without attempting SSH auth.
	DisableDefaultKey bool
	// DisablePassword skips password auth for password_ref hosts, reporting
	// auth-fail without attempting to resolve or use the password.
	DisablePassword bool
}

func ProbeServer(s *inventory.Server, timeout time.Duration, opts ...ProbeOptions) ProbeResult {
	var opt ProbeOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

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

	// Dual-auth: hosts with both identity and password_ref try key auth first,
	// falling back to password if the key fails. Password-only hosts (no
	// explicit identity) go straight to password auth.
	if s.HasPassword() {
		if opt.DisablePassword {
			result.Status = StatusAuthFail
			result.KeyUsed = "(password)"
			result.Error = "password auth disabled"
			return result
		}
		if s.Identity != "" {
			// Try key auth first; fall through to password on failure.
			keyResult := probeKey(s, addr, result, timeout)
			if keyResult.Status == StatusAuthOK {
				return keyResult
			}
		}
		return probePassword(s, addr, result, timeout)
	}

	// When --disable-default-key is set, hosts with no explicit identity
	// skip the probe entirely — no key to try means no-key, not "try default".
	if opt.DisableDefaultKey && s.Identity == "" {
		result.Status = StatusAuthFail
		result.Error = "no explicit identity and default key disabled"
		return result
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
	}

	client, err := sshDialWithDeadline(addr, config, timeout)
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

// probeKey attempts publickey auth for a host that also carries a password_ref.
// Returns auth-ok when the key works; on failure the caller falls back to
// password auth.
func probeKey(s *inventory.Server, addr string, result ProbeResult, timeout time.Duration) ProbeResult {
	keyPaths := s.KeyPaths()
	fileSigners := loadSigners(keyPaths)

	var named []namedSigner
	for i, signer := range fileSigners {
		named = append(named, namedSigner{signer, keyPaths[i]})
	}
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
		if len(named) == 0 {
			for _, signer := range agents {
				named = append(named, namedSigner{signer, "(agent)"})
			}
		}
	}
	if len(named) == 0 {
		result.Status = StatusAuthFail
		return result
	}

	allSigners := make([]ssh.Signer, len(named))
	for i := range named {
		allSigners[i] = named[i].Signer
	}
	if len(named) == 1 {
		result.KeyUsed = named[0].label
	} else {
		result.KeyUsed = "(multi-key)"
	}

	config := &ssh.ClientConfig{
		User:            s.DisplayUser(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(allSigners...)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := sshDialWithDeadline(addr, config, timeout)
	if err != nil {
		result.Status = StatusAuthFail
		result.Error = err.Error()
		return result
	}
	result.Status = StatusAuthOK
	rttStart := time.Now()
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
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
	}

	client, err := sshDialWithDeadline(addr, config, timeout)
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

// ProbeExtraKeys tries each key path individually against a host,
// returning paths of keys that successfully authenticate.
// Handles encrypted keys by matching .pub against the SSH agent.
func ProbeExtraKeys(s *inventory.Server, keyPaths []string, timeout time.Duration) []string {
	addr := s.ConnectAddr()
	agents := agentSigners()

	// Skip keys that match the host's configured identity
	configured := make(map[string]bool)
	for _, cp := range s.KeyPaths() {
		configured[cp] = true
	}

	var succeeded []string
	for _, p := range keyPaths {
		if configured[p] {
			continue
		}
		signer := resolveKeyOrAgent(p, agents)
		if signer == nil {
			continue
		}
		config := &ssh.ClientConfig{
			User:            s.DisplayUser(),
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}
		client, err := sshDialWithDeadline(addr, config, timeout)
		if err != nil {
			continue
		}
		client.Close()
		succeeded = append(succeeded, p)
	}
	return succeeded
}

// resolveKeyOrAgent loads a private key from disk, or if encrypted,
// matches its .pub against agent signers to find the corresponding signer.
func resolveKeyOrAgent(path string, agents []ssh.Signer) ssh.Signer {
	data, err := os.ReadFile(path)
	if err == nil {
		if signer, err := ssh.ParsePrivateKey(data); err == nil {
			return signer
		}
	}
	// Encrypted or unreadable — try .pub match against agent
	pub := loadPublicKey([]string{path})
	if pub == nil {
		return nil
	}
	pubBytes := pub.Marshal()
	for _, signer := range agents {
		if string(signer.PublicKey().Marshal()) == string(pubBytes) {
			return signer
		}
	}
	return nil
}

type ProbeCallback func(result ProbeResult, done int, total int)

func ProbeAll(servers []inventory.Server, concurrency int, timeout time.Duration, opts ProbeOptions, cb ProbeCallback) []ProbeResult {
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

			r := ProbeServer(&servers[idx], timeout, opts)
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
