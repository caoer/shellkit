package sshconn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
)

// sopsRoot is the directory that relative "sops:" password_ref paths resolve
// against (the inventory repo root). Set once at startup via SetSopsRoot. When
// empty, resolution falls back to walking up from the current working directory.
var sopsRoot string

// passwordSSHOpts force the system ssh binary down the password path when
// driven by sshpass: no key offering (avoids "too many authentication
// failures" when an agent holds many keys), no host-key prompt (sshpass cannot
// answer it), and exactly one password attempt. Mirrors the generated config's
// StrictHostKeyChecking=no convention.
var passwordSSHOpts = []string{
	"-o", "PreferredAuthentications=password",
	"-o", "PubkeyAuthentication=no",
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "NumberOfPasswordPrompts=1",
}

var (
	pwCache   = map[string]string{}
	pwCacheMu sync.Mutex
)

// resolvePassword decrypts the server's password_ref to plaintext.
func resolvePassword(s *inventory.Server) (string, error) {
	if s.PasswordRef == "" {
		return "", fmt.Errorf("%s: no password_ref", s.Name)
	}
	pw, err := resolvePasswordRef(s.PasswordRef)
	if err != nil {
		return "", fmt.Errorf("%s: %w", s.Name, err)
	}
	return pw, nil
}

// resolvePasswordRef resolves a "scheme:..." reference, caching by ref string.
// Only the "sops:" scheme is supported today.
func resolvePasswordRef(ref string) (string, error) {
	pwCacheMu.Lock()
	if v, ok := pwCache[ref]; ok {
		pwCacheMu.Unlock()
		return v, nil
	}
	pwCacheMu.Unlock()

	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("invalid password_ref %q (want scheme:...)", ref)
	}

	var pw string
	var err error
	switch scheme {
	case "sops":
		pw, err = resolveSopsRef(rest)
	default:
		return "", fmt.Errorf("unsupported password_ref scheme %q (only sops: supported)", scheme)
	}
	if err != nil {
		return "", err
	}

	pwCacheMu.Lock()
	pwCache[ref] = pw
	pwCacheMu.Unlock()
	return pw, nil
}

// resolveSopsRef handles "<file>#<key>": decrypt <file> with sops and extract
// the scalar at <key> (a flat top-level key, matching the common sops convention).
func resolveSopsRef(rest string) (string, error) {
	file, key, ok := strings.Cut(rest, "#")
	if !ok || file == "" || key == "" {
		return "", fmt.Errorf("invalid sops ref %q (want file#key)", rest)
	}
	path := resolveSopsPath(file)
	out, err := exec.Command("sops", "decrypt", "--extract", fmt.Sprintf("[%q]", key), path).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("sops decrypt %s#%s: %s", file, key, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("sops decrypt %s#%s: %w", file, key, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// resolveSopsPath turns a ref-relative sops file path into an absolute one.
// Absolute paths pass through. Relative paths join sopsRoot when set, else are
// located by walking up from the current working directory.
func resolveSopsPath(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	if sopsRoot != "" {
		return filepath.Join(sopsRoot, file)
	}
	dir, _ := os.Getwd()
	for {
		cand := filepath.Join(dir, file)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return file
}

// keySSHOpts force a publickey-only attempt: never prompt for a password
// (BatchMode), and offer keys only. Used for the probe and the key-success
// branch of resolveInvocation so a key-capable host never gets stuck on the
// password path.
var keySSHOpts = []string{
	"-o", "PreferredAuthentications=publickey",
	"-o", "BatchMode=yes",
}

// sshKeyInvocation builds a plain ssh command forced to publickey auth.
func sshKeyInvocation(srv *inventory.Server, remoteCmd ...string) (string, []string) {
	args := append([]string{}, keySSHOpts...)
	args = append(args, SSHArgs(srv, addrPref)...)
	args = append(args, remoteCmd...)
	return "ssh", args
}

// sshPasswordInvocation builds the sshpass password-only command, decrypting
// the server's password_ref into SSHPASS.
func sshPasswordInvocation(srv *inventory.Server, remoteCmd ...string) (name string, args []string, env []string, err error) {
	pw, perr := resolvePassword(srv)
	if perr != nil {
		return "", nil, nil, perr
	}
	args = append([]string{"-e", "ssh"}, passwordSSHOpts...)
	args = append(args, SSHArgs(srv, addrPref)...)
	args = append(args, remoteCmd...)
	return "sshpass", args, []string{"SSHPASS=" + pw}, nil
}

// sshInvocation returns the executable, args, and extra env to run ssh against
// srv. For password_ref hosts it returns an sshpass invocation carrying the
// decrypted password in SSHPASS; otherwise a plain ssh invocation. remoteCmd is
// appended after the ssh target (empty for an interactive shell). Callers that
// can probe (non-interactive exec) should prefer resolveInvocation, which tries
// key auth before falling back to the password.
func sshInvocation(srv *inventory.Server, remoteCmd ...string) (name string, args []string, env []string, err error) {
	if srv.HasPassword() {
		return sshPasswordInvocation(srv, remoteCmd...)
	}
	base := SSHArgs(srv, addrPref)
	return "ssh", append(base, remoteCmd...), nil, nil
}

var (
	keyAuthCacheMu  sync.Mutex
	keyAuthCacheMap = make(map[string]keyAuthCacheEntry)
)

type keyAuthCacheEntry struct {
	works     bool
	checkedAt time.Time
}

const keyAuthCacheTTL = 5 * time.Minute

// keyAuthWorks runs a cheap publickey-only auth check against srv. Returns true
// when the host accepts key auth, so password_ref hosts that also carry a
// deployed key prefer the key instead of being forced onto the password path.
// A network failure or key rejection returns false; the caller then falls back
// to the password invocation.
//
// Results are cached per host address for keyAuthCacheTTL to avoid redundant
// SSH connections across sequential steps targeting the same host.
func keyAuthWorks(ctx context.Context, srv *inventory.Server) bool {
	cacheKey := SSHRateLimitKey(srv)

	keyAuthCacheMu.Lock()
	if entry, ok := keyAuthCacheMap[cacheKey]; ok && time.Since(entry.checkedAt) < keyAuthCacheTTL {
		keyAuthCacheMu.Unlock()
		return entry.works
	}
	keyAuthCacheMu.Unlock()

	// Rate-limit the probe connection like any other SSH attempt.
	if err := SSHRateLimit.Acquire(ctx, cacheKey); err != nil {
		return false
	}

	args := append([]string{}, keySSHOpts...)
	args = append(args, "-o", "ConnectTimeout=8")
	args = append(args, SSHArgs(srv, addrPref)...)
	args = append(args, "true")
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	works := exec.CommandContext(pctx, "ssh", args...).Run() == nil

	keyAuthCacheMu.Lock()
	keyAuthCacheMap[cacheKey] = keyAuthCacheEntry{works: works, checkedAt: time.Now()}
	keyAuthCacheMu.Unlock()

	return works
}

// ResolveInvocation picks the ssh invocation for a non-interactive exec.
// Hosts without a password_ref use a plain ssh command (system config / agent
// keys), unchanged. Hosts with a password_ref are key-first: a publickey probe
// decides whether to run key auth or fall back to sshpass password auth, so a
// stray password_ref on a key-auth host no longer blocks the key.
func ResolveInvocation(ctx context.Context, srv *inventory.Server, remoteCmd ...string) (name string, args []string, env []string, err error) {
	if !srv.HasPassword() {
		base := SSHArgs(srv, addrPref)
		return "ssh", append(base, remoteCmd...), nil, nil
	}
	if keyAuthWorks(ctx, srv) {
		n, a := sshKeyInvocation(srv, remoteCmd...)
		return n, a, nil, nil
	}
	return sshPasswordInvocation(srv, remoteCmd...)
}
