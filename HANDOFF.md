# HANDOFF: Version-guard enhancements from issue triage (issues #27–#29)

Issues #27 + #28 (doctor version checks) are implemented; issue #29 (memory-pressure
shedding) is partially done — unconditional parts shipped, pinning blocked on upstream.

## What's done

- **Issue #26 (closed)**: Updated SubagentModel minimum-version notes from v2.1.146 to
  v2.1.147 in three places:
  - `cmd/claude-profiles/delegate_bg.go` (inline comment, lines 228-230)
  - `CLAUDE.md` (SubagentModel invariant note)
  - `docs/spec-issue-18-subagent-model.md` (context section + dependencies)

- **Issues #27 + #28 (implemented, PR on branch `claude/eloquent-carson-3zoCe`):**
  - `cmd/claude-profiles/doctor.go`: Added `parseClaudeVersion`, `versionAtLeast`,
    `knownBadVersions` (v2.1.147 Bash regression), and extended `checkClaudeBinary()`
    to emit:
    - **fail** for v2.1.147 (known-bad: Bash tool exit code 127)
    - **warn** for <v2.1.146 (permission re-prompting in delegate bg sessions)
    - **ok** for v2.1.148+ (no known issues)
  - `cmd/claude-profiles/delegate_bg.go`: Watcher timeout `dispatch-error.md` message
    updated to include hints about memory pressure, permission re-prompting, and upgrade
    recommendation.
  - `README.md`: Updated minimum-version note to call out v2.1.147 as known-bad and
    recommend v2.1.148+.
  - `cmd/claude-profiles/doctor_test.go` (new): Unit tests for `parseClaudeVersion`,
    `versionAtLeast`, and the version-classification logic.

- **Issue #29 — unconditional parts (shipped in the same PR):**
  - `CLAUDE.md`: Added "Delegate sessions are non-pinned" operational-constraint note,
    including the investigation result (no `--pin` flag in the CLI as of v2.1.150).
  - `docs/spec-issue-29-memory-pressure-shedding.md`: Updated with investigation result
    and a list of remaining blocked items.
  - Watcher timeout message consolidated: memory-pressure hint merged with the
    permission re-prompting hint from #27/#28 into a single message.

## Open items (blocked on upstream)

### Issue #29 — programmatic pinning + smoke test
**Still open** — two remaining tasks:

1. **Programmatic pinning**: No `--pin` or `--priority` flag exists in `claude --bg`
   as of v2.1.150 (checked 2026-05-24). If Anthropic adds one, add it to
   `cmdDelegateBgDispatch` alongside `--permission-mode`, and add a `doctor` version
   check for the minimum version that supports it.

2. **Smoke test extension**: `scripts/smoke-delegate-bg.sh` does not yet have a
   mid-run session kill test (simulating memory-pressure shedding). Requires a
   `CLAUDE_PROFILES_WATCHER_TIMEOUT` env override (or build-tag-gated flag) so the
   smoke test can use a short abandon window (e.g. 10s) instead of the 30-minute
   production default. See `docs/spec-issue-29-memory-pressure-shedding.md` for
   the full test sketch.

## Suggested next step

1. Re-check `claude --bg --help` after future Claude Code updates for a `--pin` flag.
2. Add `CLAUDE_PROFILES_WATCHER_TIMEOUT` env override to `delegate_bg.go` (small,
   ~5 lines) and extend `smoke-delegate-bg.sh` with the kill test.
