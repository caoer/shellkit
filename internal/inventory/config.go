// Package inventory loads and serves the SSH server inventory: the Server
// model, address-preference resolution, identity/key path resolution, nix
// inventory loading (with OrbStack VM discovery), the sops repo-root locator,
// and a live-reloading InventoryStore.
package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Server struct {
	Provider       string `json:"provider"`
	Name           string `json:"-"`
	SSHAlias       string `json:"-"`
	IP             string `json:"wan_ip"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	Identity       string `json:"identity"`
	Location       string `json:"location"`
	State          string `json:"state"`
	Project        string `json:"project"`
	Role           string `json:"role"`
	InstanceType   string `json:"instance_type"`
	InstanceID     string `json:"instance_id"`
	PrivateIP      string `json:"lan_ip"`
	Notes          string `json:"notes"`
	Plan           string `json:"plan"`
	Account        string `json:"account"`
	Expires        string `json:"expires"`
	DC             string `json:"dc"`
	IPv6           string `json:"ipv6"`
	Region         string `json:"region"`
	Zone           string `json:"zone"`
	Type           string `json:"type"`
	TailscaleIP    string `json:"tailscale_ip"`
	WGIP           string `json:"wireguard_ip"`
	EasytierIP     string `json:"easytier_ip"`
	Owner          string `json:"owner"`
	Group          string `json:"group"`
	Orb            string `json:"orb"`
	Managed        string `json:"managed"`
	PasswordRef    string `json:"password_ref"`
	PreferNet      string `json:"prefer_net"`
	ProxyJump      string `json:"proxy_jump"`
	IdentitiesOnly bool   `json:"identities_only"`
}

func (s Server) IsOrb() bool {
	return s.Orb != ""
}

func (s Server) OrbTarget() string {
	return fmt.Sprintf("%s@%s@orb", s.DisplayUser(), s.Orb)
}

func (s Server) ConnectAddr() string {
	host := s.ResolvedIP()
	return fmt.Sprintf("%s:%d", host, s.PortFor(host))
}

func (s Server) DisplayUser() string {
	if s.User != "" {
		return s.User
	}
	return "root"
}

// HasPassword reports whether the server carries a password_ref.
func (s Server) HasPassword() bool {
	return s.PasswordRef != ""
}

type AddrPref string

const (
	AddrAuto      AddrPref = "auto"
	AddrWan       AddrPref = "wan"
	AddrLan       AddrPref = "lan"
	AddrWG        AddrPref = "wireguard"
	AddrTailscale AddrPref = "tailscale"
	AddrEasytier  AddrPref = "easytier"
)

func ParseAddrPref(s string) (AddrPref, error) {
	switch AddrPref(s) {
	case AddrAuto, AddrWan, AddrLan, AddrWG, AddrTailscale, AddrEasytier:
		return AddrPref(s), nil
	default:
		return "", fmt.Errorf("unknown address preference %q (valid: auto, wan, lan, wireguard, tailscale, easytier)", s)
	}
}

func (s Server) HostFor(pref AddrPref) (string, error) {
	switch pref {
	case AddrWan:
		if s.IP == "" {
			return "", fmt.Errorf("no wan_ip for %s", s.Name)
		}
		return s.IP, nil
	case AddrLan:
		if s.PrivateIP == "" {
			return "", fmt.Errorf("no lan_ip for %s", s.Name)
		}
		return s.PrivateIP, nil
	case AddrWG:
		if s.WGIP == "" {
			return "", fmt.Errorf("no wireguard_ip for %s", s.Name)
		}
		return s.WGIP, nil
	case AddrTailscale:
		if s.TailscaleIP == "" {
			return "", fmt.Errorf("no tailscale_ip for %s", s.Name)
		}
		return s.TailscaleIP, nil
	case AddrEasytier:
		if s.EasytierIP == "" {
			return "", fmt.Errorf("no easytier_ip for %s", s.Name)
		}
		return s.EasytierIP, nil
	case AddrAuto:
		for _, addr := range []string{s.EasytierIP, s.WGIP, s.TailscaleIP, s.PrivateIP, s.IP} {
			if addr != "" {
				return addr, nil
			}
		}
		return "", fmt.Errorf("no address available for %s", s.Name)
	default:
		return "", fmt.Errorf("unknown address preference: %s", pref)
	}
}

// ResolvedIP returns the best IP for SSH config generation.
// Uses prefer_net if set, otherwise prefers public IP, then any available.
func (s Server) ResolvedIP() string {
	if s.PreferNet != "" {
		pref, err := ParseAddrPref(s.PreferNet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shellkit: %s: invalid prefer_net %q: %v\n", s.Name, s.PreferNet, err)
		} else {
			addr, err := s.HostFor(pref)
			if err != nil {
				fmt.Fprintf(os.Stderr, "shellkit: %s: prefer_net %q but %v\n", s.Name, s.PreferNet, err)
			} else {
				return addr
			}
		}
	}
	// SSH config default: public first, then LAN — overlay networks
	// (easytier, wireguard, tailscale) require explicit prefer_net.
	for _, addr := range []string{s.IP, s.PrivateIP} {
		if addr != "" {
			return addr
		}
	}
	return ""
}

// PortFor returns the SSH port for a resolved IP address.
// Custom port applies only to the public IP (NAT forwarding); all other
// network addresses use port 22.
func (s Server) PortFor(resolvedHost string) int {
	if resolvedHost == s.IP && s.Port != 0 {
		return s.Port
	}
	return 22
}

func (s Server) ConnectAddrFor(pref AddrPref) (string, error) {
	host, err := s.HostFor(pref)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, s.PortFor(host)), nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func ResolveIdentityPath(identity string) string {
	if strings.Contains(identity, "/") {
		return expandHome(identity)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "keys", identity)
}

// FindSopsRoot walks up from start looking for the directory that owns
// .sops.yaml (the inventory repo root). Returns "" when none is found.
// Relative "sops:" password_ref paths resolve against this root.
func FindSopsRoot(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".sops.yaml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// DefaultIdentityRef is the SSH identity shellkit falls back to when a host
// specifies none, in the ~-prefixed form used inside SSH config files. Override
// with SHELLKIT_DEFAULT_IDENTITY (an absolute or ~ path). Defaults to the
// conventional ~/.ssh/id_ed25519.
func DefaultIdentityRef() string {
	if v := os.Getenv("SHELLKIT_DEFAULT_IDENTITY"); v != "" {
		return v
	}
	return "~/.ssh/id_ed25519"
}

// defaultIdentity is DefaultIdentityRef expanded to an absolute path.
func defaultIdentity() string {
	return expandHome(DefaultIdentityRef())
}

func loadDefaultIdentityFiles() []string {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".ssh", "configs.generated.conf")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return []string{defaultIdentity()}
	}
	var files []string
	inHostStar := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Host ") {
			inHostStar = trimmed == "Host *"
			continue
		}
		if inHostStar && strings.HasPrefix(trimmed, "IdentityFile ") {
			path := strings.TrimPrefix(trimmed, "IdentityFile ")
			files = append(files, expandHome(path))
		}
	}
	if len(files) == 0 {
		return []string{defaultIdentity()}
	}
	return files
}

func (s Server) KeyPaths() []string {
	var paths []string
	if s.Identity != "" {
		paths = append(paths, ResolveIdentityPath(s.Identity))
	}
	paths = append(paths, loadDefaultIdentityFiles()...)
	return paths
}

func LoadInventory(path string) ([]Server, error) {
	if strings.HasSuffix(path, ".nix") {
		return loadInventoryFromNix(path)
	}
	return nil, fmt.Errorf("unsupported inventory format: %s (expected .nix)", path)
}

func loadInventoryFromNix(nixFile string) ([]Server, error) {
	cmd := exec.Command("nix", "eval", "--json", "-f", nixFile, "hosts")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("nix eval %s: %s", nixFile, string(ee.Stderr))
		}
		return nil, fmt.Errorf("nix eval %s: %w", nixFile, err)
	}

	var raw map[string]Server
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse nix eval output: %w", err)
	}

	servers := make([]Server, 0, len(raw))
	for name, s := range raw {
		s.Name = name
		s.SSHAlias = name
		servers = append(servers, s)
	}

	existing := make(map[string]bool, len(servers))
	for _, s := range servers {
		if s.Orb != "" {
			existing[s.Orb] = true
		}
	}
	for _, orb := range discoverOrbVMs() {
		if !existing[orb.Orb] {
			servers = append(servers, orb)
		}
	}
	return servers, nil
}

func discoverOrbVMs() []Server {
	// Don't wake a stopped OrbStack engine. `orbctl list` auto-boots the
	// engine; probe only when it's already running (socket present), so host
	// resolution never has the side effect of starting OrbStack.
	if home, err := os.UserHomeDir(); err != nil {
		return nil
	} else if _, err := os.Stat(filepath.Join(home, ".orbstack/run/docker.sock")); err != nil {
		return nil
	}
	out, err := exec.Command("orbctl", "list").Output()
	if err != nil {
		return nil
	}
	var servers []Server
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		state := fields[1]
		var ip string
		for _, f := range fields[2:] {
			if strings.Count(f, ".") == 3 && f[0] >= '0' && f[0] <= '9' {
				ip = f
				break
			}
		}
		servers = append(servers, Server{
			Provider: "orbstack",
			Name:     "orb-" + name,
			Orb:      name,
			IP:       ip,
			State:    state,
			Location: "local",
		})
	}
	return servers
}
