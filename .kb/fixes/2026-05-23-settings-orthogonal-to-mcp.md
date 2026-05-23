---
type: fix
date: 2026-05-23
commits: [5edd5c1]
tags: [profile-subsystem, mcp, plugin-only-profiles]
---

## Settings.json silently dropped for plugin-only profiles

`loadProfileAt` was loading `settings.json` only inside the `if .mcp.json exists` branch. Plugin-only profiles (agents/skills/monitors/hooks but no MCP servers — e.g. kb-curator) had their settings.json silently dropped: model, agent, permissions, statusLine never reached the profile struct or the claude invocation.

**Root cause**: the two config surfaces (MCP servers in `.mcp.json`, Claude settings in `settings.json`) were incorrectly treated as mutually exclusive. They are orthogonal — a profile can ship both, one, or neither.

**Fix**: load `settings.json` independently of `.mcp.json` existence. A new test, `TestLoadProfileAt_PluginOnlySettings`, covers the plugin-only path to prevent regression.

**How to avoid next time**: When adding a new config surface to profiles, treat it as independent. Test all combinations of presence/absence, not just the "both present" path.
