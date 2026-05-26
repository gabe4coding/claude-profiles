# HANDOFF: Version-guard enhancements and enhancement specs (issues #27–#29, #34)

Issues #27 and #28 (doctor version checks) are now implemented. Issues #29 and #34
are enhancement specs awaiting implementation.

## What's done

- **Issue #26 (closed)**: Updated SubagentModel minimum-version notes from v2.1.146 to
  v2.1.147 in three places.

- **Issues #27 + #28 (implemented, PR open)**:
  - `cmd/claude-profiles/doctor.go`: Added `parseClaudeVersion()`, `versionAtLeast()`,
    `badVersionRange` type, `knownBadVersions` table (v2.1.147 Bash regression), and
    version-threshold constants (`minDelegate*` = v2.1.146, `recommended*` = v2.1.149).
    `checkClaudeBinary()` now produces FAIL for known-bad releases and WARN for versions
    below the minimum/recommended thresholds.
  - `cmd/claude-profiles/delegate_bg.go`: Watcher timeout `dispatch-error.md` message
    extended to mention memory pressure (v2.1.147+), permission re-prompting (< v2.1.146),
    and a pointer to `claude-profiles doctor`.
  - `README.md`: Requirements section replaced with a version table
    (v2.1.146 min → v2.1.147 known-bad → v2.1.148 fixed → v2.1.149 recommended).

- **Issues #38 + #39 (closed as noise)**:
  - Code audit confirmed all writes in `delegate_bg.go` and `distill.go` target
    `~/.claude-profiles/` (home dir) — not repo-relative paths — so the v2.1.149
    sandbox tightening does not affect claude-profiles.
  - `/insights` crash (issue #39) is in Claude Code (fixed in v2.1.149); the
    `state.json` is owned by the Claude Code daemon and is read-only from our side.
    The doctor recommended-minimum check (v2.1.149) covers users on older versions.

## Open items

### Issue #29 — Memory-pressure shedding diagnostics
**Spec**: `docs/spec-issue-29-memory-pressure-shedding.md`

Key open question before implementing the programmatic-pinning branch:

> **Does `claude --bg` accept a `--pin` or `--priority` flag?**
> Run `claude --bg --help` on a live system. The entire programmatic-pinning branch
> is contingent on this answer.

Unconditional work already done:
- Watcher timeout message updated to mention memory pressure (folded into #27/#28 PR).
- CLAUDE.md note about non-pinned shedding is still pending (see spec §3).

Remaining unconditional items:
1. Add CLAUDE.md operational-constraint note (spec §3).
2. (Optional) Add `CLAUDE_PROFILES_WATCHER_TIMEOUT` env override for smoke-test speed.
3. Extend `scripts/smoke-delegate-bg.sh` with kill-based shed simulation (spec §4).

### Issue #34 — Delegate cost visibility via `/usage`
**Spec**: `docs/spec-issue-34-usage-cost-visibility.md`

Three phases defined in the spec:
1. **Phase 1** (immediate, low risk): Add cost note to README delegate section.
   No code change beyond docs.
2. **Phase 2** (medium): Hub TUI profile list shows delegate display-name format
   so users can cross-reference with `/usage → subagents`. Requires bubble-tea
   model change in `hub.go`.
3. **Phase 3** (long-term): `claude-profiles usage` subcommand — contingent on
   Claude Code exposing machine-readable per-session usage data.

Open question: Does `~/.claude/logs/` or `state.json` contain token/cost fields?
Check before implementing Phase 3.

## Suggested order

1. Phase 1 of issue #34 (README note — trivial, safe to batch with any PR).
2. Issue #29 unconditional items: CLAUDE.md note + smoke test.
3. Investigate `claude --bg --help` for `--pin` flag, then implement #29 pinning if available.
4. Phase 2 of issue #34: hub TUI cost hint (requires bubble-tea model work).
5. Phase 3 of issue #34: only if machine-readable usage data is confirmed.
