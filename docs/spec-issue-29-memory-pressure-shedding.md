# Spec: Memory-pressure delegate shedding (Issue #29)

## Background

Claude Code v2.1.147 introduced an explicit shedding hierarchy for background sessions:
pinned sessions (Ctrl+T in `claude agents`) survive memory pressure; **non-pinned
sessions are shed first**.

Delegate sessions are non-pinned bg sessions. On memory-constrained systems, the Claude
Code runtime will now preferentially terminate delegate sessions before any user-pinned
interactive sessions. The failure path:

1. Delegate dispatched → `bg-session-id.txt` written.
2. Memory pressure → Claude Code sheds the delegate bg session.
3. `cmdDelegateBgWatcher` never sees a terminal state in `state.json` → times out →
   writes `dispatch-error.md`.
4. Parent hook injects a timeout error. User sees a vague failure with no indication
   that memory pressure was the cause.

A secondary risk: the v2.1.147 "restart in place" behavior for *pinned* sessions raises
the question of whether a restarted session preserves its bg session ID. This does not
affect non-pinned delegates today, but if pinning is ever added the watcher's
`claude stop <bg-id>` would target a stale ID and the delegate would never be cleanly
stopped.

## Affected components

- `cmd/claude-profiles/delegate_bg.go`
  - `cmdDelegateBgDispatch` (~line 160–240): launches the `claude --bg` subprocess.
  - `cmdDelegateBgWatcher` (~line 282–377): polls state.json and handles timeout.
  - Watcher timeout error message (line 375-376): no mention of memory pressure.
- `CLAUDE.md`: no mention of memory-pressure shedding as an operational constraint.
- `scripts/smoke-delegate-bg.sh`: no simulated mid-run session termination test.

## Proposals

### 1. Investigate CLI pinning flag (ACTION REQUIRED before implementation)

Check whether `claude --bg` accepts a `--pin`, `--priority`, or analogous flag to
launch a session as pinned. If it does:

- Add the flag to `cmdDelegateBgDispatch` (next to `--permission-mode auto`).
- Document the minimum Claude Code version that supports the flag.
- This fully resolves the memory-pressure shedding risk.

If no such flag exists in the current CLI:
- Document the gap in CLAUDE.md and the dispatch code.
- Consider filing an upstream feature request with Anthropic.

**Verification method**: run `claude --bg --help` (or `claude help bg`) and check for
pinning-related flags. This requires a Claude Code binary on the path — cannot be
verified statically from the codebase.

### 2. Improve watcher timeout diagnostic (always applicable)

In `delegate_bg.go` line 375-376, extend the `writeDispatchError` message to mention
memory pressure as a possible cause (complementing the permission-reprompt hint from
spec-issues-27-28):

```go
writeDispatchError(dir, fmt.Sprintf(
    "(delegate %s abandoned by bg watcher after %s — session %s stopped; "+
        "possible causes: memory pressure (non-pinned bg sessions are shed first "+
        "under pressure on Claude Code v2.1.147+), network failure, or permission "+
        "re-prompting (upgrade to v2.1.146+ to rule out the last cause); "+
        "attach with `claude attach %s` if the session might still be useful)",
    delegateID, bgWatcherAbandonAfter, bgID, bgID))
```

Note: this overlaps with the improvement proposed in spec-issues-27-28. Consolidate
into a single message update covering both hints.

### 3. CLAUDE.md documentation

Add an operational-constraint note to the `**`/delegate` is bg-only`** section:

> **Delegate sessions are non-pinned and can be shed under memory pressure (v2.1.147+).**
> On memory-constrained systems, Claude Code will terminate delegate bg sessions before
> user-pinned interactive sessions. The watcher handles this identically to a timeout
> (dispatch-error.md). No programmatic pinning flag is available in the CLI today; if
> one becomes available, add it to `cmdDelegateBgDispatch` alongside `--permission-mode`.

### 4. Smoke test for mid-run session termination

Extend `scripts/smoke-delegate-bg.sh` to verify that killing the mock `claude` process
mid-run produces `dispatch-error.md` rather than a hang:

```bash
# Step N: simulate memory-pressure shedding
echo "--- Step N: simulate shed (kill bg process mid-run) ---"
# Launch a delegate with a slow mock claude, then kill it
# ...delegate dispatch...
BG_PID=$(cat "$DELEGATE_DIR/bg-pid.txt" 2>/dev/null)  # watcher writes this
if [ -n "$BG_PID" ]; then
    kill "$BG_PID" 2>/dev/null
fi
# Wait for watcher to detect the gone session and write dispatch-error.md
# (watcher polls every 2s; use a shorter timeout for smoke test by overriding
#  bgWatcherAbandonAfter via an env var or test-only flag — see note below)
sleep 10
[ -f "$DELEGATE_DIR/dispatch-error.md" ] || fail "dispatch-error.md not written after process kill"
echo "PASS: dispatch-error.md written on process kill"
```

Note: `bgWatcherAbandonAfter = 30 * time.Minute` makes a real kill-based test slow.
Consider adding a `CLAUDE_PROFILES_WATCHER_TIMEOUT` env override (test-only, gated on
a build tag) to allow the smoke test to use a shorter abandon window (e.g. 10s).

## Acceptance criteria

1. `dispatch-error.md` message contains the phrase "memory pressure" when a watcher
   timeout fires.
2. `CLAUDE.md` documents non-pinned delegate shedding as an operational constraint.
3. (If CLI pinning flag exists) `cmdDelegateBgDispatch` passes the pin flag; smoke test
   verifies the flag appears in the bg invocation.
4. (Smoke test) A mid-run mock-process kill produces `dispatch-error.md` within a
   configurable watcher timeout.

## Implementation plan

1. **Investigate**: run `claude --bg --help` on a live system; check for `--pin` or
   `--priority`. Document result in this spec before writing code.
2. If pin flag found: add it to `cmdDelegateBgDispatch`; add `checkClaudeVersion` gate
   in `doctor.go` for the minimum version that supports it.
3. Update watcher timeout error message in `delegate_bg.go` (consolidate with #27/#28
   message improvements — single `writeDispatchError` call, combined hint text).
4. Update `CLAUDE.md` with the shedding constraint note.
5. (Optional) Add `CLAUDE_PROFILES_WATCHER_TIMEOUT` env override for testability.
6. Extend `scripts/smoke-delegate-bg.sh` with the kill-based shed simulation.

## Open question

Does `claude --bg` support a `--pin` flag as of v2.1.147+? This is the key unknown.
The entire "programmatic pinning" branch of this spec is contingent on the answer.
Check before implementing step 2.
