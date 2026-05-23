---
type: decision
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [kb-curator, ux, filtering]
---

# Optional curation lens via `.kb/.focus`

The curator's "what counts as KB-worthy" rules are intentionally broad (architectural decisions / debugging fixes / multi-step sessions). Users frequently want to narrow that for a given project — e.g. "only stuff about the monitor subsystem" or "only API design choices, ignore bugfixes". Solution: a per-repo lens stored in `.kb/.focus`.

Mechanism:
- On the first wake the agent creates `.kb/.focus` from a self-explanatory template (comments + empty body) and prints a one-line nudge so the user knows it exists. No prompt blocks the queue.
- On subsequent wakes, the agent reads the file. Comment-only / empty / literal `none` → no lens, default behaviour. Otherwise non-comment lines become the active lens.
- During step 2c of the workflow the lens runs AFTER the bucket filter as a *restrictive* second pass — "when in doubt, skip". Bucket filter alone keeps too much; the lens trims toward what the user explicitly cares about.
- End line may include `[lens: <summary>]` so a single grep across logs reveals which runs were filtered.

Lives entirely in `agents/kb-curator.md` — no Go change. Plugin agent definitions are loaded at hub boot, so changes need a session restart or `/reload-plugins` to take effect.

Related: [[2026-05-22-kb-tail-monitor]], [[2026-05-22-kb-tail-self-curation]]
