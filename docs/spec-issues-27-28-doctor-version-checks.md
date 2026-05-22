# Spec: Doctor version checks — permission re-prompt guard & known-bad releases (Issues #27, #28)

## Background

Two separate Claude Code regressions/fixes motivate adding version-aware checks to
`claude-profiles doctor`:

- **Issue #27** — Before v2.1.146, bg sessions re-prompted for tool permissions even
  when "don't ask again" had been granted. Because delegates run in bg sessions, a
  stuck permission prompt inside a delegate would cause the watcher to hit its 30-minute
  timeout and write `dispatch-error.md`. The failure looked like a generic timeout with
  no actionable diagnosis.

- **Issue #28** — v2.1.147 introduced a Bash tool regression (exit code 127 on every
  command) that was fixed in v2.1.148. Any user on exactly v2.1.147 would have seen
  every Bash invocation fail silently (the distill bookmark still advanced because it
  advances before the decision block, per CLAUDE.md invariant).

Both issues share the same root fix: `claude-profiles doctor` should parse the active
`claude --version` and compare it against known thresholds and known-bad releases.

## Affected components

- `cmd/claude-profiles/doctor.go` — `checkClaudeBinary()` (line 86) already reads
  `claude --version` but does not interpret it. Extend with version comparison logic.
- `cmd/claude-profiles/delegate_bg.go` — watcher timeout message (line 375-376).
  Complement with a suggestion to upgrade to v2.1.146+ when a timeout occurs.

## Proposed changes

### 1. Version parsing helper

Add a `parseClaudeVersion(s string) (major, minor, patch int, ok bool)` helper that
handles the `claude X.Y.Z` or `X.Y.Z` format emitted by `claude --version`. Keep it
simple and tolerant of extra words.

```go
func parseClaudeVersion(s string) (major, minor, patch int, ok bool) {
    // "claude 2.1.147" or "2.1.147" — grab the last token that matches \d+\.\d+\.\d+
    for _, tok := range strings.Fields(s) {
        n, _ := fmt.Sscanf(tok, "%d.%d.%d", &major, &minor, &patch)
        if n == 3 {
            return major, minor, patch, true
        }
    }
    return 0, 0, 0, false
}

// versionAtLeast returns true when parsed version (maj, min, pat) >= (needMaj, needMin, needPat).
func versionAtLeast(maj, min, pat, needMaj, needMin, needPat int) bool {
    if maj != needMaj { return maj > needMaj }
    if min != needMin { return min > needMin }
    return pat >= needPat
}
```

### 2. Known-bad version table

Add a package-level slice of known-bad version ranges with a human-readable description:

```go
type badVersionRange struct {
    // Inclusive range [from, to]. Set to == from for a single version.
    fromMaj, fromMin, fromPat int
    toMaj,   toMin,   toPat   int
    description string
}

var knownBadVersions = []badVersionRange{
    {2, 1, 147, 2, 1, 147,
        "Bash tool exits with code 127 on every command — upgrade to v2.1.148+"},
}
```

### 3. Extended `checkClaudeBinary()`

Replace the existing check (which only reports ok/warn for path + version string) with:

```go
func checkClaudeBinary() docCheck {
    path, err := exec.LookPath("claude")
    if err != nil {
        return docCheck{"claude binary", "fail", "not found in PATH"}
    }
    out, err := exec.Command("claude", "--version").Output()
    version := strings.TrimSpace(string(out))
    if err != nil || version == "" {
        return docCheck{"claude binary", "warn", path + " (version unknown)"}
    }

    maj, min, pat, ok := parseClaudeVersion(version)
    if !ok {
        return docCheck{"claude binary", "warn", path + " (version unparseable: " + version + ")"}
    }

    // Check known-bad releases first.
    for _, bad := range knownBadVersions {
        inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
            !versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
        if inRange {
            return docCheck{"claude binary", "fail",
                fmt.Sprintf("%s (%s) — known-bad: %s", path, version, bad.description)}
        }
    }

    // Warn on versions below the minimum reliable delegate baseline.
    const minDelegateMaj, minDelegateMin, minDelegatePat = 2, 1, 146
    if !versionAtLeast(maj, min, pat, minDelegateMaj, minDelegateMin, minDelegatePat) {
        return docCheck{"claude binary", "warn",
            fmt.Sprintf("%s (%s) — below v2.1.146: delegate bg sessions may silently timeout "+
                "due to permission re-prompting; upgrade recommended", path, version)}
    }

    return docCheck{"claude binary", "ok", path + " (" + version + ")"}
}
```

### 4. Watcher timeout message improvement (issue #27)

In `delegate_bg.go` line 375-376, extend the `writeDispatchError` message:

```go
writeDispatchError(dir, fmt.Sprintf(
    "(delegate %s abandoned by bg watcher after %s — session %s stopped; "+
        "possible causes: memory pressure, network failure, or permission re-prompting "+
        "(upgrade to Claude Code v2.1.146+ to rule out the last cause); "+
        "attach with `claude attach %s` if the session might still be useful)",
    delegateID, bgWatcherAbandonAfter, bgID, bgID))
```

### 5. README / `claude-profiles upgrade` note (issue #28)

Add a one-line minimum-version note to the README:

```markdown
> **Minimum Claude Code version**: v2.1.148 recommended (v2.1.147 has a known Bash
> tool regression; v2.1.146 is the minimum for reliable delegate bg sessions).
```

## Acceptance criteria

1. `claude-profiles doctor` on v2.1.147 prints a **fail** row for the claude binary
   with the Bash-tool regression message.
2. `claude-profiles doctor` on v2.1.145 or earlier prints a **warn** row calling out
   permission re-prompting in delegate bg sessions.
3. `claude-profiles doctor` on v2.1.148+ prints an **ok** row for the claude binary.
4. The watcher timeout `dispatch-error.md` message includes the permission re-prompting
   upgrade hint.
5. `go build ./cmd/claude-profiles/` passes with no new lint warnings.

## Implementation plan

1. Add `parseClaudeVersion` + `versionAtLeast` to `doctor.go` (or a new
   `version_check.go` — keep it small, no new file needed given the helper is <20 lines).
2. Add `knownBadVersions` slice to `doctor.go`.
3. Replace the body of `checkClaudeBinary()` with the extended version above.
4. Update the `writeDispatchError` call in `delegate_bg.go` (watcher timeout path).
5. Add the README minimum-version note.
6. Extend `scripts/smoke-delegate-bg.sh` or add a unit test that exercises
   `parseClaudeVersion` with v2.1.147, v2.1.145, v2.1.148 inputs and verifies
   the correct check status is returned.

## Notes

- The version check is **non-fatal at dispatch time** — only `doctor` warns. Adding a
  mandatory version gate to `cmdRun` would break users on air-gapped systems who can't
  easily upgrade; a doctor warning is the right level of friction.
- The `knownBadVersions` table must be easy to extend — the design above makes adding
  future known-bad ranges a one-liner.
- Issues #27 and #28 are consolidated here because they both reduce to "parse
  `claude --version` in `checkClaudeBinary` and compare against thresholds." Keeping
  one canonical version-check path avoids duplicating the parser.
