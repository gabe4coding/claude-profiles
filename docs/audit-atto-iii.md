# Audit for Atto III — demolition of the legacy tmux delegate path

**Status**: Atto II shipped (default flip done). This document is the input to Atto III.

## Goal of Atto III

Once the `--legacy-tmux` path has had time in the wild without regressions, demolish it entirely:

- Delete `cmdDelegateRunner`, `writeResultOnFirstTurnComplete`, the post-run JSONL fallback, and `delegate-watch.sh` / `delegateWatchScript`.
- Drop the `--legacy-tmux` flag and `CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX` env from `delegate-launch.sh`.
- Stop announcing `jsonl-path.txt` (it's a tmux-internal coordination file).
- Drop `OwnerRepo` enforcement at dispatch time and keep it as a hub filter hint only.
- Drop `delegate-<id>` worktree naming (the bg dispatcher uses Claude Code's auto-worktree).
- Replace `result.md` as the parent ↔ delegate integration point with a direct read of `~/.claude/jobs/<bg-id>/state.json` from the parent's UserPromptSubmit hook (Section "Hook redesign" below).

## Consumer map — `result.md`

`result.md` is currently *the* integration point between every delegate path and the parent. After Atto II, it is no longer written by the legacy tmux path's runner alone — both paths converge on the same file.

| Writer | Trigger | Path |
| --- | --- | --- |
| `cmdDelegateRunner.writeResultOnFirstTurnComplete` (legacy tmux) | First-turn complete in the delegate's JSONL | `cmd/claude-profiles/delegate.go:728` |
| `cmdDelegateRunner` post-run fallback (legacy tmux) | Watcher missed the first turn; runner reads JSONL after exit | `cmd/claude-profiles/delegate.go:557` |
| `cmdDelegateBgDispatch.writeDelegateResult` (bg) | Dispatch-time failure (profile resolve, disabled, OwnerRepo, claude missing) | `cmd/claude-profiles/delegate_bg.go:88, 93, 98, 108, 114, 133, 140, 192, 210, 217, 226, 230` |
| `cmdDelegateBgWatcher.writeDelegateResult` (bg) | First turn done OR timeout (state=done/blocked/failed/stopped) | `cmd/claude-profiles/delegate_bg.go:320, 333` |

| Reader | Purpose | Path |
| --- | --- | --- |
| `cmdHookPromptSubmit` | Inject as `additionalContext`, rename → `delivered.md` | `cmd/claude-profiles/delegate.go:924, 941-942` |
| `delegate-watch.sh` (legacy tmux) | Stop signal — watcher exits when `result.md` appears | `cmd/claude-profiles/delegate.go:174, 224, 226, 229, 271` |
| `cmdDelegateBgWatcher` pre-loop | Don't overwrite if already there (race with dispatcher's failure writes) | `cmd/claude-profiles/delegate_bg.go:286, 291` |

### Contract worth preserving

Two non-obvious invariants verified empirically during Atto I validation (2026-05-21):

1. **Rejected delegates ride the same channel.** `cmdDelegateBgDispatch` writes `result.md` via `writeDelegateResult` before its `fatal()` calls (profile not found, disabled, `OwnerRepo` mismatch, etc.). The parent hook then delivers that rejection message as `additionalContext`. Atto III's replacement protocol must support this — failure-before-session-start cases need to flow back to the parent without a `~/.claude/jobs/<bg-id>/state.json` ever existing.
2. **`writeDelegateResult` appends a delegate-cleanup reminder.** Anything reading `result.md` (today the parent hook, tomorrow the parent hook's replacement) gets the assistant's text plus a stable trailer. Atto III's redesign can drop the trailer if the cleanup is encoded into the new protocol, but the parent-facing message must still be self-explanatory.

## Consumer map — `jsonl-path.txt`

Tmux-internal coordination file. Not part of the parent contract.

| Writer | Trigger | Path |
| --- | --- | --- |
| `announceDelegateJSONLPath` (legacy tmux) | Polling the delegate's `claude --output-format=stream-json` (or fallback session-dir scan) | `cmd/claude-profiles/delegate.go:676` |

| Reader | Purpose | Path |
| --- | --- | --- |
| `delegate-launch.sh` (legacy tmux) | Pass through to slash command as `DELEGATE_JSONL` env (informational) | `cmd/claude-profiles/delegate.go:139, 142-143` |
| `cmdDelegateRunner` post-run fallback (legacy tmux) | Find the JSONL when `result.md` is missing | `cmd/claude-profiles/delegate.go:575` |
| `delegate-watch.sh` (legacy tmux) | Tail the JSONL for digest events | `cmd/claude-profiles/delegate.go:173, 182` |
| `writeResultOnFirstTurnComplete` (legacy tmux) | Read absolute JSONL path written by the announce goroutine | `cmd/claude-profiles/delegate.go:739, 741` |

**All four readers are inside the legacy tmux path.** Atto III deletes the writer (no `announceDelegateJSONLPath` without the runner) and all four readers in the same commit.

## Hook redesign — parent ↔ delegate without `result.md`

The bg path already exposes everything the parent hook needs via `~/.claude/jobs/<bg-id>/state.json`. The redesign:

1. **Parent's UserPromptSubmit hook** (`cmdHookPromptSubmit`) walks `~/.claude-profiles/delegates/<parent-sid>/*/bg-session-id.txt` instead of scanning for `result.md`.
2. For each bg id found, read `~/.claude/jobs/<bg-id>/state.json`. If `state` is terminal (`done`, `blocked`, `failed`, `stopped`) and the delegate dir has no `delivered.txt` marker, extract the assistant text from `linkScanPath` and inject as `additionalContext`. Write the `delivered.txt` marker.
3. **Failure-before-session-start** (no `bg-session-id.txt`): leave a `dispatch-error.md` file next to `request.json`. The hook walks both kinds of file. `dispatch-error.md` rename to `delivered-error.md` after injection. (Cleaner than overloading `result.md` for two different shapes.)
4. **`writeDelegateResult` callers in `cmdDelegateBgDispatch`** rewrite to call a new `writeDispatchError(dir, msg)` helper.
5. Watcher writes nothing to the delegate dir on success — the hook reads `state.json` directly. (Bonus: removes the watcher → hook timing dependency.)
6. Watcher continues writing `dispatch-error.md` on timeout (`abandoned by bg watcher`) because there's no state for "the watcher gave up" — that's a watcher-owned signal, not a Claude Code one.

### What stays

- `bg-session-id.txt` (dispatcher → hook).
- `request.json` (dispatcher → hook for the failure-before-session-start case).
- `delivered.txt` / `delivered-error.md` markers — same role `delivered.md` plays today, just a different filename to make the protocol switch a one-shot migration rather than a silent rename.

### What goes

- `result.md` (replaced by `state.json` + `dispatch-error.md`).
- `delivered.md` (replaced by `delivered.txt` marker — content no longer kept, the hook re-extracts from state if it ever needs to).
- `jsonl-path.txt` (tmux-only, deleted with the runner).
- `delegate-watch.sh` / `delegateWatchScript` (tmux-only).
- `cmdDelegateRunner`, `writeResultOnFirstTurnComplete`, `announceDelegateJSONLPath` (tmux-only).
- `--legacy-tmux` flag, `CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX` env, the entire legacy branch in `delegateLaunchScript`.

## Migration sequencing for Atto III

1. **Add new protocol alongside old.** New `cmdHookPromptSubmit` reads both the new files and the old `result.md`. New `cmdDelegateBgDispatch` / watcher write the new files only. Old paths still work for delegates dispatched before the upgrade.
2. **Bake-in window.** Let users run the new wrapper for a session or two so old `result.md` files drain naturally.
3. **Delete the legacy tmux runner + watcher.** This is the destructive step. After this, `--legacy-tmux` errors out.
4. **Delete the `result.md` reader in the hook.** Final cleanup; the new protocol stands alone.

Each step ships its own smoke. Step 3's smoke (legacy tmux gone) replaces `smoke-delegate-legacy-tmux.sh` (if we ever add one) with a "must error" assertion.

## PR #19 constraints to preserve through Atto III

[PR #19](https://github.com/gabe4coding/claude-profiles/pull/19) (open draft as of writing — issues #16/#17/#18) lands a set of bg-only invariants that Atto III must keep working when it tears down the tmux path. None overlap with my audit's demolition targets; record them here so the next session doesn't accidentally regress them.

1. **`CLAUDE_PROFILES_DELEGATE=1` env injection (load-bearing).** `cmdDelegateBgDispatch` already sets this on `cmd.Env` (`delegate_bg.go:206`). `cmdHookStop` (`distill.go`) bails when it sees this env — preventing the delegate's Stop hook from advancing the parent profile's distill bookmark. Atto III's hook redesign must not drop this guard, even if `result.md` is gone; the guard prevents *delegate-side* misbehavior, not parent-side delivery.
2. **Resumability is explicitly unsupported.** `/resume` on a completed bg delegate (Claude Code v2.1.144+ shows them in the picker) does not re-trigger result delivery — the watcher is gone and `result.md` is settled. The new hook protocol in Atto III must preserve this: scanning `state.json` on every parent `UserPromptSubmit` should detect "already delivered" via the marker file (`delivered.txt`) and skip, even if the session id is technically still "alive" in Agent View.
3. **Minimum Claude Code version v2.1.146 for slash-command tasks.** `cmdDelegateBgDispatch` documents this; the smoke covers it (Step 5 in `smoke-delegate-bg.sh` after PR #19 lands). Atto III's redesign should not bump this floor unless there's a concrete reason — bg dispatch should keep working for tasks that start with `/`.
4. **Issue #18 (`SubagentModel`) targets the bg dispatcher only.** Spec at `docs/spec-issue-18-subagent-model.md`. When implemented, `cmdDelegateBgDispatch` will inject `CLAUDE_CODE_SUBAGENT_MODEL` from `ProfilePrefs.SubagentModel`. Atto III's hook redesign is unaffected — this is a dispatcher concern, not a parent-hook concern.

## Issue #9 Steps 2-3 — interaction with Atto III

`docs/spec-issue-9-agent-discovery.md` Steps 2 (integrate `claudeAgentsJSON` into `announceDelegateJSONLPath`) and 3 (fallback in `cmdDelegateRunner`) both touch legacy tmux code. Atto III deletes both targets.

**Recommendation**: skip Issue #9 Steps 2-3. The integration target disappears in Atto III. Mark them as "deferred — superseded by Atto III demolition" in HANDOFF.md when Atto III lands.

## Open questions for the Atto III session

- Should `dispatch-error.md` exist as a separate file, or unify under one `result-or-error.md` schema? (Above prefers separate — cleaner type signal, no overloading.)
- The watcher's "abandoned after 30 minutes" case: write `dispatch-error.md` or a new `watcher-error.md`? (Above bundles into `dispatch-error.md` for fewer code paths; `state.json` already records `stopped` so the type information lives there.)
- Do we keep `OwnerRepo` enforcement at all? (HANDOFF says "keep as hub hint only" — confirm before deletion.)
