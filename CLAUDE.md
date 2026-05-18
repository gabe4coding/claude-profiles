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

**Session discovery is keyed by the directory claude runs in.**
`sessionDirsToWatch(root)` encodes `root` into `~/.claude/projects/<encoded-root>/`. Any code that spawns claude with a different `cmd.Dir` must pass that directory explicitly to `snapshotSessionFiles`, `announceDelegateJSONLPath`, and `extractLastAssistantMessage` — passing `""` (= `os.Getwd()`) silently scans the wrong project directory and session files are never found.
