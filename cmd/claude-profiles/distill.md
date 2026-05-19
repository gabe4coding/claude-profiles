# Session Distillation Procedure

This hook fires at Stop on sessions that produced at least one commit touching files outside `.claude/`. You're catching facts you re-discovered or were corrected on **this session** — not retrospective fishing through history. **Surfacing nothing is a valid outcome.** Most sessions should produce zero entries.

## Step 1 — Pick at most ONE candidate

From this session, pick the single most valuable non-obvious fact a teammate would call out in code review. If you can't pick just one, you don't have one — stop.

**Disqualifiers (skip the candidate if ANY apply):**

- **Derivable from reading the code or `git log`** → already in the codebase, no note needed.
- **Stale-tolerant** (build commands, debug tricks, current project state) → trust auto-memory at `~/.claude/projects/<project>/memory/`. **Do not write to auto-memory from this hook.**
- **Survival test fails**: "If this note were removed, would a future maintainer hit the bug it describes?" If no → skip.
- **Obvious from well-named identifiers** or directory layout.
- **What-not-why**: describes what code does instead of why it has to be that way.

What's left is what auto-memory won't route on its own: facts that should be **in context for anyone** (human or AI) entering this project — instructions, conventions, invariants a teammate would flag in review.

## Step 2 — Pick the destination

The 2D mental model: **audience** (team vs me) × **scope** (everywhere vs subtree/filetype).

**1. `./CLAUDE.md`** — committed, team-shared, always loaded.
Project-wide facts every contributor needs: build/test commands, architecture, workflows, non-obvious invariants. ≤5 lines per insertion. Default for team-shared knowledge; split into rules (2) only once CLAUDE.md grows past ~200 lines or a fact only applies to a subtree.

**2. `./.claude/rules/<topic>.md` with `paths:` frontmatter** — committed, conditionally loaded.
Conventions that only apply to a subtree or filetype (API handlers, tests, migrations). Loads only when Claude reads matching files. The `paths:` frontmatter is what earns the slot here; a rules file without `paths:` is just CLAUDE.md split into topics and doesn't belong as a separate destination.

**3. `./CLAUDE.local.md`** — gitignored, personal-in-this-repo.
Personal preferences not for the team: sandbox URLs, your test data, local shortcuts.
*Worktree caveat:* this file lives only in the worktree that created it; sibling worktrees won't see it. For preferences that should follow you across worktrees of this repo, either use destination 4 or keep the content in `~/.claude/<file>.md` and `@import` it from CLAUDE.local.md in each worktree.

**4. `~/.claude/CLAUDE.md` or `~/.claude/rules/<topic>.md`** — personal, all repos.
Cross-project preferences (always pnpm not npm, your test-data conventions). Use a path-scoped rule under `~/.claude/rules/` if the preference only applies to specific filetypes across all your repos.

## Step 3 — Draft the entry

Stay under ~80 words. Format:

```
**<one-line invariant statement, ending in a period>.**
<2-4 sentences: the symptom you'd see if this is ignored, the underlying reason, the safe action.>
```

The bold opener is the searchable name. The body is the why and the consequence.

## Step 4 — Self-review before writing

Open the target file. Then run your draft through this checklist, in order. **Any "no" means revise or discard.**

1. **Anti-duplicate**: is there already a near-duplicate entry in the target file (in spirit, not just verbatim)? If yes → update in place; do not append. If your new fact contradicts an existing entry, replace it (verify which is current) — don't leave both.
2. **Survival test**: would a future maintainer hit the bug if this note were removed?
3. **Why-not-what**: does it explain a hidden constraint, invariant, or non-obvious cause — not narrate what the code does?
4. **Scope match**: is the destination from Step 2 still right given the actual audience and reach?
5. **Format**: symptom + invariant + consequence, ≤80 words, bold opener?

If anything fails → revise once. If it still fails → discard and stop.

## Step 5 — Write or stop

- **Kept**: write the single edit, then emit a one-line summary (e.g. `→ wrote ./CLAUDE.md`).
- **Discarded** (or nothing surfaced): say so in one line (e.g. `→ nothing distillation-worthy from this session`) and stop.

## Examples

**GOOD** — names symptom + reason + consequence, survives refactor:

> **Profile prefs keys are main-repo absolute paths.** `~/.claude-profiles/profile-prefs.json` is keyed by the profile directory's absolute path in the *main* working tree. When code runs from inside a git worktree the profile path goes through `.claude/worktrees/<name>/`, so any lookup or write against the prefs store must pass through `canonicalProfileDir()` first. Forgetting this silently drops all user prefs in worktree sessions.

**BAD** — too generic, restates common practice:

> Use git for version control. Commit often with meaningful messages.

**BAD** — what-not-why, will rot at next refactor:

> The function `cmdHookStop` reads stdin and calls `wrapperContextForHook`. Used by the wrapper plugin.

**BAD** — obvious from the code on first read:

> `ensureDistillProcedureFile` writes the embedded default to disk only when the file is absent.

**BAD** — stale-tolerant, belongs in auto-memory not a distilled invariant:

> Build with `go build ./cmd/claude-profiles/`. Run tests with `go test ./...`.

Brevity is the distillation. Verbose, plausible-sounding notes are the failure mode this hook exists to prevent.
