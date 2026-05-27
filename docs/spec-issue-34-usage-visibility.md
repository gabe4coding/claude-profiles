# Spec: Delegate cost visibility via /usage (Issue #34)

## Background

Claude Code v2.1.149 extended `/usage` to show a per-category breakdown including
**subagents**. Each delegate invocation launched by `cmdDelegateBgDispatch` runs as a
background subagent, so all delegate cost now appears under the subagents line in `/usage`.

## What has already been done (this PR)

- `README.md` now notes under "Requirements" that delegate invocations count as subagent
  calls and appear in `/usage → subagents`.

## Remaining work

### Short-term: Surface a cost hint at dispatch time

In the `delegateSlashCommand` or in the `# 3. Acknowledge` step, add a note that:

> Each delegate invocation appears as a subagent call under `/usage`. Use `/usage` to
> diagnose unexpected limits consumption across profiles.

This ensures the information is visible in context (not just in the README).

### Medium-term: Per-profile cost in the hub TUI

The Claude Code agent doesn't expose usage data in a machine-readable format accessible
to claude-profiles today (no log file, no env var, no public API). This may change.

If Claude Code adds a usage export (JSON file in `~/.claude/`, env var, or MCP endpoint),
add a `claude-profiles usage` subcommand or a TUI screen in `cmdHub` that:
1. Reads usage data from the Claude Code data directory.
2. Groups by profile session (via the session→profile ledger in `session_profiles.go`).
3. Displays per-profile token consumption and estimated cost.

**This is a placeholder spec** — the feature is blocked on Claude Code exposing usage data
programmatically. File an upstream request if this is high priority.

### Doctor check (v2.1.149+)

The README version table already includes v2.1.149 (worktree sandbox fix). No separate
doctor check is warranted for the `/usage` subagent breakdown — it's a UI improvement,
not a reliability concern.

## Acceptance criteria (remaining)

1. `/delegate` slash command acknowledgment includes a note about `/usage → subagents`.
2. (Blocked until Claude Code exposes usage data programmatically) `claude-profiles usage`
   or hub TUI screen shows per-profile cost.
