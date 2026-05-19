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

## Non-obvious invariants

**Profile prefs keys are main-repo absolute paths.**
`~/.claude-profiles/profile-prefs.json` is keyed by the profile directory's absolute path in the *main* working tree. When code runs from inside a git worktree the profile path goes through `.claude/worktrees/<name>/`, so any lookup or write against the prefs store must pass through `canonicalProfileDir()` first. Forgetting this silently drops all user prefs (distill, isolated, cwd, etc.) in worktree sessions.

**`wrapperPluginHooksJSON` is idempotent by exact string match.**
`ensureWrapperPluginHooks()` only rewrites `hooks.json` when the file content differs from the constant. Changes to the hook list take effect on the next `cmdRun` startup — already-running sessions loaded the old hooks at launch and won't see the update.

**`Profile.Cwd` is hub-filtering only — not a delegate working directory.**
It controls which profiles appear in the hub when `listAllLocations()` runs. `cmdDelegateRunner` never reads it. The delegate's working directory comes from `delegateRequest.Dir`, set via `/delegate --dir <path>` at call time and resolved to absolute in the launch script (before the runner's tmux window inherits a different cwd).

**Delegate worktree name is deterministic — `--worktree delegate-<delegateID>`.**
`cmdDelegateRunner` suppresses `claudeFlags`' auto `--worktree` (which lets Claude pick a random slug like `cryptic-sauteeing-mountain`) and passes an explicit name instead. That makes the session's encoded project dir fully predictable — `computeExpectedSessionsDir(workingDir, p.Worktree, worktreeName)` returns the exact `~/.claude/projects/` subdir Claude will write to, and the runner snapshots only that single dir before/after. If you ever revert to auto-naming, restore the global-scan fallback (git history before this commit) or session discovery will silently break for worktree delegates.

**`result.md` is owned by `writeResultOnFirstTurnComplete`, not the post-run fallback.**
The first-turn watcher writes `result.md` via the absolute path in `jsonl-path.txt`. `cmdDelegateRunner`'s post-run code must `os.Stat(resultPath)` and bail out if it exists — otherwise the fallback overwrites a good result with `(delegate exited without a final assistant reply)`. When the watcher did not write, find the session via `jsonl-path.txt` first and call `extractLastAssistantFromFile`; only fall through to `snapshotJSONLInDir(expectedSessionsDir)` against the pre-launch snapshot if `jsonl-path.txt` is missing.

**`ProfileLocation.OwnerRepo` enforces the workspace binding for delegates.**
Set to the *main-tree* path of the repo whose `.claude-profiles/` the profile lives in — canonicalise with `mainRepoRoot()`, not `canonicalProfileDir()` (the latter doesn't handle bare worktree roots). User-level profiles and `alias/name` remote profiles have `OwnerRepo=""` (usable on any `--dir`). `cmdDelegateRunner` enforces the binding only when `p.Worktree && OwnerRepo != ""`, so any new profile discovery source that forgets to set `OwnerRepo` silently makes its profiles usable on any repo — defeating the workspace boundary.

**`ProfilePrefs.Disabled` uses JSON key `_hidden` — intentional mismatch.**
The Go field is `Disabled` but the on-disk JSON tag is `_hidden` to stay compatible with existing `profile-prefs.json` files without a migration. Do not "fix" the alignment: renaming the tag to `_disabled` silently clears every user's disabled-profile state on first write.

**Embedded `distill.md` and active `~/.claude-profiles/distill.md` are two copies.**
`ensureDistillProcedureFile` writes the embedded default only when the on-disk file is absent — user hand-edits survive `claude-profiles upgrade` by design. Edits to `cmd/claude-profiles/distill.md` therefore don't reach the running hook until you either delete the on-disk copy or overwrite it. Before overwriting, diff against the previous embedded version (`git show HEAD:cmd/claude-profiles/distill.md`) to confirm the active copy has no user edits to preserve.

**Distill bookmark advances on block emission, not on completion.**
`saveLastDistillBookmark` writes the current HEAD SHA to `~/.claude-profiles/last-distill/<safeID>.json` *before* the Stop hook emits its `decision:block`, not after Claude finishes the distillation. Consequence: if Claude crashes, skips, or no-ops the distillation, the next hook still treats those commits as "already prompted" and won't re-fire until a new commit lands. This is the deliberate tradeoff — guaranteed no duplicate prompts on the same work, at the cost of rare missed distillations. If you want post-completion bookmarking, you need a second hook firing after the distill-block run completes; the current Stop hook has no signal that the next turn succeeded.
