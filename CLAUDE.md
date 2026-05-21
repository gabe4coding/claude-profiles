# claude-profiles

## Build & install
```
go build -o ~/.local/bin/claude-profiles ./cmd/claude-profiles/
```

Remote install (repo is private — `GOPRIVATE` required):
```
GOPRIVATE=github.com/gabe4coding/claude-profiles go install github.com/gabe4coding/claude-profiles/cmd/claude-profiles@latest
```

## Development workflow

Every change ships with a working smoke test. If the affected surface has no test harness, add (or extend) a runnable script under `scripts/` — e.g. `./scripts/smoke-distill.sh` covers the Stop-hook distill filters. The script must be re-runnable from a clean checkout, exit non-zero on regression, and clean up its temp state. If a meaningful smoke test would require disproportionate setup (sandboxed network, real Claude session, manual user interaction), tell the user explicitly rather than skipping verification.

## Session handoffs

If a file named `HANDOFF.md` exists at the repo root at the start of a session, that's pending work from a previous session that may or may not be relevant to the current task. Before doing anything else:

1. Read `HANDOFF.md` (it's designed to be self-contained — the first paragraph is a one-sentence summary of the pending work).
2. Ask the user, in one question, whether to continue from the handoff. Quote the one-sentence summary verbatim so the user knows what they're agreeing to.
3. If yes: follow the handoff. Resume from the open items listed there.
4. If no: ask whether to **delete `HANDOFF.md`** now, and on what grounds — `feature complete` (the pending work shipped end-to-end since the handoff was written) or `dropped` (the user is abandoning that work). Delete the file on the user's confirmation. Do not delete it without an explicit "complete" or "dropped" — leaving it in place is the safe default if the user is unsure.

When you finish a piece of work that should survive a context reset (you completed an act but more acts remain; or you paused mid-feature), write or update `HANDOFF.md` yourself before the session ends. Keep the one-sentence summary at the top accurate; future sessions will quote it back to the user verbatim.

## Non-obvious invariants

**Profile prefs keys are main-repo absolute paths.**
`~/.claude-profiles/profile-prefs.json` is keyed by the profile directory's absolute path in the *main* working tree. When code runs from inside a git worktree the profile path goes through `.claude/worktrees/<name>/`, so any lookup or write against the prefs store must pass through `canonicalProfileDir()` first. Forgetting this silently drops all user prefs (distill, isolated, cwd, etc.) in worktree sessions.

**`wrapperPluginHooksJSON` is idempotent by exact string match.**
`ensureWrapperPluginHooks()` only rewrites `hooks.json` when the file content differs from the constant. Changes to the hook list take effect on the next `cmdRun` startup — already-running sessions loaded the old hooks at launch and won't see the update.

**`Profile.Cwd` is hub-filtering only — not a delegate working directory.**
It controls which profiles appear in the hub when `listAllLocations()` runs. The delegate dispatcher (`cmdDelegateBgDispatch`) never reads it. The delegate's working directory comes from `delegateRequest.Dir`, set via `/delegate --dir <path>` at call time and resolved to absolute in the launch script.

**`ProfileLocation.OwnerRepo` is hub-hint only, not enforced at dispatch.**
Set to the *main-tree* path of the repo whose `.claude-profiles/` the profile lives in — canonicalise with `mainRepoRoot()`, not `canonicalProfileDir()` (the latter doesn't handle bare worktree roots). User-level profiles and `alias/name` remote profiles have `OwnerRepo=""`. Used by `listAllLocations` to group profiles per-repo in the hub. **Atto III stopped enforcing it at dispatch** — `cmdDelegateBgDispatch` no longer rejects bound profiles invoked on the "wrong" repo. If a profile-discovery source forgets to set `OwnerRepo` the only consequence is hub mis-grouping; the workspace boundary is no longer machine-enforced. If you reintroduce enforcement, check `req.Dir` directly inside `cmdDelegateBgDispatch` — do not reach back into the deleted tmux runner.

**`ProfilePrefs.Disabled` uses JSON key `_hidden` — intentional mismatch.**
The Go field is `Disabled` but the on-disk JSON tag is `_hidden` to stay compatible with existing `profile-prefs.json` files without a migration. Do not "fix" the alignment: renaming the tag to `_disabled` silently clears every user's disabled-profile state on first write.

**Embedded `distill.md` and active `~/.claude-profiles/distill.md` are two copies.**
`ensureDistillProcedureFile` writes the embedded default only when the on-disk file is absent — user hand-edits survive `claude-profiles upgrade` by design. Edits to `cmd/claude-profiles/distill.md` therefore don't reach the running hook until you either delete the on-disk copy or overwrite it. Before overwriting, diff against the previous embedded version (`git show HEAD:cmd/claude-profiles/distill.md`) to confirm the active copy has no user edits to preserve.

**Distill bookmark advances on block emission, not on completion.**
`saveLastDistillBookmark` writes the current HEAD SHA to `~/.claude-profiles/last-distill/<safeID>.json` *before* the Stop hook emits its `decision:block`, not after Claude finishes the distillation. Consequence: if Claude crashes, skips, or no-ops the distillation, the next hook still treats those commits as "already prompted" and won't re-fire until a new commit lands. This is the deliberate tradeoff — guaranteed no duplicate prompts on the same work, at the cost of rare missed distillations. If you want post-completion bookmarking, you need a second hook firing after the distill-block run completes; the current Stop hook has no signal that the next turn succeeded.

**`/delegate` is bg-only; the parent hook reads `state.json` directly.**
After Atto III the only execution backend is `cmdDelegateBgDispatch` + `cmdDelegateBgWatcher`. The dispatcher writes `bg-session-id.txt` next to `request.json`, and the watcher's *only* job is calling `claude stop <bg-id>` once the session reaches a terminal state — it does not write any result file. The parent's `cmdHookPromptSubmit` walks each delegate dir on every `UserPromptSubmit`, reads `~/.claude/jobs/<bg-id>/state.json` directly (via `collectDelegateForInjection`), extracts the assistant text from `linkScanPath`, injects it as `additionalContext`, and writes a `delivered.txt` marker. Subsequent prompts skip the delegate because of that marker — do not delete it.

**Dispatch failures use a separate file: `dispatch-error.md`.**
When `cmdDelegateBgDispatch` aborts before `bg-session-id.txt` is written (profile not found, disabled, `claude` not on PATH, watcher spawn failed) it calls `writeDispatchError` to leave `dispatch-error.md` beside `request.json`. The hook handles it the same way as a successful delivery but renames to `delivered-error.md` instead of writing `delivered.txt`. The bg watcher uses the same file on timeout. **Do not** overload `result.md`-style files for two different shapes — the success path goes through `state.json`, the error path through `dispatch-error.md`. Keeping these distinct is what lets the hook decide injection type without parsing content.

**`CLAUDE_PROFILES_DELEGATE=1` is the delegate-side guard for `cmdHookStop`.**
`cmdDelegateBgDispatch` sets it on the delegate subprocess env. `cmdHookStop` (`distill.go`) bails as soon as it sees this env var — this is what stops a *committing* delegate from advancing the *parent profile's* distill bookmark (the delegate inherits `CLAUDE_PROFILES_WRAPPER_PID` from os.Environ, so without this guard `wrapperContextForHook` would resolve the parent's profile). Keep this guard load-bearing: if you change how `cmd.Env` is built in the dispatcher, make sure `CLAUDE_PROFILES_DELEGATE=1` survives.
