---
name: home-wiki-layout
description: Where sshkit/shellkit/ssh-tracing content lives in home-wiki, hub pages, and source-page frontmatter conventions
metadata:
  type: reference
---

Wiki root: `/Users/Shared/projects/home-wiki` (Obsidian vault, contract v3, `md check` lint).

## sshkit / shellkit content map

- **Primary domain page:** `domains/systems/infra/sshkit-mcp.md` (tags `[domain/infra, domain/tooling, type/reference]`). Hub for the MCP server. Has `## Command Trace on Timeout` section = the trap-DEBUG mechanism. Also `## Key Design Decisions` (entrypoint:, stdin `bash -s`), `## Code` file table (`mcp_exec.go` etc.), `## MCP HTTP Idle Timeout`, `## Live Output Streaming`.
- Sibling infra reference pages: `sshkit-resolution.md`, `sshkit-dashboard.md`, `sshkit-tmux-runtime.md` (all `domain/infra` + `topic/sshkit`).
- **Domain index (home):** `domains/systems/infra/INFRA.md` (`type/domain-index`, aliases `[infra, hosts]`). Lists sshkit-mcp under a related bullet. NOTE: INFRA is a fleet-inventory domain; sshkit tooling lives here somewhat by convenience — there is no dedicated shellkit/tooling domain.
- `domains/systems/osfiles/dev-tooling.md` (osfiles cluster) covers the shellkit OSS-prep format/lint/hook layer.

## Compound sources (`sources/compound/`)

trace/DSL history lives in: `2026-04-25-sshkit-mcp-dsl.md` (original trap-DEBUG trace design), `2026-05-28-sshkit-live-output.md` (trace knob wiring + per-line liveness overrule), `2026-04-26-sshkit-mcp-test-run.md` + `2026-05-12-sshkit-mcp-full-test-run.md` (116-test validation), `2026-07-07-shellkit-oss-extraction.md` + `2026-07-07-shellkit-scaffold.md` (sshkit→shellkit repo split), `2026-07-07-sshkit-test-burst-perf.md`.

## Source-page frontmatter convention (compound bucket)

```yaml
---
tags: [type/source, domain/<x>, topic/sshkit]
source: wiki://home-wiki-sessions/year=2026/month=06/15-06-shellkit
compound-type: knowledge
created: 2026-07-07
origin: wiki://home-wiki-sessions/year=2026/month=06/15-06-shellkit
lint-ignore: [source-resolves]
---
```
Sources are IMMUTABLE once written. `topic/sshkit` is the live topic tag for this cluster. `domain/` on shellkit sources has been either `domain/osfiles` or `domain/locus` (not a dedicated shellkit domain).
