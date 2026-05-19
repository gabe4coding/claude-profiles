# Handoff

**Pending work**: migrate `/delegate` from its tmux-based runner to `claude --bg` (Claude Code Agent View) — Atto I shipped in PR #4 as an opt-in flag, Atti II/III (default flip + demolition of `result.md` as the parent ↔ delegate integration point) still to do.

This file is a baton between sessions. If it exists at session start, the agent must ask whether to continue from here before doing other work. Delete it when the feature is shipped end-to-end or explicitly dropped (see CLAUDE.md → "Session handoffs").

---

## Context

`/delegate` today spawns a `claude` instance inside a tmux window via `cmdDelegateRunner`; the runner extracts the first assistant reply from the session JSONL and writes `result.md` in `~/.claude-profiles/delegates/<parent-sid>/<id>/`. The parent's UserPromptSubmit hook (`cmdHookPromptSubmit`) reads `result.md` and injects it as `additionalContext`, then renames to `delivered.md`.

We're migrating the **execution layer** to `claude --bg` (so delegates show up in Agent View with peek/attach/needs-input for free) while preserving the **integration protocol** (`result.md` consumed by the hook). The migration is split into three atti because doing it all at once would put protocol changes and execution changes in the same risk basket.

## Where we are

- **Atto I — SHIPPED** in [PR #4](https://github.com/gabe4coding/claude-profiles/pull/4) (draft as of writing).
  - Opt-in flag `/delegate --bg` (or env `CLAUDE_PROFILES_DELEGATE_BG=1`)
  - New Go code: `cmd/claude-profiles/delegate_bg.go` (dispatcher + state watcher)
  - Launch script (`delegateLaunchScript`) branches on `--bg`; slash command markdown updated
  - Hub palette key `a` → opens `claude agents --cwd <mainRepoRoot>` (no profile-locking flags)
  - Smoke test `scripts/smoke-delegate-bg.sh` exercises dispatch → state.json fixture → watcher → result.md → hook injection
  - Copilot review addressed: settings.json moved to `<delegate-dir>` (no `/tmp` leak), rune-aware truncation in `bgDisplayName`, cutset dedup in `parseBackgroundedID`, fatal-on-write-fail for settings, `ensureHandoffSlashCommand` no longer gates on `tmuxAvailable()` (so `/delegate --bg` is reachable without tmux)

## Open items (in priority order)

1. **Real-world validation of Atto I.** Smoke uses a stub `claude`. Before flipping defaults we want a human to actually dispatch a few `--bg` delegates and confirm: state.json shape stable, distill Stop hook actually fires under `--bg`, parent hook delivers result.md content end-to-end.

2. **Atto I.5 (optional, additive) — `claude-profiles goal` command.** Pure read layer over `~/.claude/jobs/*/state.json`:
   - `/delegate --bg --goal <name>` prepends `goal:<name> | ` to the display name in `bgDisplayName`
   - `claude-profiles goal list` groups bg sessions by `goal:*` prefix, prints `goal → N working, M blocked, K completed`
   - `claude-profiles goal show <name>` filters one goal
   - No changes to `result.md` or the hook; no risk to existing flows
   - Stack as new PR on top of `claude/define-next-task-dfkie` (or on top of `main` after PR #4 merges)

3. **Atto II — Default flip + audit.** Flip `/delegate` default to `--bg`; old path behind `--legacy-tmux`. Audit consumers of `result.md` / `jsonl-path.txt` (one known: `cmdHookPromptSubmit`). Must come *after* (1).

4. **Atto III — Demolition.** Delete tmux runner (`cmdDelegateRunner`, `writeResultOnFirstTurnComplete`, post-run fallback), `result.md` as integration point, `jsonl-path.txt`, `OwnerRepo` enforcement at dispatch (keep as hub hint only), `delegate-<id>` worktree naming. Redesign parent ↔ delegate handoff to read `~/.claude/jobs/<id>/state.json` directly from the hook.

## How to resume

1. Confirm PR #4 status (`mcp__github__pull_request_read --method get`).
2. Ask the user which Atto / open item to pick up.
3. Branch naming: `claude/<short-handle>-XXXX` off latest `main`.
4. Run all three smoke tests before pushing:
   - `./scripts/smoke-delegate-bg.sh` (new)
   - `./scripts/smoke-distill.sh` (existing, guards distill Stop hook)
   - `./scripts/smoke-ui.sh` (Go unit tests)
5. Always create the PR as **draft**.

## Hot spots not to forget

- `claude --bg` is a **hidden flag** (does not appear in `claude --help`), but it works on v2.1.139+. The `agents` subcommand IS documented; the `--bg` flag is documented at https://code.claude.com/docs/en/agent-view#dispatch-new-agents.
- `~/.claude/jobs/<short-id>/state.json` is the supervisor's source of truth. Key fields: `state` (`working|blocked|completed|failed|stopped`), `linkScanPath` (absolute path to the session JSONL), `sessionId`, `daemonShort`, `intent`, `name`.
- The bg watcher treats `blocked` as "first turn done" — delegates don't drive multi-turn conversations, so the natural "awaiting input" state after one assistant reply is our terminal signal.
- Settings file for `--bg` lives at `<delegate-dir>/settings.json` (NOT `os.TempDir`). Cleanup is tied to the delegate dir lifecycle.
- The `--bg` path does NOT pass `--worktree`: Claude Code's bg machinery auto-creates one under `.claude/worktrees/` with cleanup tied to `claude rm`. For profiles with `Worktree:true`, this is a behaviour change confined to the `--bg` path (no deterministic `delegate-<id>` name).

## When to delete this file

- **Feature complete** = Atto III shipped end-to-end (default is `--bg`, tmux path demolished, `result.md` replaced or formally retired). Delete and add a final invariant to CLAUDE.md if any non-obvious gotchas remain.
- **Dropped** = decision to abandon the migration. Delete and (optionally) note in CLAUDE.md why, so future agents don't reopen the question.
