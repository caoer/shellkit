# Sample shellkit inventory.
#
# shellkit loads hosts by running `nix eval --json -f <this-file> hosts`, so the
# file must expose a top-level `hosts` attribute set: { <name> = { ...fields }; }.
# Each host's fields map 1:1 to the JSON keys shellkit understands (see below).
#
# Point shellkit at this file with either:
#   shellkit -f ./examples/inventory.sample.nix list
#   SHELLKIT_INVENTORY=./examples/inventory.sample.nix shellkit list
#
# All addresses below use the IETF documentation ranges (RFC 5737 / RFC 3849)
# and RFC 1918 private space — replace them with your own.
{
  hosts = {
    # Minimal public host: a name, a WAN address, and a login user.
    web-1 = {
      provider = "example-cloud";
      wan_ip = "203.0.113.10"; # public address (RFC 5737 TEST-NET-3)
      port = 22; # custom WAN port (NAT forward); other nets always use 22
      user = "deploy";
      identity = "id_ed25519"; # ~/.ssh/keys/<name>, or an absolute/~ path
      location = "us-east";
      role = "web";
      managed = "example"; # filter with: shellkit --managed example list
    };

    # Private-only host reached through a bastion via proxy_jump.
    db-1 = {
      provider = "example-cloud";
      lan_ip = "10.0.0.20"; # private address (RFC 1918)
      user = "admin";
      proxy_jump = "web-1"; # ssh -J web-1
      role = "db";
      prefer_net = "lan"; # auto|wan|lan|wireguard|tailscale|easytier
      managed = "example";
    };

    # Mesh host advertising multiple overlay addresses. shellkit's --addr flag
    # (auto by default) selects which one to connect on.
    edge-1 = {
      provider = "example-bare-metal";
      wan_ip = "198.51.100.30"; # RFC 5737 TEST-NET-2
      lan_ip = "10.0.0.30";
      wireguard_ip = "10.99.0.30";
      tailscale_ip = "100.64.0.30"; # RFC 6598 CGNAT (Tailscale range)
      ipv6 = "2001:db8::30"; # RFC 3849 documentation IPv6
      user = "root";
      role = "edge";
      managed = "example";
      # password_ref = "sops:secrets/common.yaml#edge-1_password";
      #   ^ optional: resolve the login password from a sops-encrypted file
      #     (relative paths resolve against the directory owning .sops.yaml).
    };
  };
}
