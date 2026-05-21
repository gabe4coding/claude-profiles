# Handoff

**Pending work**: restore Monitor-style live progress streaming for `/delegate` after Atto III demolished `delegate-watch.sh` — the bg-only architecture removed the watcher subprocess whose stdout fed Monitor, and we want a clean replacement that doesn't reanimate the bash script.

This file is a baton between sessions. If it exists at session start, the agent must ask whether to continue from here before doing other work. Delete it when the feature is shipped end-to-end or explicitly dropped (see CLAUDE.md → "Session handoffs").

---

## Context

Atto III ([PR #21](https://github.com/gabe4coding/claude-profiles/pull/21)) deleted `delegateWatchScript` along with the rest of the tmux runner. That script's job was to tail the delegate's session JSONL and emit one digest line per window (`⏳` heartbeat, `⚠ stuck`, `✗ abandoning`, `✓ delegate done`) for the parent agent to consume via the Monitor tool. Bg delegates were never given an equivalent — the design accepted "headless delegates, result delivered on next prompt" as the trade-off.

The user noted in-session that they'd like to keep the Monitor pattern available for delegates that genuinely benefit from live observation (long tool sequences, debugging delegate behavior, watching for stuck calls). The dispatch / hook / state.json protocol from Atto III is unchanged — this is purely a *new* opt-in capability layered on top.

## Two candidate designs

### Option 1 — Helper Go subcommand + native Monitor tail (recommended)

Add a hidden subcommand `claude-profiles _delegate-jsonl <delegate-id>` that:

1. Reads `bg-session-id.txt` from the delegate dir.
2. Polls `~/.claude/jobs/<bg-id>/state.json` until `linkScanPath` is non-empty (the bg supervisor takes ~5s to materialise the JSONL after dispatch — handle the race).
3. Prints the absolute path to stdout and exits.

Callers compose their own `tail -F` + `jq --unbuffered` pipeline and hand it to Monitor:

```bash
JSONL=$(claude-profiles _delegate-jsonl <delegate-id>)
tail -F "$JSONL" | jq -r --unbuffered '
  select(.type == "assistant") |
  .message.content[]? |
  select(.type == "tool_use") |
  "🔧 \(.name)"
'
```

Exit when `state.json.state` reaches a terminal value (`done`/`blocked`/`failed`/`stopped`) — a tiny secondary poll loop in the pipeline.

**Pros**: ~30 lines Go, no bash watcher, filters live at the call site (skill, agent, ad-hoc), composable. Stays consistent with the Atto III "the Go binary owns lifecycle, the hook owns delivery" split.

**Cons**: the standardised digest output the old script provided (`⏳`/`⚠`/`✗`/`✓`) now lives in the caller — each consumer rewrites it, or we ship a single template in the slash-command markdown.

### Option 2 — Reanimate `delegate-watch.sh`

Restore a bash watcher very close to the deleted `delegateWatchScript`, but:

- Reads `bg-session-id.txt` instead of `jsonl-path.txt`.
- Resolves `linkScanPath` from `state.json` instead of reading a separate path file.
- Exits on `state.json.state` terminal instead of on `result.md` appearing.
- Keeps the same `⏳`/`⚠`/`✗`/`✓` digest contract for Monitor.

**Pros**: UX identical to Atto I/II, no per-caller filtering, no new Go subcommand.

**Cons**: ~130 lines of bash that mostly duplicate the Atto II watcher we just removed. Re-introduces a surface (state.json schema, JSONL parsing, digest format) that needs to stay in sync with two languages.

## Recommendation

Option 1. The helper subcommand is the smaller and more composable piece. If we want a standard digest, ship the `tail -F | jq` template as a docstring in `delegateSlashCommand` markdown — that keeps the recipe documented for the calling agent without re-introducing a bash script we'd have to keep maintained.

## How to resume

1. Confirm Atto III ([PR #21](https://github.com/gabe4coding/claude-profiles/pull/21)) is merged or about to merge.
2. Branch naming: `claude/atto-iii-monitor-restore-XXXX` off latest `main`.
3. Implement `_delegate-jsonl <delegate-id>`:
   - Add case in `cmd/claude-profiles/main.go` next to `_delegate-bg-dispatch` / `_delegate-bg-watcher`.
   - New function `cmdDelegateLinkScanPath` in `delegate_bg.go`. Reads `bg-session-id.txt`, polls `readBgJobState` until `LinkScanPath != ""` (or 30s timeout — write a clear error to stderr and exit non-zero).
   - Fail mode: when the dispatcher already wrote `dispatch-error.md` (no bg-session-id.txt to point at), print nothing and exit non-zero with a useful stderr message.
4. Extend `delegateSlashCommand` markdown with an *optional* "Live progress (advanced)" section that shows the `_delegate-jsonl` + `tail -F` + Monitor recipe. Phrase it as "skip unless you specifically want to watch tools fire" — most delegates don't need this.
5. Extend `scripts/smoke-delegate-bg.sh` with a new step: dispatch, call `_delegate-jsonl`, assert the returned path matches `state.LinkScanPath`. The stub's state.json already populates `linkScanPath`, so the smoke covers the happy path.
6. Run all smoke tests before pushing:
   - `./scripts/smoke-delegate-bg.sh`
   - `./scripts/smoke-delegate-flags.sh`
   - `./scripts/smoke-distill.sh`
   - `./scripts/smoke-ui.sh` (note: pre-existing `TestHubFilterByTyping` flake on `main`)
7. Always create the PR as **draft**.

## Hot spots not to forget

- `bg-session-id.txt` may not exist yet when the user invokes `_delegate-jsonl` (the dispatcher writes it after `claude --bg` returns the bg id). Polling for the file itself, then for `linkScanPath` inside the state file, is two waits — make sure the timeout budget covers both.
- `state.json.linkScanPath` is the absolute path that the supervisor wrote — already canonical, don't try to re-resolve it.
- Monitor's stdout-per-event model is the contract: pipe through `grep --line-buffered` or `jq --unbuffered` to avoid pipe buffering swallowing events.
- The new subcommand should NEVER write to the delegate dir — it's a pure read. Writing would invite races with the watcher (which still calls `claude stop`).

## When to delete this file

- **Feature complete** = `_delegate-jsonl` subcommand shipped, smoke covers it, slash-command markdown documents the Monitor recipe. Delete and add a final invariant to CLAUDE.md if any non-obvious gotchas remain (likely the polling timeout budget and the "never write from this subcommand" rule).
- **Dropped** = decision to leave delegates strictly headless. Delete and (optionally) note in CLAUDE.md why, so future agents don't reopen the question.
