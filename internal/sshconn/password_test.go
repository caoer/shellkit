package sshconn

import (
	"context"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/inventory"
)

func TestResolvePasswordRefErrors(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"no scheme", "secrets/common.yaml#key", "want scheme:"},
		{"unknown scheme", "vault:foo#bar", "unsupported password_ref scheme"},
		{"sops missing key", "sops:secrets/common.yaml", "want file#key"},
		{"sops empty key", "sops:secrets/common.yaml#", "want file#key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePasswordRef(tc.ref)
			if err == nil {
				t.Fatalf("expected error for %q", tc.ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestResolveSopsPath(t *testing.T) {
	defer func() { sopsRoot = "" }()

	sopsRoot = "/tmp/osfiles"
	if got := resolveSopsPath("secrets/common.yaml"); got != "/tmp/osfiles/secrets/common.yaml" {
		t.Fatalf("got %q", got)
	}

	if got := resolveSopsPath("/abs/secrets.yaml"); got != "/abs/secrets.yaml" {
		t.Fatalf("absolute path should pass through, got %q", got)
	}
}

func TestSSHInvocationNoPassword(t *testing.T) {
	srv := &inventory.Server{Name: "plain", SSHAlias: "plain"}
	name, args, env, err := sshInvocation(srv, "bash", "-s")
	if err != nil {
		t.Fatal(err)
	}
	if name != "ssh" {
		t.Fatalf("name = %q, want ssh", name)
	}
	if env != nil {
		t.Fatalf("env should be nil for keyed host, got %v", env)
	}
	if args[len(args)-2] != "bash" || args[len(args)-1] != "-s" {
		t.Fatalf("remoteCmd not appended: %v", args)
	}
}

func TestSSHKeyInvocationForcesPublickey(t *testing.T) {
	srv := &inventory.Server{Name: "robot", SSHAlias: "robot"}
	name, args := sshKeyInvocation(srv, "bash", "-s")
	if name != "ssh" {
		t.Fatalf("name = %q, want ssh", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "PreferredAuthentications=publickey") {
		t.Fatalf("missing publickey auth opt: %v", args)
	}
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Fatalf("missing BatchMode opt: %v", args)
	}
	if args[len(args)-2] != "bash" || args[len(args)-1] != "-s" {
		t.Fatalf("remoteCmd not appended: %v", args)
	}
}

// resolveInvocation must leave non-password hosts on the plain ssh path — no
// forced auth opts, no probe — so agent/config behavior is unchanged.
func TestResolveInvocationNoPasswordIsPlain(t *testing.T) {
	srv := &inventory.Server{Name: "plain", SSHAlias: "plain"}
	name, args, env, err := ResolveInvocation(context.Background(), srv, "bash", "-s")
	if err != nil {
		t.Fatal(err)
	}
	if name != "ssh" || env != nil {
		t.Fatalf("want plain ssh with nil env, got name=%q env=%v", name, env)
	}
	if strings.Contains(strings.Join(args, " "), "PreferredAuthentications") {
		t.Fatalf("plain host should not force auth opts: %v", args)
	}
}

func TestSSHInvocationPassword(t *testing.T) {
	// Drive resolution through the cache so the test needs no sops/age setup.
	const ref = "sops:test#fixture"
	pwCacheMu.Lock()
	pwCache[ref] = "s3cret"
	pwCacheMu.Unlock()
	defer func() {
		pwCacheMu.Lock()
		delete(pwCache, ref)
		pwCacheMu.Unlock()
	}()

	srv := &inventory.Server{Name: "robot", SSHAlias: "robot", PasswordRef: ref}
	name, args, env, err := sshInvocation(srv, "bash", "-s")
	if err != nil {
		t.Fatal(err)
	}
	if name != "sshpass" {
		t.Fatalf("name = %q, want sshpass", name)
	}
	if len(env) != 1 || env[0] != "SSHPASS=s3cret" {
		t.Fatalf("env = %v, want [SSHPASS=s3cret]", env)
	}
	if args[0] != "-e" || args[1] != "ssh" {
		t.Fatalf("args should start with -e ssh, got %v", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "PreferredAuthentications=password") {
		t.Fatalf("missing password auth opt: %v", args)
	}
	if args[len(args)-2] != "bash" || args[len(args)-1] != "-s" {
		t.Fatalf("remoteCmd not appended: %v", args)
	}
}
