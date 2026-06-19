// Package sshconn builds the ssh/sshpass invocations shellkit uses to reach
// hosts: argument assembly from the inventory, sops-backed password resolution,
// per-host connection rate limiting, the TCP/auth probe, and SSH config
// generation. It depends on internal/inventory and internal/ui.
package sshconn

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/caoer/shellkit/internal/inventory"
)

// addrPref is the process-wide default address preference applied by the
// implicit-pref helpers (SSHCommand/SSHCommandString and the password/key
// invocation builders). Functions that already accept a pref parameter use
// that instead. Set once at startup via SetDefaultAddrPref.
var addrPref = inventory.AddrAuto

// SetDefaultAddrPref sets the process-wide default address preference.
func SetDefaultAddrPref(p inventory.AddrPref) {
	addrPref = p
}

// SetSopsRoot sets the directory that relative "sops:" password_ref paths
// resolve against (the inventory repo root). When empty, resolution falls back
// to walking up from the current working directory.
func SetSopsRoot(root string) {
	sopsRoot = root
}

// SSHArgs assembles the ssh argument vector for srv at the given address
// preference. Orb hosts target the orb alias; AddrAuto with an alias defers to
// the ssh config; an explicit pref resolves the address and carries identity,
// port, IdentitiesOnly, and ProxyJump as flags.
func SSHArgs(s *inventory.Server, pref inventory.AddrPref) []string {
	if s.IsOrb() {
		return []string{s.OrbTarget()}
	}
	var args []string
	if pref == inventory.AddrAuto && s.SSHAlias != "" {
		return append(args, s.SSHAlias)
	}
	host, err := s.HostFor(pref)
	if err != nil {
		host = s.IP
	}
	if s.User != "" {
		args = append(args, "-l", s.User)
	}
	port := 22
	if host == s.IP && s.Port != 0 {
		port = s.Port
	}
	args = append(args, "-p", fmt.Sprintf("%d", port))
	if s.Identity != "" {
		args = append(args, "-i", inventory.ResolveIdentityPath(s.Identity))
	}
	// AddrAuto defers to the alias (ssh config supplies these); an explicit
	// address pref bypasses the config, so carry them through as flags.
	if s.IdentitiesOnly {
		args = append(args, "-o", "IdentitiesOnly=yes")
	}
	if s.ProxyJump != "" {
		args = append(args, "-J", s.ProxyJump)
	}
	args = append(args, host)
	return args
}

// SSHCommandString renders the ssh command line for srv at the default address
// preference (display/copy only).
func SSHCommandString(s *inventory.Server) string {
	return "ssh " + strings.Join(SSHArgs(s, addrPref), " ")
}

// SSHCommand builds an *exec.Cmd to ssh into srv at the default address
// preference. For password_ref hosts it returns an sshpass invocation; if
// password resolution fails it falls back to plain ssh so the user can still
// connect (ssh prompts interactively).
func SSHCommand(s *inventory.Server) *exec.Cmd {
	name, args, env, err := sshInvocation(s)
	if err != nil {
		// Password resolution failed — fall back to plain ssh so the user can
		// still connect (ssh will prompt for the password interactively).
		fmt.Fprintf(os.Stderr, "shellkit: %s: %v\n", s.Name, err)
		name, args, env = "ssh", SSHArgs(s, addrPref), nil
	}
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}
