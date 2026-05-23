---
type: decision
date: 2026-05-23
sessions: [24c809bf]
commits: [d637286]
tags: [kb-curator, hooks, indexing, architecture]
---

# Tag index + SessionStart hook: make the `.kb/` a consumed asset

Until this commit the KB was write-only — entries accumulated under `.kb/{decisions,fixes,sessions}/` but nothing in the assistant ecosystem ever read them back. Two reinforcing additions flip that: a **tag index** (`.kb/INDEX-by-tag.md`, regenerated alongside the other INDEX files by the Stop hook) gives a cross-cutting discovery surface, and a **SessionStart hook** (`scripts/session-start.py`) injects the KB pointer + root INDEX + a tag-index excerpt at every session start. Rejected: leaving retrieval to ad-hoc `Read` calls — future-Claude wouldn't know the KB exists unless told, defeating the curation effort.

Tag discipline is part of the contract, not just aesthetic: the kb SKILL and the orchestrator both read `INDEX-by-tag.md` during bootstrap and must **reuse existing tags** before inventing new ones — aim for 2-4 per entry (one bucket-domain, one technical surface, 0-2 cross-cutting). Drift (`kb-curator` vs `kbcurator` vs `kb_curator`) silently makes the index useless because the bash parser does exact-string grouping.

Non-obvious invariants this commit introduces:

- **Tag parser handles only inline form** `tags: [a, b, c]`. Block-list YAML form is silently ignored to keep `kb-index.sh` one-line-per-tag bash. If you write a block list, your entry won't show up under any tag.
- **SessionStart hook must stay silent + conditional** — it emits `additionalContext` ONLY when `.kb/INDEX.md` is a file; otherwise exits 0 with no output. This is what lets the plugin be installed in any repo without polluting non-curated sessions.
- **KB root resolution priority is duplicated across three scripts** (`kb-tail.py`, `kb-index.sh`, `session-start.py`): `KB_TAIL_DIR > CLAUDE_PROJECT_DIR > main repo root via --git-common-dir`. Change one → change all three. `kb-index.sh` previously missed `KB_TAIL_DIR` and silently wrote to the wrong directory in global mode.
- **Injection budget is capped** — pointer + full INDEX + first ~30 lines of INDEX-by-tag. The assistant reads the full tag index on demand. Don't expand the injection without measuring per-session token cost first.

General lesson: every curation surface needs a matching consumption surface wired from session start. A write-only KB is dead weight.

Related: [[2026-05-22-kb-index-via-plugin-stop-hook]], [[2026-05-22-curator-as-orchestrator-with-skills]], [[2026-05-22-kb-tail-monitor]]
