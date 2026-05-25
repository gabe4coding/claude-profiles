# Spec: Surface delegate cost visibility via `/usage` (Issue #34)

## Background

Claude Code v2.1.149 extended `/usage` to show a **per-category breakdown** of limits
consumption, including a **subagents** line item. Every delegate session dispatched by
`cmdDelegateBgDispatch` runs as a background subagent and now appears under that line
item. Users who run multiple active profiles with frequent delegation have no current way
to understand which profiles are driving their limits — this `/usage` update gives them a
diagnostic path, but claude-profiles doesn't surface it anywhere.

Two concrete gaps:

1. **Documentation gap**: nothing tells users that each delegate invocation counts as a
   subagent call, and nothing points them to `/usage` → subagents when they hit limits
   unexpectedly.

2. **`claude-profiles doctor` gap**: no check verifies the user is on v2.1.149+ and
   therefore has access to the per-subagent breakdown in `/usage`. Heavy delegate users
   on older versions see no subagent line item and can't diagnose limits pressure at all.

## Affected components

- `README.md`: delegate section lacks a cost/limits note.
- `cmd/claude-profiles/doctor.go`: `checkClaudeBinary()` already checks v2.1.146 (min
  delegate) and v2.1.147 (known-bad). Extend the check to note v2.1.149 as the version
  that unlocks `/usage` subagent visibility.
- `cmd/claude-profiles/delegate_bg.go`: dispatch confirmation log (if any) could mention
  cost tracking.

## Proposed changes

### 1. README — add a cost/limits note to the `/delegate` section

In the `/delegate` feature description, add:

> **Cost visibility**: each delegate invocation is billed as a subagent call and appears
> under the **Subagents** category in `/usage` (Claude Code v2.1.149+). If you hit limits
> unexpectedly while running multiple active profiles, `/usage` → Subagents is the first
> place to look.

### 2. `doctor.go` — version advisory for `/usage` subagent visibility

Extend `checkClaudeBinary()` to add a non-blocking advisory at the info/ok level when
the active version is below v2.1.149. This is not a warning (below-minimum or known-bad
gets warn/fail); it's a "you could have this nice thing" note appended to the ok detail:

```go
// v2.1.149: /usage shows per-subagent breakdown — useful for delegate cost debugging.
const usageSubagentMaj, usageSubagentMin, usageSubagentPat = 2, 1, 149
if !versionAtLeast(maj, min, pat, usageSubagentMaj, usageSubagentMin, usageSubagentPat) {
    detail += " (upgrade to v2.1.149+ for per-subagent cost breakdown in /usage)"
}
return docCheck{"claude binary", "ok", path + " (" + version + ")" + detail}
```

### 3. (Medium-term) `claude-profiles usage` subcommand or TUI panel

Explore whether Claude Code exposes usage data in a machine-readable format (log file,
env var, or API endpoint) that claude-profiles could read to show per-profile delegate
cost in the hub TUI. This requires investigation — it is NOT part of the immediate
implementation.

## Acceptance criteria (immediate)

1. README `/delegate` section contains a sentence explaining that delegate calls appear
   under "Subagents" in `/usage`.
2. `claude-profiles doctor` on v2.1.148 produces an ok row for the claude binary that
   includes the v2.1.149 advisory in its detail text.
3. `claude-profiles doctor` on v2.1.149+ produces an ok row without the advisory.
4. `go build ./cmd/claude-profiles/` passes with no new lint warnings.

## Implementation plan

1. Add README note (5 min).
2. Add `usageSubagentMaj/Min/Pat` constants to `doctor.go`.
3. Extend the final `return` in `checkClaudeBinary()` to append the advisory when below
   v2.1.149.
4. Add a test case in `doctor_test.go` that verifies v2.1.148 gets the advisory text and
   v2.1.149 does not.

## Notes

- The advisory is intentionally non-fatal (no warn/fail) — being on an older Claude Code
  does not break any claude-profiles functionality, it just limits observability.
- The medium-term "claude-profiles usage" subcommand is deferred pending investigation
  of what machine-readable data Claude Code exposes. Do not start that until the
  exploration step is done.
