---
---
# Runner execution: bootstrap, diagnostics, known limitations

Deep-dive for the mvdan/sh runner introduced alongside the `"interp"` DSL flag — see SKILL.md
"Two execution paths" for the flag itself and when a step falls back. This file covers what
happens under the hood the first time a host runs an `"interp": true` step, how to read the
failure diagnostics, and the two divergence classes that stay documented rather than silently
shipped.

## Bootstrap: push, cache, verify

The first `"interp": true` step against a host pushes the runner binary; every step after reuses
it. The push rides the exact same ssh exec channel the tool already uses for everything else — no
sftp subsystem, no extra auth round-trip.

- **Cache location** — an ordered candidate chain (Mitogen pattern): the inventory's per-host
  `runner_tmp` override (if set on that host) → `~/.cache/shellkit` → `$XDG_CACHE_HOME` →
  `/var/tmp` → `/dev/shm`. Each candidate is write+chmod+exec smoke-tested *before* anything
  commits to it — this is how a `noexec` mount or a read-only home gets detected up front instead
  of causing a confusing mid-push failure. The winning candidate is cached per host so later steps
  skip the probe.
- **Cold push** — decompress the embedded binary daemon-side, `cat` the raw bytes in-band to a
  hidden dotname temp file, `chmod +x`, then atomic `mv -f` to a hash-named immutable final path
  (concurrent pushes to the same host are safe by construction — same content hashes to the same
  final path). The daemon then verifies the pushed bytes' own sha256 against what the remote side
  reports, and runs one exec smoke-test (`runner --version`) before treating the host as bootstrapped.
- **Warm** — the cached binary at that host already answers `--version` with the expected content
  hash; no push happens. The live progress ticker still shows `phase:bootstrap` briefly on a warm
  run — that phase also spans the ssh connect + protocol handshake, not just a push, so seeing it
  on a warm step is not a re-push and not a bug.
- **Version/digest mismatch** — if the remote binary's version or sha256 doesn't match what the
  daemon expects (stale cached binary from a previous build, or a protocol version skew), that's a
  *distinct* logged diagnostic — not folded into a generic "push failed" message — followed by one
  re-push attempt. Still mismatched after that: fall back to legacy for the step, never a silent
  partial state.
- **noexec / read-only home** — if all four candidates fail the write+chmod+exec probe, the step
  falls back to legacy with one explicit diagnostic naming noexec/ro-home as the cause:
  ```
  note: runner bootstrap failed on host-b (no writable+executable cache dir: ~/.cache,
  $XDG_CACHE_HOME, /var/tmp, /dev/shm all unusable — noexec or read-only home) — ran under legacy path
  ```
  Bootstrap never step-fails outright over this — it always yields a usable fallback verdict.
- **darwin/arm64 signing** — the embedded darwin/arm64 binary is ad-hoc signed at build time
  (`codesign -s -`, no Apple developer account required), because an unsigned Apple-Silicon binary
  gets SIGKILLed by AMFI on its very first exec attempt — a silent, confusing bootstrap failure if
  it slipped through unsigned. If a signature issue ever does slip through, the exec smoke-test
  fails and the step falls back with a diagnostic naming the cause, rather than looking like a
  plain stall.

## Known limitations — residual silent-divergence classes

The runner's shell glue (`mvdan.cc/sh`) is a **bash-compatible subset**, not real bash — the
subprocesses it spawns for external commands are real, unmodified processes, but constructs that
live entirely in shell semantics (pipelines, subshells, `$$`, `IFS`, ...) can in principle behave
differently than bash. Most dangerous idioms are caught by a static screen and routed to real bash
before they ever execute under the runner (see SKILL.md "When a step falls back to legacy anyway").
Two classes are **not** statically screenable and remain documented, known limitations when a
screened-clean script does run on the runner:

- **`set -e` + command substitution** — interp's abort-on-error handling inside `$(...)` can
  diverge from bash's: the captured value, or the point at which the script aborts, can differ
  between the two.
- **`IFS=` reassignment + word-splitting** — interp's handling of empty fields during the split can
  drop or keep them differently than bash does.

A third idiom in the same danger class, `$$`/`$!` used in a word, *is* statically screened (any
occurrence routes to real bash automatically), so ordinary scripts are covered. The narrow edge
left over: a `$$`/`$!` built dynamically in a way that evades the static scan (e.g. via `eval`)
could still collide under concurrent runner execution, the same evasion category as any
dynamically-constructed gap construct.

These aren't guesses — they were measured with a 43-script bash-vs-runner differential corpus that
diffs every script's cwd/env/stdout/stderr/exit-code/files-touched against real bash on the same
input. 15 scripts match strictly under the runner, 26 get statically screened to real bash, and
exactly these 2 classes are the corpus's accepted residual divergences — 0 unexplained. If a
script's correctness depends on either idiom's exact bash semantics, add `"interp": false` to keep
it on the legacy path.
