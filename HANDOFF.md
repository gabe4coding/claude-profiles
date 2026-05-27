# HANDOFF: Issue triage + implementation for issues #27–#29, #34, #41–#45

All 9 open issues were triaged as valid; bugs were implemented and enhancements were
specced in the current PR on branch `claude/eloquent-carson-Ai217`.

## What's done (this session)

### Implemented (code changes)

- **#27 + #28 (doctor version checks)**: `checkClaudeBinary()` in `doctor.go` now parses
  `claude --version` and compares against:
  - Known-bad version table (`knownBadVersions` slice) — v2.1.147 Bash regression → FAIL
  - Minimum delegate baseline (v2.1.146) — permission re-prompting → WARN
  New helpers: `parseClaudeVersion()`, `versionAtLeast()`, `badVersionRange` type.

- **#41 partial (worktree sandbox doctor gate)**: `checkVersionConstraints()` in `doctor.go`
  warns when Claude Code is below v2.1.149 and any `_worktree` profile exists.

- **#44 (subagent stability gate)**: `checkVersionConstraints()` also warns when below
  v2.1.152 and any `_subagent_model` profile exists (background worker crash risk).

- **#29 partial (watcher message)**: `writeDispatchError` timeout message in `delegate_bg.go`
  now names memory pressure + permission re-prompting + upgrade hint as possible causes.

- **#42 (sessionTitle)**: `cmdHookSessionStart()` in `run.go` now reads `session_id` from
  stdin and emits `sessionTitle` = `"<profile> · claude-profiles"` (or `"[delegate] <profile>"`
  for delegate sessions) in `hookSpecificOutput`. Fallback: no key emitted if profile
  unresolvable. Effective on Claude Code v2.1.152+.

- **#43 (reloadSkills)**: `cmdHookSessionStart()` now includes `"reloadSkills": true` in
  `hookSpecificOutput`. Backward-compatible (ignored by older Claude Code). Effective on v2.1.152+.

- **#45 (PushNotification in delegate template)**:
  - `delegateSlashCommand` in `delegate.go` updated to note automatic PushNotification
    calls in step 3 (Acknowledge).
  - `cmdDelegateBgDispatch` in `delegate_bg.go` now appends a PushNotification instruction
    to prose tasks (skipped for slash-command tasks to avoid corrupting skill syntax).

- **#28 + #34 (README)**: `README.md` Requirements section now includes a version
  compatibility table (v2.1.146–v2.1.152) and a cost-visibility note for `/usage`.

- **#29 + #41 (CLAUDE.md)**: Added two new invariant entries:
  - `_worktree` profiles require v2.1.149+ for correct OS-level sandbox.
  - Delegate sessions are non-pinned and can be shed under memory pressure (v2.1.147+).

### Spec/plan documents (enhancements, not implemented)

- `docs/spec-issue-41-worktree-sandbox-audit.md` — write-path audit (preliminary: no
  cross-worktree writes found in current code), smoke test proposal, static guard proposal.
- `docs/spec-issue-34-usage-visibility.md` — per-profile cost in hub TUI (blocked on
  Claude Code exposing usage data programmatically).

## Open items

### Issue #29 — `--pin` flag investigation
The memory-pressure section of the spec (`docs/spec-issue-29-memory-pressure-shedding.md`)
is still open on the core question:

> **Does `claude --bg` accept a `--pin` or `--priority` flag?**

The watcher message and CLAUDE.md note are done. The programmatic-pinning branch depends on
this answer. Check by running `claude --bg --help` on a live system.

### Issue #34 — Hub TUI cost view
Blocked on Claude Code exposing usage data programmatically (no JSON file, env var, or
public API today). `docs/spec-issue-34-usage-visibility.md` tracks the requirement.

### Issue #41 — Worktree smoke test
`docs/spec-issue-41-worktree-sandbox-audit.md` has the test outline. Preliminary audit
found no cross-worktree writes in current code; smoke test would confirm the hook guard
works end-to-end.

### Issues #42 + #43 — Per-profile skills (medium-term)
The `reloadSkills: true` key is now emitted, enabling future per-profile skill sets.
The medium-term spec (symlink `.claude-profiles/<profile>/skills/*.md` into
`.claude/commands/<profile>/` at session start) is noted in issue #43 but not yet planned.

## Suggested order for next session

1. Run `claude --bg --help` → document pinning answer → implement if flag exists (#29).
2. Implement worktree smoke test (#41, low-risk).
3. Monitor for Claude Code usage data export → implement hub TUI cost view (#34).
