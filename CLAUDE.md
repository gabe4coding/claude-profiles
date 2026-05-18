# claude-profiles

## Build & install
```
go build -o ~/.local/bin/claude-profiles ./cmd/claude-profiles/
```

## Non-obvious invariants

**Profile prefs keys are main-repo absolute paths.**
`~/.claude-profiles/profile-prefs.json` is keyed by the profile directory's absolute path in the *main* working tree. When code runs from inside a git worktree the profile path goes through `.claude/worktrees/<name>/`, so any lookup or write against the prefs store must pass through `canonicalProfileDir()` first. Forgetting this silently drops all user prefs (distill, isolated, cwd, etc.) in worktree sessions.

**`wrapperPluginHooksJSON` is idempotent by exact string match.**
`ensureWrapperPluginHooks()` only rewrites `hooks.json` when the file content differs from the constant. Changes to the hook list take effect on the next `cmdRun` startup — already-running sessions loaded the old hooks at launch and won't see the update.

**`Profile.Cwd` is hub-filtering only — not a delegate working directory.**
It controls which profiles appear in the hub when `listAllLocations()` runs. `cmdDelegateRunner` never reads it. The delegate's working directory comes from `delegateRequest.Dir`, set via `/delegate --dir <path>` at call time and resolved to absolute in the launch script (before the runner's tmux window inherits a different cwd).

**Session discovery is keyed by the directory claude runs in — but that directory is unpredictable.**
`sessionDirsToWatch(root)` encodes `root` into `~/.claude/projects/<encoded-root>/`. Setting `cmd.Dir` on a subprocess does not guarantee Claude uses that path as its project dir: git root detection, worktree context inherited from the environment, and other internal heuristics can cause Claude to resolve a different effective CWD. The most common trigger is `--worktree` — Claude auto-creates a fresh worktree with a random name (e.g. `cryptic-sauteeing-mountain`) and writes its session under that worktree's encoded project dir, unrelated to `req.Dir`. For delegate session discovery, use `snapshotAllSessionFiles()` (scans every `~/.claude/projects/` subdir) rather than `snapshotSessionFiles(req.Dir)` — the "not-in-before" filter prevents false positives from concurrent sessions.

**`result.md` is owned by `writeResultOnFirstTurnComplete`, not the post-run fallback.**
The first-turn watcher writes `result.md` via the absolute path in `jsonl-path.txt`, so it can find the session regardless of where Claude put it. `cmdDelegateRunner`'s post-run code must `os.Stat(resultPath)` and bail out if it exists — otherwise the fallback overwrites a good result with `(delegate exited without a final assistant reply)` whenever the session lives somewhere `extractLastAssistantMessage(sessionID, req.Dir)` can't reach (e.g. an auto-created worktree). When the watcher did not write, find the session via `jsonl-path.txt` directly and call `extractLastAssistantFromFile` — do not go through `sessionDirsToWatch`.

**`ProfileLocation.OwnerRepo` enforces the workspace binding for delegates.**
Set to the *main-tree* path of the repo whose `.claude-profiles/` the profile lives in — canonicalise with `mainRepoRoot()`, not `canonicalProfileDir()` (the latter doesn't handle bare worktree roots). User-level profiles and `alias/name` remote profiles have `OwnerRepo=""` (usable on any `--dir`). `cmdDelegateRunner` enforces the binding only when `p.Worktree && OwnerRepo != ""`, so any new profile discovery source that forgets to set `OwnerRepo` silently makes its profiles usable on any repo — defeating the workspace boundary.
