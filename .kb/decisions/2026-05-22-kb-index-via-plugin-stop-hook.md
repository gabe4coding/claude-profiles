---
type: decision
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [kb-curator, hooks, indexing, plugin-scoping]
---

# `.kb/INDEX.md` auto-regen via plugin Stop hook (not agent frontmatter)

The KB grew enough to want a single `INDEX.md` listing all entries. Two implementation paths considered:

1. **Plugin-level Stop hook** (chosen) — `.claude-profiles/kb-curator/hooks/hooks.json` registers a Stop hook running `bash "${CLAUDE_PLUGIN_ROOT}"/scripts/kb-index.sh`. The plugin is only loaded when the user runs `claude-profiles run kb-curator`, so the hook fires only inside the curator session — effectively agent-scoped without needing the agent-frontmatter mechanism.
2. **Agent-frontmatter `hooks:`** (rejected) — Claude Code's docs block hooks on plugin subagents for security reasons. The only way to use frontmatter hooks would be to move the agent out of the plugin into `.claude/agents/`, losing the rest of the plugin scoping.

`scripts/kb-index.sh` resolves `repo_root` from `$CLAUDE_PROJECT_DIR` (set by Claude Code when an hook fires) with a `git rev-parse` fallback. It enumerates each bucket, extracts the first `# Heading` as link text, sorts by date desc, and writes atomically via tmp+rename. **Always exits 0** so a regen failure never blocks the user's turn.

Index layout: `.kb/INDEX.md` is a root meta-index (one line per bucket, with count and singular/plural). Each bucket also gets its own `.kb/<bucket>/INDEX.md` with the full ordered list, using basenames as relative links. Empty buckets get their stale `INDEX.md` removed; the root is always regenerated (placeholder when the KB is empty). `INDEX.md` itself is excluded from the per-bucket scan to avoid self-listing.

Quote `${CLAUDE_PLUGIN_ROOT}` in `hooks.json` — paths with spaces would otherwise tokenize wrong and silently no-op.

Related: [[2026-05-22-kb-tail-monitor]], [[2026-05-22-kb-curator-focus-lens]]
