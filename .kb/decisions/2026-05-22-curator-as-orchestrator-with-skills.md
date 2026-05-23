---
type: decision
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [kb-curator, architecture, skills, memory, separation-of-concerns]
---

# kb-curator becomes an orchestrator; KB + memory move into skills

The curator was originally a single agent that decided AND wrote `.kb/` entries. As Claude Code's auto-memory surface entered scope, we needed a second destination (`~/.claude/projects/<encoded-repo>/memory/`) with different semantics. Stuffing both behaviours into one agent prompt blurred the rules ("when is something a KB fix vs a memory cue?") and made each future surface a fresh prompt rewrite.

New shape:

- **Agent `kb-curator`** = pure orchestrator. Owns the focus lens (`.kb/.focus`), drains `.kb/inbox/` in order, resolves transcript/diff source, classifies, **routes** to one or both skills, moves files to `processed/`. **Never writes** under `.kb/<bucket>/` or under the memory dir. Outputs `curated N remembered M skipped K [lens: ...]`.
- **Skill `kb`** (`skills/kb/SKILL.md`) = owns `.kb/{decisions,fixes,sessions}/` entry shape, bucket + lens filters, cross-links, atomic writes. Reports `kb: curated|updated|skipped`.
- **Skill `memory`** (`skills/memory/SKILL.md`) = owns `~/.claude/projects/<encoded-repo>/memory/`, classifies into `user|feedback|project|reference` types, writes `<type>_<slug>.md`, maintains `MEMORY.md` index. Reports `memory: saved|updated|skipped`.

Anti-duplication rule (lives in the memory SKILL): **decisions / fixes go in KB**, **behaviour cues** (workflow preferences, install steps, "always do X after Y") go in memory. The orchestrator may legitimately route a single event to both skills when it informs both surfaces, but that's rare.

Pure markdown — no Go change. Effects load on hub session restart (plugin agents/skills are loaded at boot, not hot-reloaded).

Related: [[2026-05-22-kb-tail-monitor]], [[2026-05-22-kb-curator-focus-lens]], [[2026-05-22-kb-index-via-plugin-stop-hook]]
