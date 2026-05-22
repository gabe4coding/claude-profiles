# HANDOFF: Version-guard enhancements from issue triage (issues #26–#29)

Issue #26 (version comment bump) was implemented directly; issues #27, #28, #29 are
enhancement specs awaiting implementation.

## What's done

- **Issue #26 (closed)**: Updated SubagentModel minimum-version notes from v2.1.146 to
  v2.1.147 in three places:
  - `cmd/claude-profiles/delegate_bg.go` (inline comment, lines 228-230)
  - `CLAUDE.md` (SubagentModel invariant note)
  - `docs/spec-issue-18-subagent-model.md` (context section + dependencies)

## Open items

### Issues #27 + #28 — `doctor` version checks
**Spec**: `docs/spec-issues-27-28-doctor-version-checks.md`

Both issues require the same core change: extend `checkClaudeBinary()` in `doctor.go`
to parse `claude --version` and compare against:
1. A minimum recommended version (v2.1.146 — below this, delegate bg sessions can
   silently timeout due to permission re-prompting in bg sessions).
2. A known-bad version table (v2.1.147 — Bash tool exit code 127 regression).

Also: update the watcher timeout `dispatch-error.md` message in `delegate_bg.go`
(line 375-376) to include an upgrade hint.

Estimated size: medium (~80 lines in `doctor.go`, ~5-line message update in
`delegate_bg.go`, README note, smoke/unit test).

### Issue #29 — Memory-pressure shedding diagnostics
**Spec**: `docs/spec-issue-29-memory-pressure-shedding.md`

Delegate sessions are non-pinned bg sessions and are shed first under memory pressure
(Claude Code v2.1.147+). Key open question before implementing:

> **Does `claude --bg` accept a `--pin` or `--priority` flag?**
> Run `claude --bg --help` on a live system. The entire programmatic-pinning branch is
> contingent on this answer.

Regardless of the answer, two items are unconditional:
- Watcher timeout message update (mention memory pressure; consolidate with #27/#28 hint).
- `CLAUDE.md` operational-constraint note for non-pinned shedding.

Estimated size: small-medium for the unconditional parts; medium for pinning if the
flag exists.

## Suggested order

1. Implement #27 + #28 together (one `checkClaudeBinary` extension, one message update).
2. Investigate the `--pin` flag, then implement #29.
