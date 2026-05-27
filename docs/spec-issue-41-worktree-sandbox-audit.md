# Spec: Worktree sandbox write allowlist audit (Issue #41)

## Background

Claude Code v2.1.149 fixed a sandbox write allowlist bug that caused the OS-level sandbox
inside a git worktree session to cover the **entire main repository root** instead of only
the shared `.git` directory (with `hooks/` and `config` denied). Before this fix, any tool
call inside a worktree session that slipped past `cmdHookGuardWorktreeWrites` had no OS
backstop — the sandbox was whitelisting the same paths the application-layer hook was trying
to block.

claude-profiles is built around git-worktree isolation (`_worktree: true` profiles run in
`.claude/worktrees/<name>/`). This is precisely the scenario the bug affected.

## What has already been done (this PR)

- `claude-profiles doctor` now emits a `WARN` row when the active Claude Code version is
  below v2.1.149 and any `_worktree` profile is configured (`checkVersionConstraints()` in
  `doctor.go`).
- `CLAUDE.md` documents v2.1.149 as the minimum for correct worktree isolation under
  "Non-obvious invariants".
- `README.md` version table includes the v2.1.149 row.

## Remaining work: write-path audit

The issue asked for an audit of `cmdDelegateBgDispatch`, `cmdHookStop` (`distill.go`), and
`cmdHookPromptSubmit` for writes that go to main-repo paths from a worktree context. A
preliminary read found:

- `distill.go` writes only to `~/.claude-profiles/last-distill/` (safe — under `$HOME`).
- `cmdHookPromptSubmit` writes `delivered.txt` / `delivered-error.md` under
  `~/.claude-profiles/delegates/` (safe — under `$HOME`).
- `cmdDelegateBgDispatch` writes under `~/.claude-profiles/delegates/` (safe).
- `run.go` hook setup writes to `~/.claude-profiles/claude-profiles/hooks/hooks.json` (safe).

**Preliminary verdict: no write crosses the worktree boundary from claude-profiles itself.**
All delegate/hook writes go through `$HOME`-relative paths that are outside any git worktree.

The residual risk is:
1. A future change that adds a write relative to `req.Dir` or `os.Getwd()` inside a worktree
   session context — this could accidentally target the main repo root on pre-v2.1.149 hosts.
2. MCP servers configured in a profile writing relative to the worktree's parent.

## Proposed follow-up

### 1. Smoke test (blocked: requires worktree setup)

Extend `scripts/smoke-delegate-bg.sh` (or add `scripts/smoke-worktree-isolation.sh`) to:
1. Create a real git repo with a linked worktree under `.claude/worktrees/test-wt/`.
2. Launch a mock delegate from inside the worktree that attempts a `Write` to `../main-file.txt`
   (a path outside the worktree, inside the main repo root).
3. Verify `cmdHookGuardWorktreeWrites` blocks the call (non-zero exit from the hook).

### 2. Static guard

Add a compile-time/linter check that any `os.WriteFile` or `os.Create` call inside
`cmdHookStop`, `cmdHookPromptSubmit`, and `cmdDelegateBgDispatch` uses a path that
goes through a `$HOME`-relative helper (`profilesDir()`, `runDirPath()`, etc.) rather
than a raw path construction that could resolve under the main repo root.

This is best done as a comment convention ("// path is $HOME-relative, safe in worktree")
+ a code review checklist item until the codebase is larger and warrants automated checks.

## Acceptance criteria (remaining)

1. Smoke test confirms `cmdHookGuardWorktreeWrites` blocks `../main-file.txt` writes.
2. All new write-path additions go through reviewed `$HOME`-relative helpers or carry an
   explicit "safe in worktree" comment.
3. `claude-profiles doctor` already shows the v2.1.149 warn on older Claude Code — verified
   by manual test or unit test exercising `checkVersionConstraints`.

## Notes

- The doctor check and CLAUDE.md note are the highest-value deliverables here — they
  ensure users on older versions are informed before relying on worktree isolation.
- The full OS-level sandbox guard only applies to Claude Code itself, not to claude-profiles
  code (which runs outside the sandbox). The application-layer hook guard remains the
  primary defense.
