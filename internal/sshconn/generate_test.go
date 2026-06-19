package sshconn

import (
	"bytes"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/inventory"
)

// A host with proxy_jump set must emit a ProxyJump line; a host without it
// must not. ProxyJump is how a ProxyJump-only host (no direct/mesh path,
// reachable only through a bastion) gets a working generated SSH config.
func TestGenerateSSHConfig_ProxyJump(t *testing.T) {
	servers := []inventory.Server{
		{
			Provider:       "example",
			Name:           "robot-router",
			SSHAlias:       "robot-router",
			PrivateIP:      "192.168.90.1", // reachable only from the jump host
			User:           "admin",
			ProxyJump:      "bastion-1",
			IdentitiesOnly: true,
		},
		{
			Provider: "example",
			Name:     "direct-host",
			SSHAlias: "direct-host",
			IP:       "203.0.113.7",
			User:     "root",
		},
	}

	var buf bytes.Buffer
	GenerateSSHConfig(servers, &buf)
	out := buf.String()

	// The jump host's block carries ProxyJump and IdentitiesOnly.
	router := hostBlock(out, "robot-router")
	if !strings.Contains(router, "ProxyJump bastion-1") {
		t.Errorf("robot-router block missing ProxyJump:\n%s", router)
	}
	if !strings.Contains(router, "IdentitiesOnly yes") {
		t.Errorf("robot-router block missing IdentitiesOnly:\n%s", router)
	}

	// A plain host must not gain a ProxyJump or IdentitiesOnly line.
	direct := hostBlock(out, "direct-host")
	if strings.Contains(direct, "ProxyJump") {
		t.Errorf("direct-host block should not contain ProxyJump:\n%s", direct)
	}
	if strings.Contains(direct, "IdentitiesOnly") {
		t.Errorf("direct-host block should not contain IdentitiesOnly:\n%s", direct)
	}
}

// hostBlock returns the generated lines belonging to `Host <alias>` up to the
// next blank line.
func hostBlock(config, alias string) string {
	lines := strings.Split(config, "\n")
	var block []string
	in := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "Host "+alias {
			in = true
			block = append(block, ln)
			continue
		}
		if in {
			if strings.TrimSpace(ln) == "" {
				break
			}
			block = append(block, ln)
		}
	}
	return strings.Join(block, "\n")
}
