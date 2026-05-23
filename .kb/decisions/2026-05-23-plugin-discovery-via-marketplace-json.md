---
type: decision
date: 2026-05-23
sessions: [24c809bf]
commits: [9ca09fb]
tags: [kb-curator, claude-profiles, plugin-discovery, marketplace]
---

# Register kb-curator plugin via `.claude-plugin/marketplace.json` (keep plugin agnostic)

After moving kb-curator out of `.claude-profiles/kb-curator/` to make the plugin agnostic from claude-profiles, the profile disappeared from `claude-profiles list` because `listCwdProfileLocations` only scans `.claude-profiles/<name>/`. Two rejected paths: (a) teach `listCwdProfileLocations` to also scan `plugins/*/` — couples claude-profiles to plugin layout conventions it doesn't own; (b) symlink `.claude-profiles/kb-curator → plugins/kb-curator` — leaks claude-profiles back into the plugin's surface and breaks the "plugin works without the wrapper" property.

Chosen path: declare the plugin via `.claude-plugin/marketplace.json` at the repo root. `listCwdMarketplaceProfiles` already discovers marketplace entries as a first-class profile source, so the plugin reappears as `[project]` in the hub with zero Go change.

Load-bearing distinction: `marketplace.json` is a claim **FROM the repo ABOUT a plugin location**, not a dependency declared **BY the plugin**. The `plugins/kb-curator/` tree stays free of any claude-profiles reference — anyone can install the plugin into vanilla Claude Code without touching the wrapper. The repo-level marketplace file is the only place those two worlds meet.

General lesson: when a plugin moves out of `.claude-profiles/`, prefer reaching for an existing discovery surface (marketplace) over modifying the wrapper or symlinking. The wrapper has multiple profile sources by design — check them before adding code.

Related: [[2026-05-22-curator-as-orchestrator-with-skills]], [[2026-05-22-mcp-config-optional]]
