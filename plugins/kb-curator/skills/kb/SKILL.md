---
name: kb
description: Use when a transcript Stop or commit event reveals an architectural decision, a debugging fix worth remembering, or a multi-step task completion — anything worth recording as durable historical KNOWLEDGE for future reference by humans or Claude. Writes a markdown entry under .kb/{decisions,fixes,sessions}/.
---

# KB skill — curate one event into `.kb/`

You receive one inbox event (already loaded into your context by the orchestrator). Decide whether it deserves a KB entry, and if so, write or update it. Do not handle inbox lifecycle (listing, moving to processed/) — that is the orchestrator's job.

## Decide

The event must pass THREE filters in order. Skipping any one of them → `kb: skipped`.

0. **Codebase-can-tell-it filter** — is the content of this candidate entry recoverable from `git show <sha>`, the commit message body, an existing `CLAUDE.md`, or an existing `.kb/` entry? If yes, skip. The KB captures only what the code cannot say — *the principle behind the diff, never the diff itself*. If an existing entry covers the same ground, prefer `Edit` to refine it over creating a near-duplicate.

1. **Bucket filter** — at least one bucket must clearly apply:
   - **decisions/**: an architectural or design choice was made, with a rationale articulable in 2-4 sentences. Anchor on **what was rejected and why**, not the picked path alone. Not "I picked variable name X" — yes "we chose pattern A over B because…".
   - **fixes/**: a real bug was diagnosed and resolved. The patch is in git; only file an entry if the lesson **generalizes beyond this one bug** — a concrete "how to avoid next time" line that another debugging episode could reuse.
   - **sessions/**: in the default (repo-mode) interpretation, a multi-step task whose recap contains **at least ONE** of: an invariant the codebase doesn't encode, a dead-end that future-Claude shouldn't retry, a cross-cutting design constraint discovered during the work. "We did X, Y, Z in files A, B, C" duplicates `git log` — skip.
     - **Activity-tracking allowance** (when the active lens explicitly invites history/tracing/journal-style capture — e.g. a personal-wiki `.focus`): a session entry MAY be a chronological recap of work done (date, `source_cwd`, what was attempted, what was concluded, what was left mid-flight) even without one of the three signals above. The lens is the gate — without it, the strict criteria apply. Frame the entry as `<date>-<source-project-or-slug>-work` so daily logs group naturally; keep the body 3–6 sentences; tag with `history` plus the source project's tag if known.

2. **Lens filter** (only when the orchestrator passes you an active lens from `.kb/.focus`): even if a bucket matches, skip the event when it does not align with the lens. The lens is intentionally restrictive — when in doubt, skip.

## Write

If you decide to curate:

1. **Pick a path**: `.kb/<bucket>/YYYY-MM-DD-<short-slug>.md`. The date is the event timestamp, not today. Slug is 2-5 hyphenated words capturing the subject.

2. **Prefer Edit over Write** when a related entry already exists. Extend or refine it rather than creating a near-duplicate. Use `[[other-entry-slug]]` cross-links liberally.

3. **Frontmatter** (minimal, consistent):

   ```yaml
   ---
   type: decision | fix | session
   date: YYYY-MM-DD
   sessions: [<short-id>, ...]
   commits: [<short-sha>, ...]
   tags: [<topic1>, <topic2>]
   ---
   ```

   **Tags**: use the inline `[a, b, c]` form (the parser is one-line by design). Before assigning, **read `.kb/INDEX-by-tag.md`** and reuse existing tags whenever one fits — drift between `kb-curator` / `kbcurator` / `kb_curator` makes the index useless. Invent a new tag ONLY when no existing one captures the concept. Aim for 2–4 tags per entry: one bucket-domain tag (e.g. `kb-curator`, `delegate`, `distill`), one technical surface (`hooks`, `monitors`, `mcp`), and 0–2 cross-cutting concerns (`security`, `ux`, `self-loop`).

4. **Body** — terse, 2-6 sentences. State the thing, not the journey. For fixes, end with a "How to avoid next time" line. For decisions, end with "Related: [[…]]" when applicable.

## Report

Output exactly one line for the orchestrator:
- `kb: curated decisions/<filename>` (or fixes/ / sessions/)
- `kb: updated decisions/<filename>` if you edited an existing entry
- `kb: skipped` if neither filter passed

Do not narrate. Do not summarize. Do not ask questions.

## Hard rules

- Never write outside `.kb/`. Never touch `.kb/inbox/` (that is the orchestrator's domain).
- Never write outside the three buckets (decisions/, fixes/, sessions/). New buckets require a design discussion, not an ad-hoc invention.
- Never invent cross-link slugs that don't exist — only link to entries you can confirm are present in the KB.
