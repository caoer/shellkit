---
name: shellkit-runner-gap
description: The shellkit-runner (mvdan.cc/sh interp, ndjson, gap-screener, runnerDefaultOn) topic had ZERO home-wiki coverage as of 2026-07-17
metadata:
  type: project
---

As of 2026-07-17, the home-wiki has NO page mentioning: `shellkit-runner`, `mvdan`, `mvdan.cc/sh`, `interp`, `ndjson` (protocol context), `runnerDefaultOn`, `ns-precision`, `gap-screener`, or `bash-differential` routing. Confirmed via full-tree grep (incl. inbox + logs).

**Why:** ZT is building a new `shellkit-runner` static Go binary (embeds mvdan.cc/sh/v3 interp, ndjson over ssh exec channel, per-command ns-precision tracing, gap-screener bash-differential routing, in-band binary bootstrap, opt-in `runnerDefaultOn=false`) that supersedes the trap-DEBUG remote tracing documented in `domains/systems/infra/sshkit-mcp.md` § Command Trace on Timeout.

**How to apply:** When this work compounds, the runner path is a NEW mechanism that supersedes the trap-DEBUG section on `sshkit-mcp.md`. The old trap mechanism should be marked as the legacy/bash-default path, not deleted (it still runs when `runnerDefaultOn=false`). Candidate home: a new leaf page under `domains/systems/infra/` (topic/sshkit) or possibly a dedicated shellkit domain if the runner + repo split earns ≥3 pages. See [[home-wiki-layout]].
