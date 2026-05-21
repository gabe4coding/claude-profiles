# Spec: SubagentModel for bg delegate profiles (Issue #18)

## Context

Claude Code v2.1.146 fixed `CLAUDE_CODE_SUBAGENT_MODEL` not being forwarded to child
processes in multi-agent sessions. Before that fix, setting the env var only affected
the top-level process; child processes spawned via `claude --bg` or as subagents did
not inherit it.

`cmdDelegateBgDispatch` already controls the **delegate's own model** via the profile's
`_settings.model` field (and branches on `"haiku"` for permission mode). But the model
used by **subagents the delegate itself spawns** was previously uncontrollable. With the
v2.1.146 fix, injecting `CLAUDE_CODE_SUBAGENT_MODEL` into the delegate's subprocess env
now reliably pins the model for any second-level subagents.

## Scope

Target: `cmdDelegateBgDispatch` in `delegate_bg.go` (the surviving `--bg` path).
Explicitly excluded: `cmdDelegateRunner` (tmux path, scheduled for Atto III demolition).

This is a power-user feature. Only users who dispatch delegates that themselves
orchestrate further multi-agent work benefit from it.

## Proposed changes

### 1. `ProfilePrefs` — new field

```go
type ProfilePrefs struct {
    // existing fields...

    // SubagentModel, when non-empty, injects CLAUDE_CODE_SUBAGENT_MODEL into
    // the bg delegate's subprocess environment (v2.1.146+). Controls the model
    // used by any subagents the delegate itself spawns — distinct from the
    // delegate's own model, which is set via _settings.model. Requires
    // Claude Code >= v2.1.146; silently a no-op on older versions.
    SubagentModel string `json:"subagent_model,omitempty"`
}
```

JSON tag is `subagent_model` (no leading underscore — it's a prefs-only field,
not in the profile.json schema, so the "user metadata vs. profile content" convention
doesn't apply here).

### 2. `cmdDelegateBgDispatch` — env injection

After the `cmd.Env` is assembled:

```go
cmd.Env = append(os.Environ(), "CLAUDE_PROFILES_DELEGATE=1")
prefs := loadProfilePrefs(filepath.Dir(loc.JSONPath))
if prefs.SubagentModel != "" {
    cmd.Env = append(cmd.Env, "CLAUDE_CODE_SUBAGENT_MODEL="+prefs.SubagentModel)
}
```

Note: `loadProfilePrefs` is already called earlier in `cmdDelegateBgDispatch` (for the
`Disabled` check). Consolidate to a single call at the top of the function to avoid
two disk reads.

### 3. Round-trip through `saveProfilePrefs` / `saveProfileAt`

`saveProfileAt` currently round-trips a fixed set of fields (Description, Isolated,
Disabled, Worktree, Prompts, Cwd, Distill). `SubagentModel` must be added to the
`ProfilePrefs` literal in `saveProfileAt` to survive a write-through:

```go
return saveProfilePrefs(dir, ProfilePrefs{
    // existing fields...
    SubagentModel: p.SubagentModel, // propagate from Profile if it ever gains a field
})
```

Since `Profile` struct doesn't expose `SubagentModel` (it's a prefs-only concern),
the round-trip should preserve the existing prefs value:

```go
existingPrefs := loadProfilePrefs(dir)
return saveProfilePrefs(dir, ProfilePrefs{
    // existing fields...
    Disabled:      existingPrefs.Disabled,      // already preserved
    SubagentModel: existingPrefs.SubagentModel, // add this
})
```

### 4. Hub / edit flow (optional, deferred)

The hub's profile edit menu (`cmdEditProfile`) doesn't need to expose `SubagentModel`
in the initial implementation — it's a power-user field that advanced users can set
directly in `profile-prefs.json`. A follow-up can add a field in the edit TUI if demand
warrants it.

## Acceptance criteria

1. A profile with `"subagent_model": "claude-haiku-4-5-20251001"` in its
   `profile-prefs.json` entry causes `cmdDelegateBgDispatch` to pass
   `CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-4-5-20251001` in the subprocess env.
2. A profile with no `subagent_model` key passes no `CLAUDE_CODE_SUBAGENT_MODEL`
   (the parent's env value, if any, is still inherited via `os.Environ()`).
3. `saveProfileAt` preserves the existing `SubagentModel` value across a write
   (same pattern as `Disabled`).
4. Smoke test: extend `scripts/smoke-delegate-bg.sh` with a step that:
   - Creates a prefs entry with `"subagent_model": "claude-haiku-4-5-20251001"`.
   - Dispatches a delegate using that profile.
   - Asserts `bg-args.log` (or the stub's env) shows
     `CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-4-5-20251001` was set.
   (The stub `claude` binary would need to dump its env to a file for assertion (3).)

## Implementation plan

1. Add `SubagentModel string \`json:"subagent_model,omitempty"\`` to `ProfilePrefs`
   in `profile.go`.
2. In `cmdDelegateBgDispatch` (`delegate_bg.go`): hoist the `loadProfilePrefs` call
   to the top, inject env var when `SubagentModel != ""`.
3. In `saveProfileAt` (`profile.go`): preserve `existingPrefs.SubagentModel` in the
   saved struct.
4. Extend `scripts/smoke-delegate-bg.sh` with a Step 6 (env injection assertion).
5. Unit test: extend `delegate_bg_test.go` or add a new test for the env injection
   (mock exec or inspect the built `cmd.Env`).

## Dependencies / ordering

- Must not be implemented on `cmdDelegateRunner` (tmux path) — that code is
  scheduled for Atto III demolition.
- Safe to implement any time after Atto I ships; does not block Atto II/III.
- Requires Claude Code >= v2.1.146 on the user's machine to take effect; older
  versions silently ignore `CLAUDE_CODE_SUBAGENT_MODEL` in child processes.
