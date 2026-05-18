# Session Distillation Procedure

This hook fires at Stop on sessions that touched non-`.claude/` files. You're catching facts you re-discovered or were corrected on **this session** — not retrospective fishing through history.

## Pre-filters — apply before choosing a destination

1. **Derivable from reading the code or `git log`?** → skip. Don't restate what the codebase already shows.
2. **A learning Claude would keep for itself?** → trust auto-memory at `~/.claude/projects/<project>/memory/`. It captures stale-tolerant insights (build commands, debug tricks, project state) and decides what's worth writing. **Do not write to auto-memory from this hook.**

What's left is what auto-memory won't route on its own: facts that should be **in context for anyone** (human or AI) entering this project — instructions, conventions, invariants a teammate would call out in code review.

## Pick one destination per fact

The 2D mental model: **audience** (team vs me) × **scope** (everywhere vs subtree/filetype).

**1. `./CLAUDE.md`** — committed, team-shared, always loaded.
Project-wide facts every contributor needs: build/test commands, architecture, workflows ("run X before commit"), non-obvious invariants. ≤5 lines per insertion. This is the default for team-shared knowledge; only split into rules (2) once CLAUDE.md grows past ~200 lines or a fact only applies to a subtree.

**2. `./.claude/rules/<topic>.md` with `paths:` frontmatter** — committed, conditionally loaded.
Conventions that only apply to a subtree or filetype (API handlers, tests, migrations). Loads only when Claude reads matching files — saves context vs bloating CLAUDE.md. The `paths:` frontmatter is what earns the slot here; a rules file without `paths:` is just CLAUDE.md split into topics and doesn't belong as a separate distillation choice.

**3. `./CLAUDE.local.md`** — gitignored, personal-in-this-repo.
Personal preferences not for the team: sandbox URLs, your test data, local shortcuts.
*Worktree caveat:* this file lives only in the worktree that created it; sibling worktrees won't see it. For preferences that should follow you across worktrees of this repo, either use destination 4 or keep the content in `~/.claude/<file>.md` and `@import` it from CLAUDE.local.md in each worktree.

**4. `~/.claude/CLAUDE.md` or `~/.claude/rules/<topic>.md`** — personal, all repos.
Cross-project preferences (always pnpm not npm, your test-data conventions). Use a path-scoped rule under `~/.claude/rules/` if the preference only applies to specific filetypes across all your repos.

## Rules

- **One destination per fact.** If unsure, prefer the more local / less-shared surface.
- **Read the target file before writing.** If a related entry exists, update it in place — do not append a near-duplicate. If your new fact contradicts an existing entry, replace it (and verify which is current); don't leave both.
- **Prefer minimal edits over insertions.** Two near-duplicate lines is the rot path this hook exists to prevent.
- **End with a one-line summary** of files written (e.g. `→ wrote ./CLAUDE.md (uncommitted)`) so the user sees what changed.
- **If nothing surfaced that fits any of (1)–(4)**, say so in one line and stop.

Brevity is the distillation — verbose notes are the failure mode this hook prevents.
