---
---
# Address & port resolution — which IP:port a named host connects on

The 3-layer **name → host** lookup (inventory → raw target → ssh_config) is in SKILL.md. This page covers the next step: once a host is named from inventory, how shellkit picks the concrete **address** and **port**. Only inventory hosts reach this logic — raw targets and ssh_config aliases use the address/port you typed.

## The port follows the resolved address

A host's custom `port` is the **NAT forward on the hypervisor's public IP**, so it applies **only when the resolved address is that public IP**. Any mesh/LAN address (easytier / wireguard / tailscale / private) uses port **22**.

```
public wan_ip  → custom NAT port (e.g. 4218)
easytier/wg/ts/private/lan → 22
```

This matters on shared hypervisors: 15+ VMs can share one public IP via per-VM NAT ports. Pairing a custom port with a mesh address would be right-port-wrong-address — the rule prevents it.

## `--addr` preference

Request a specific network explicitly with `--addr`: `auto | wan | lan | wireguard | tailscale | easytier`. `auto` uses the default fallback order (below). The preference only works for inventory hosts — there's metadata to route on.

## Two default orders — intentional, not drift

| Context | Order | Why |
|---|---|---|
| **Runtime connect** (`auto`) | easytier → wireguard → tailscale → private → public | **VPN-first** — ride the mesh; public IP is last resort |
| **Config generation** (`~/.ssh/config`) | `prefer_net` → public → easytier → private → wireguard → tailscale | **public-first** — a generated config should default to the stable public address |

Same mechanism underneath (one port rule, one ordered slice per path); the order differs on purpose.

## `prefer_net` override

The inventory `prefer_net` field forces config-gen resolution to a specific network. An invalid value logs to stderr and falls back to the public-first order.

## Address fields the resolver reads

Inventory hosts carry these address fields; the fallback orders and `--addr` select among them:

| Field | Network |
|---|---|
| `wan_ip` | public internet |
| `lan_ip` | Proxmox LAN |
| `wireguard_ip` | WireGuard mesh |
| `tailscale_ip` | Tailscale / Headscale |
| `easytier_ip` | EasyTier mesh |

> The daemon **live-reloads** inventory (fsnotify watch on `lib/ssh/hosts/`), so registry edits are picked up automatically — no restart needed. A host resolves as `unknown host` only when it isn't in the registry at all (and isn't a raw target / ssh_config alias).
