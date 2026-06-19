# Security Policy

## Reporting a vulnerability

Please report security issues privately via GitHub's
[private vulnerability reporting](https://github.com/caoer/shellkit/security/advisories/new)
("Report a vulnerability" under the repository's **Security** tab). Do not open a
public issue for security problems.

We aim to acknowledge reports within a few days and will keep you updated on the
fix and disclosure timeline.

## Scope and what to keep in mind

shellkit executes commands on remote hosts and handles sensitive material. When
assessing impact, note that shellkit:

- shells out to the system `ssh`, `sshpass`, `sops`, and `tmux` binaries;
- resolves passwords from `sops`-encrypted files (`password_ref`);
- runs an MCP server that, over HTTP, is guarded only by the bearer token in
  `SHELLKIT_MCP_TOKEN` — run it on a trusted network or behind a proxy;
- performs **literal** (not shell-escaped) template substitution in the `ssh`
  DSL — callers are responsible for quoting interpolated values.

The generated SSH config sets `StrictHostKeyChecking no` and
`UserKnownHostsFile /dev/null` by design for ephemeral managed fleets; this
trades host-key verification for convenience and may not suit your threat model.
