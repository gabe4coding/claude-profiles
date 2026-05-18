# Session Distillation Procedure

At session end, scan for non-obvious facts that should be **shared or committed** — facts native auto-memory won't route on its own.

**Background:** Claude Code's native auto-memory (`~/.claude/projects/<project>/memory/`) already captures stale-tolerant insights it noticed during the session — build commands, debug tricks, preferences, project state. Do **not** write to auto-memory from this hook; trust the native system. This procedure handles only the destinations auto-memory can't pick for you.

Pick at most one destination per fact:

**1. `./CLAUDE.md` (or `./.claude/CLAUDE.md`)** — committed, team-shared
Use for slow-rotting facts every contributor needs: build/test commands, architecture, workflows ("run X before commit"), conventions. ≤5 lines per insertion. Skip if derivable from reading the code.

**2. `./.claude/rules/<topic>.md` with `paths:` frontmatter** — committed, conditionally loaded
Use for conventions that only apply to a subtree or file type (API handlers, tests, migrations). Saves context vs bloating CLAUDE.md.

**3. `./CLAUDE.local.md`** — gitignored, personal-in-this-repo
Use for personal preferences that shouldn't be committed: sandbox URLs, your test data, local shortcuts.

**4. `~/.claude/CLAUDE.md` or `~/.claude/rules/<topic>.md`** — personal, all-repos
Use for cross-project preferences (always pnpm not npm, your test-data conventions).

**Rules**
- One destination per fact. If unsure, prefer the more local / less-shared surface.
- Skip anything derivable from reading the code.
- **Read the target file before writing.** If a related entry exists, update it in place — do not append a parallel near-duplicate. If your new fact contradicts an existing entry, replace it (and verify which is current); don't leave both.
- Prefer minimal edits over insertions. Two near-duplicate lines is the rot path this hook exists to prevent.
- End with a one-line summary of files written (e.g. `→ wrote ./CLAUDE.md (uncommitted)`) so the user sees what changed.
- If nothing surfaced that fits any of (1)–(4), say so in one line and stop.

Brevity is the distillation — verbose notes are the failure mode this hook prevents.
