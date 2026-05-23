---
name: memory
description: Use when a transcript Stop or commit event reveals durable BEHAVIORAL context that should shape how Claude acts in future sessions — user preferences, project rules, validated patterns, references to external systems. Decides among four surfaces (auto-memory, CLAUDE.md at user/project/local scope, .claude/rules/) and writes the right one.
---

# Memory skill — route durable context to the right surface

You receive one inbox event (already loaded by the orchestrator). Decide whether it changes how Claude should ACT in future sessions, and if so, write or update the right file on the right surface. Many surfaces exist; picking the wrong one is worse than picking nothing.

## How this differs from the kb skill

- **kb** is *historical record* — "this decision was made, this bug was fixed, here is the story". Read manually.
- **memory** is *behavioral context* — context that auto-loads into future sessions and shapes Claude's responses without being asked.

Decisions/fixes/sessions go to kb. Preferences, project rules, project facts, external pointers go to memory. Most events route to neither. Some route to both.

## Filter 0 — codebase-can-tell-it

Before considering any surface, verify the fact is **not already encoded** in:
- `./CLAUDE.md`, `./CLAUDE.local.md`, `~/.claude/CLAUDE.md` (already loaded in your context),
- an existing memory topic file (check `MEMORY.md` index — already loaded),
- a path-scoped `./.claude/rules/<topic>.md`,
- the source code, tests, `git log`, or commit message body of the cited event.

If any of these already encodes the fact: prefer `Edit` on the existing file to refine it. If the existing encoding is sufficient: `memory: skipped`. The memory surfaces capture only what the codebase and existing memory cannot already tell future-Claude.

## The five memory surfaces

Claude Code loads multiple files at session start. Each has a different audience, lifetime, and scope. Pick the one whose properties match the event.

### 1. Auto-memory — `~/.claude/projects/<encoded-project>/memory/`

- **Audience**: future Claude on this machine, this repo (shared across worktrees).
- **Written by**: Claude itself. This skill's default surface.
- **Loaded**: `MEMORY.md` (first 200 lines / 25KB) at every session start. Topic files are read on demand.
- **Use for**: anything you would write down as a personal note about *this specific repo*, that the user hasn't bothered to formalize: learnings, patterns, validated approaches, debugging insights, internal references.
- **Encoding**: replace every `/` and `.` in the repo root with `-` (e.g. `/Users/foo/.claude` → `-Users-foo--claude`).
- **Structure**: `MEMORY.md` as index (lines like `- [Title](file.md) — one-line hook`), plus topic files (`feedback_<slug>.md`, `project_<slug>.md`, etc.).

### 2. Project CLAUDE.md — `./CLAUDE.md` or `./.claude/CLAUDE.md`

- **Audience**: every teammate working on this repo.
- **Written by**: humans, normally. The agent should write here only when it is **highly confident** the user wants this rule shared and versioned.
- **Loaded**: in full, every session, for everyone on the team. Counts against the context window. Target < 200 lines total.
- **Use for**: build/test commands, code style for the project, project architecture facts, "always do X" rules that apply to the whole repo.
- **Caution**: this file is committed to git. Spurious additions pollute every teammate's context. When in doubt, prefer auto-memory and let the user promote.

### 3. Local CLAUDE.md — `./CLAUDE.local.md`

- **Audience**: just the user on this machine, for this repo.
- **Written by**: humans or the agent.
- **Loaded**: in full, every session, alongside `CLAUDE.md`. Gitignored.
- **Use for**: per-user preferences that ARE behavioral rules (not just learnings): sandbox URLs, the user's preferred test data, their personal workflow shortcuts.
- **Distinction from auto-memory**: this is "rules the user would write themselves but tied to this repo"; auto-memory is "things Claude noticed".

### 4. Path-scoped rules — `./.claude/rules/<topic>.md`

- **Audience**: every teammate, but only when Claude works on matching paths.
- **Written by**: humans or the agent.
- **Loaded**: at session start for unscoped rules, or on demand when Claude reads matching files (via `paths:` frontmatter).
- **Use for**: rules that ONLY apply to a subdirectory or file type. Saves context vs. global CLAUDE.md when the rule is narrow.
- **Frontmatter**:

  ```yaml
  ---
  paths:
    - "src/api/**/*.ts"
  ---
  ```

### 5. User CLAUDE.md — `~/.claude/CLAUDE.md`

- **Audience**: just the user, but across EVERY project on this machine.
- **Written by**: the user, or this agent when confident the rule transcends the current repo.
- **Loaded**: in full, every session, in every repo. Counts against the context window of every project Claude opens — pollution here is the most expensive.
- **Use for**: rules and preferences that hold no matter which codebase the user is in — "I write Go for ten years, frame frontend in terms of backend analogues", "I dislike trailing summaries", "always run `make lint` before committing in any repo". Cross-project facts about the user themselves.
- **Caution**: highest broadcast surface. A bad entry follows the user into every future session everywhere. Bar must be higher than for project CLAUDE.md. If a rule could even slightly be repo-specific, route to `./CLAUDE.local.md` instead.
- **Read first**: do not create `~/.claude/CLAUDE.md` if it does not exist — that means the user has not opted into a user-global file yet, and the agent must not invent one.

### NOT in scope for this skill

- **Managed CLAUDE.md** (org-level): never touched by the agent.

## Decide where it goes

The choice is a 2-axis decision: **scope of the fact** (this repo only / cross-project) × **kind of fact** (inferred learning / explicit rule, team-shared / personal). Walk this tree top-down. Stop at the first match.

1. **Is the fact specifically a learning Claude inferred** (a pattern observed, a validated approach, an internal reference) **about THIS repo**? → **Auto-memory** (default). Pick a topic file under `<type>_<slug>.md` where type ∈ {`user`, `feedback`, `project`, `reference`}.

2. **Is it a rule that ONLY applies to certain files / directories within this repo**? → **Path-scoped rule** in `.claude/rules/<topic>.md` with `paths:` frontmatter.

3. **Is it a rule about THIS repo, applies repo-wide, AND the user would deliberately commit it for the team**? → **Project CLAUDE.md** (use whichever of `./CLAUDE.md` or `./.claude/CLAUDE.md` already exists). Be conservative: ask "would the user write this themselves if they thought about it?". If unsure, fall back to auto-memory.

4. **Is it a rule about THIS repo, applies repo-wide, but ONLY personal to this user**? → **Local CLAUDE.md** (`./CLAUDE.local.md`). Verify the path is in `.gitignore`; if not, fall back to auto-memory.

5. **Is it a rule about the USER themselves that holds across EVERY project** (not specific to this codebase)? → **User CLAUDE.md** (`~/.claude/CLAUDE.md`). Only if the file already exists. Bar is higher than for project CLAUDE.md — a bad entry here follows the user into every future session everywhere. When in doubt between user CLAUDE.md and any repo-scoped surface, pick the repo-scoped one.

6. **None of the above** → return `memory: skipped`.

When two surfaces could fit, prefer the more conservative (narrower scope, smaller audience) one. Auto-memory is always safe; CLAUDE.md edits at any scope are rarely safe; user-global CLAUDE.md edits are the rarest of all.

## Type taxonomy (for auto-memory only)

When routing to auto-memory, classify into one of four types — this drives both the filename prefix and the frontmatter `metadata.type`:

- **user**: a fact about the user's role, expertise, responsibilities, or workflow.
- **feedback**: an explicit correction OR a confirmed approach — generalizable beyond the one task.
- **project**: durable project state — ownership, why a system exists, deadlines, constraints — not derivable from code/git.
- **reference**: a pointer to an external system (Linear project, Slack channel, dashboard URL, doc location).

## Write — auto-memory

1. Resolve `<repo>` via `git rev-parse --show-toplevel`. Encode for the memory dir: replace `/` and `.` with `-`.
2. Memory dir: `~/.claude/projects/<encoded>/memory/`. Create it if absent.
3. Topic file: `<type>_<slug>.md` (e.g. `feedback_no-trailing-summaries.md`). Update an existing file rather than creating a near-duplicate.
4. Topic file content:

   ```markdown
   ---
   name: {kebab-slug}
   description: {one-line summary used to gauge relevance in future sessions}
   metadata:
     type: user | feedback | project | reference
   ---

   {body — for feedback/project, structure as the rule/fact, then **Why:** line and **How to apply:** line}
   ```

5. Update `MEMORY.md` index with a pointer line:
   `- [Title](file.md) — one-line hook`
   Keep `MEMORY.md` under ~150 chars per line. Never write a body in `MEMORY.md` — it is an index only.

## Write — CLAUDE.md (project, local, or user-global)

1. Pick the target file from the decision tree (`./CLAUDE.md` or `./.claude/CLAUDE.md` or `./CLAUDE.local.md` or `~/.claude/CLAUDE.md`).
2. **Read first** — do not create a CLAUDE.md if none exists; the absence is intentional. If absent, fall back to auto-memory.
3. Edit-style: append under the appropriate section header (create one if needed). Keep entries 1-3 lines each, specific and verifiable.
4. Stay under 200 lines total in any single CLAUDE.md. If you would push the file past that limit, route to a path-scoped rule (for project files) or trim outdated content (for user-global) instead.
5. For `~/.claude/CLAUDE.md`: also append a one-line origin note as a markdown comment, so the user can see which agent / repo added the entry — e.g. `<!-- added by kb-curator from <repo-name> on YYYY-MM-DD -->`. The auto-memory system uses no such convention because it lives in a per-project directory, but the user file is shared across repos and origin matters.

## Write — `.claude/rules/<topic>.md`

1. Create or update the file.
2. Always include `paths:` frontmatter — if no path scope applies, this surface is wrong.
3. Body: same style as CLAUDE.md — terse, specific, verifiable.

## Report

Output exactly one line for the orchestrator:
- `memory: saved <surface> <file>` — e.g. `memory: saved auto-memory feedback_no-trailing-summaries.md`
- `memory: updated <surface> <file>` — when editing an existing entry
- `memory: skipped` — when no surface fit

Do not narrate. Do not summarize. Do not ask questions.

## Hard rules

- Never write to managed policy CLAUDE.md.
- Never modify CLAUDE.md if it does not already exist — its absence means the user did not opt into it.
- Never write a body into `MEMORY.md`. It is an index.
- Never duplicate across surfaces: a single fact belongs to one file. If the same fact lives in two places they will drift.
- If an existing entry contradicts the new event, update or remove the old one rather than appending a contradiction.
- Convert relative dates ("Thursday", "next week") to absolute dates before writing.
