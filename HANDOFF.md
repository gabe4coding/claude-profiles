# HANDOFF: Issues #27/#28 implemented; enhancements #29/#34 specced; #33/#35 triaged as noise

Issues #27 and #28 (doctor version checks + dispatch-error message) are now implemented
and tested. Issues #29 and #34 are enhancement specs waiting for a PR with the "enhance"
label. Issues #33 and #35 were triaged as noise (false assumptions vs. codebase).

## What's done

### Issues #27 + #28 — doctor version checks (IMPLEMENTED)
- Added `parseClaudeVersion` + `versionAtLeast` helpers to `doctor.go`.
- Added `knownBadVersions` slice (v2.1.147 flagged as known-bad: Bash tool exit-127).
- Added `minDelegateMaj/Min/Pat` constants (v2.1.146 minimum for reliable delegates).
- Extended `checkClaudeBinary()` to warn on <v2.1.146 and fail on known-bad releases.
- Updated `dispatch-error.md` watcher timeout message to list memory pressure,
  permission re-prompting, and upgrade hint as possible causes.
- Updated README `Requirements` section with explicit v2.1.148 recommended / v2.1.147
  known-bad / v2.1.146 minimum notes.
- Added `doctor_test.go` with unit tests for `parseClaudeVersion`, `versionAtLeast`,
  and the known-bad/min-version logic.

### Issue #29 — memory-pressure shedding (PARTIAL: unconditional parts done)
- Added CLAUDE.md invariant note: "Delegate sessions are non-pinned and can be shed
  under memory pressure (v2.1.147+)."
- The `dispatch-error.md` message update (already done above) also covers the memory
  pressure hint.
- **Open**: investigate whether `claude --bg` supports `--pin` or `--priority` flag
  (requires a live Claude Code binary). Spec at `docs/spec-issue-29-memory-pressure-shedding.md`.

### Issues #33/#35 — triaged as noise
- **#33** (v2.1.149 sandbox write tightening): false alarm. All claude-profiles writes
  go to `~/.claude-profiles/` (home-relative), never to the repo's `.claude/`. The
  sandbox change is scoped to worktree→main-repo-root writes, which claude-profiles
  does not do. Closed with explanation.
- **#35** (v2.1.149 cd/pushd/popd permission tracking): not applicable. The only `cd`
  in hook/script code is `REPO_DIR=$(cd "$2" && pwd)` in `delegate.go` — a subshell
  with no persistent cwd change and no subsequent path-sensitive writes. The main.go
  `cd` runs via tmux/exec.Command, outside Claude's Bash tool entirely. Closed with
  explanation.

### Issue #34 — /usage subagent cost visibility (SPECCED, not yet implemented)
- Spec at `docs/spec-issue-34-usage-subagent-cost.md`.
- Immediate items: README delegate section note + `doctor.go` v2.1.149 advisory.
- Medium-term: investigate machine-readable usage data for per-profile cost TUI.

## Open items

### Issue #29 — investigate `--pin` flag
**Action**: run `claude --bg --help` on a live system. Check for `--pin`, `--priority`,
or analogous flag. If found, add to `cmdDelegateBgDispatch` and add a doctor version
gate. Document result in `docs/spec-issue-29-memory-pressure-shedding.md`.

### Issue #34 — implement the immediate doc + doctor changes
**Spec**: `docs/spec-issue-34-usage-subagent-cost.md`

Both items are small:
1. Add a cost/limits note to the README `/delegate` section.
2. Add a v2.1.149 advisory to `checkClaudeBinary()` in `doctor.go`.
3. Add test cases in `doctor_test.go`.

## Suggested order

1. Implement #34 immediate items (30 min, no investigation needed).
2. Investigate `--pin` flag, then finish #29 programmatic-pinning branch if the flag
   exists.
