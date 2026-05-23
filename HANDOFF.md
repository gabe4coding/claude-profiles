# HANDOFF: Issue triage complete — issues #26–#29 fully resolved or specced

All four open issues (#26–#29) have been addressed. Issues #27, #28, and the
unconditional parts of #29 are now implemented; issue #26 is closed.

## What's done

### Issue #26 (closed in previous session, comment to be added)
Version comment bump: SubagentModel minimum-version notes updated from v2.1.146 to
v2.1.147 in `delegate_bg.go`, `CLAUDE.md`, and `docs/spec-issue-18-subagent-model.md`.

### Issues #27 + #28 — `doctor` version checks (IMPLEMENTED)

`cmd/claude-profiles/doctor.go`:
- Added `parseClaudeVersion(s string) (major, minor, patch int, ok bool)` helper.
- Added `versionAtLeast(maj, min, pat, needMaj, needMin, needPat int) bool` helper.
- Added `badVersionRange` type and `knownBadVersions` table (v2.1.147 Bash regression).
- Added `minDelegateMaj/Min/Pat` constants (v2.1.146 minimum).
- Replaced `checkClaudeBinary()` body with version-aware logic:
  - **fail** for known-bad releases (v2.1.147).
  - **warn** for versions below v2.1.146 (permission re-prompting in bg sessions).
  - **ok** for v2.1.148+.

`cmd/claude-profiles/doctor_test.go` (new file):
- `TestParseClaudeVersion` — covers "claude X.Y.Z", bare "X.Y.Z", unparseable.
- `TestVersionAtLeast` — ordering semantics including cross-component comparisons.
- `TestCheckClaudeBinaryVersionLogic` — exercises fail/warn/ok branches without a
  live claude binary.

### Issue #29 — Memory-pressure shedding diagnostics (UNCONDITIONAL PARTS IMPLEMENTED)

`cmd/claude-profiles/delegate_bg.go`:
- Updated watcher timeout `writeDispatchError` message (consolidated hint for issues
  #27, #28, #29): now mentions memory pressure (v2.1.147+ shedding), permission
  re-prompting (upgrade to v2.1.146+), and references `claude-profiles doctor`.

`CLAUDE.md`:
- Added **"Delegate sessions are non-pinned and can be shed under memory pressure
  (v2.1.147+)"** invariant note, documenting the shedding hierarchy, the watcher
  timeout path, and the open pinning-flag question.

`README.md`:
- Updated minimum version note: v2.1.148 recommended (v2.1.147 is known-bad),
  v2.1.146 is the minimum for reliable delegate bg sessions.

## Open items

### Issue #29 — Pinning flag investigation (DEFERRED)

The entire `--pin` / `--priority` branch of spec-issue-29 is still pending. It
requires running `claude --bg --help` on a live system to check for a pinning flag.
If the flag exists, add it to `cmdDelegateBgDispatch` alongside `--permission-mode`.
See `docs/spec-issue-29-memory-pressure-shedding.md` §Proposal 1.

### Issue #29 — Smoke test for mid-run session termination (DEFERRED)

`scripts/smoke-delegate-bg.sh` does not yet cover the kill-based shed simulation.
This requires `bgWatcherAbandonAfter` to be overridable via env (e.g.
`CLAUDE_PROFILES_WATCHER_TIMEOUT`) so the test finishes in a reasonable time.
See `docs/spec-issue-29-memory-pressure-shedding.md` §Proposal 4.
