# Spec: Use `background_tasks` and `session_crons` in distill Stop hook

**Issue**: #7  
**Kind**: enhancement  
**Affected file**: `cmd/claude-profiles/distill.go` (`cmdHookStop`), `scripts/smoke-distill.sh`

---

## Problem

Claude Code v2.1.145 extended the Stop hook stdin payload with two new fields:

- `background_tasks` — non-empty when bg delegates (or other background agent tasks) are still in flight when the main session's turn ends.
- `session_crons` — non-empty when the stopping turn was scheduled by a session cron rather than triggered interactively by the user.

`cmdHookStop()` today parses only `session_id` and `stop_hook_active`. It has no awareness of either field.

Two problematic scenarios follow from the omission:

1. **In-flight delegates**: if a parent session ends its turn while a `--bg` delegate is still running, the Stop hook fires and (when conditions are met) emits `decision:block` to run distillation. But the delegate's commits haven't landed yet — the distillation runs over incomplete history and advances the bookmark past uncommitted delegate work. When the delegate finally commits, those commits look "already distilled" and are silently skipped.

2. **Cron-triggered turns**: a session cron ends its turn → Stop hook fires → distillation is triggered. Cron turns are non-interactive; distillation of a robot-initiated turn adds noise without user value, and the block it emits stalls the cron's exit unnecessarily.

---

## Proposed changes

### `cmd/claude-profiles/distill.go`

Extend the input struct in `cmdHookStop()`:

```go
var input struct {
    SessionID       string            `json:"session_id"`
    StopHookActive  bool              `json:"stop_hook_active"`
    BackgroundTasks []json.RawMessage `json:"background_tasks"`
    SessionCrons    []json.RawMessage `json:"session_crons"`
}
```

Add two new early exits **before** `saveLastDistillBookmark` (so the bookmark does NOT advance — we want to retrigger distillation on the next real commit):

```go
// Skip while bg delegates are in flight — their commits aren't landed yet.
if len(input.BackgroundTasks) > 0 {
    return
}
// Skip cron-triggered turns — not interactive work, distillation adds noise.
if len(input.SessionCrons) > 0 {
    return
}
```

Place these guards immediately after the `stop_hook_active` check (line 71) and before the profile/distill-setting checks, so they short-circuit cheaply without loading the profile.

### `scripts/smoke-distill.sh`

Add two test cases that pass synthetic payloads to `claude-profiles _hook-stop` and assert the hook exits 0 with no output (no block emitted):

```bash
# Case: background_tasks present — must NOT block
payload='{"session_id":"test-sid","stop_hook_active":false,"background_tasks":[{"id":"abc"}]}'
out=$(echo "$payload" | claude-profiles _hook-stop 2>/dev/null)
[ -z "$out" ] || fail "Expected no output with background_tasks, got: $out"

# Case: session_crons present — must NOT block
payload='{"session_id":"test-sid","stop_hook_active":false,"session_crons":[{"name":"nightly"}]}'
out=$(echo "$payload" | claude-profiles _hook-stop 2>/dev/null)
[ -z "$out" ] || fail "Expected no output with session_crons, got: $out"
```

---

## Invariant notes

- **Bookmark must NOT advance on these early exits.** The existing early-exit paths (no wrapper context, no committed work, no non-Claude commits) all return before `saveLastDistillBookmark`. The new guards must follow the same pattern — they appear before line 110 in the current file.
- **`stop_hook_active` guard stays first.** It prevents infinite distill loops and must remain the highest-priority check.
- **`json.RawMessage` is the right type.** We only care about whether the slice is empty; we don't need to deserialize the task/cron structs. `json.RawMessage` parses any array element without schema coupling to Claude Code internals.

---

## Open questions

1. Are `background_tasks` entries present for tmux-mode `/delegate` sessions (not `--bg`)? If yes, the guard correctly defers distillation in both delegate modes. If no, tmux delegates are unaffected (which is fine — their commits land synchronously before the parent's Stop hook fires).
2. Does `session_crons` appear in non-cron turns with an empty array `[]`, or is the key absent entirely? `json.Decoder` treats both as `len == 0`, so the guard is safe either way.

---

## Implementation order

1. Extend the input struct.
2. Add the two guards.
3. Extend `scripts/smoke-distill.sh` with the two new test cases.
4. Build and run `./scripts/smoke-distill.sh` — must exit 0.
5. Run `./scripts/smoke-ui.sh` (Go unit tests) — must pass.
