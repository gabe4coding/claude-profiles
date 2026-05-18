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
