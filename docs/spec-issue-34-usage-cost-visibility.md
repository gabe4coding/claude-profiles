# Spec: Delegate cost visibility via `/usage` (Issue #34)

## Background

Claude Code v2.1.149 added a per-category breakdown to `/usage` that includes a
**subagents** line. Every delegate session launched by `cmdDelegateBgDispatch`
runs as a background subagent, so its token/cost consumption now appears there.

Two gaps today:

1. **Documentation gap**: Users don't know that delegate invocations appear under
   "subagents" in `/usage`. A heavy delegate user who hits limits unexpectedly has
   no obvious path to diagnosing which profile is responsible.

2. **UX gap**: The claude-profiles hub TUI has no cost visibility. There is no way
   to see per-profile delegate spend without manually cross-referencing `/usage`
   output with session names.

## Affected components

- `README.md` — delegate docs section
- `cmd/claude-profiles/hub.go` — TUI hub (future: cost column or screen)
- `cmd/claude-profiles/doctor.go` — already has the v2.1.149 recommended-minimum
  check added in issues #27/#28 implementation (folded in there)

## Proposed changes

### Phase 1 — Documentation (low risk, immediate)

1. Add a note to the `/delegate` section of `README.md`:

   > **Cost note**: Each delegate dispatch is counted as a subagent call. Run
   > `/usage` inside any Claude Code session to see per-subagent cost breakdown
   > (requires Claude Code ≥ v2.1.149). If you hit limits unexpectedly while
   > running multiple profiles, `/usage → subagents` is the right place to
   > diagnose which profile is driving consumption.

2. Add a tip to the `dispatch-error.md` or watcher flow (optional): no change
   needed; the existing error message already points to `claude-profiles doctor`
   which checks the version.

### Phase 2 — Hub cost hint (medium, requires research)

Add a one-liner in the hub TUI profile list that shows the delegate display name
format (`<profile>:<task-truncated>`) so users can cross-reference with `/usage`
subagent names:

- The display name is composed by `bgDisplayName()` in `delegate_bg.go`.
- No machine-readable cost data is available from the Claude Code API today
  (see Open Question below).
- Until an API exists, show the display-name format as a hint so users know what
  to look for in `/usage`.

### Phase 3 — `claude-profiles usage` subcommand (long-term, TBD)

If Claude Code exposes usage data in a machine-readable format (log file, env
var, structured output from `/usage`), add a `claude-profiles usage` subcommand
that aggregates per-profile delegate cost by matching session names.

This is contingent on the API existing — do not implement until the format is
confirmed stable.

## Open question

**Does Claude Code expose per-session usage data in a machine-readable format?**
Check whether `~/.claude/logs/` or `~/.claude/jobs/<id>/state.json` contains
token/cost fields. If yes, Phase 3 becomes viable. If no, Phase 1+2 are the
entire deliverable.

## Acceptance criteria

### Phase 1
1. `README.md` delegate section includes the cost note above.
2. The note mentions `/usage → subagents` and the v2.1.149 requirement.

### Phase 2
1. Hub TUI profile list shows the delegate name format as a tooltip or secondary
   line (exact UX TBD — keep it non-intrusive).

## Implementation plan

1. **Phase 1** (immediate):
   - Edit `README.md` delegate section to add the cost note.
   - `go build` + tests pass.

2. **Phase 2** (when picking up this spec):
   - Investigate `bgDisplayName()` format vs. what `/usage` shows for subagent names.
   - Add a tooltip/secondary line to `hub.go` profile list (bubble-tea model change).
   - Smoke test: launch a delegate, verify the hub shows the matching display name.

3. **Phase 3** (only if machine-readable data found):
   - Add `cmdUsage` subcommand that reads usage data and filters by profile name prefix.

## Notes

- The `doctor.go` check for v2.1.149 as the recommended minimum already covers the
  "are they on a version where /usage shows subagent breakdown?" question — no
  separate doctor check needed for this issue.
- Phase 1 is safe to ship anytime. Phase 2 requires a bubble-tea model change that
  should be reviewed for accessibility (screen-reader friendly format for the hint).
