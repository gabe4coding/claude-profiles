---
type: decision
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [profile-subsystem, mcp, plugin-only-profiles]
---

# `.mcp.json` is optional — plugin-only profiles need no placeholder

Plugin-only profiles (those that contribute only `agents/`, `monitors/`, `commands/`, `hooks/`, `skills/`) no longer require a placeholder `.mcp.json` to be launchable. `resolveMCPConfigPath()` in `profile.go` returns the split file if present, the combined `profile.json` if present, or `""`. All three callsites (`run.go`, `exec.go`, `delegate_bg.go`) now skip BOTH `--strict-mcp-config` AND `--mcp-config` when the resolver returns empty — letting claude fall back to native MCP discovery. Previously the wrapper forced `--strict-mcp-config <stale-json>` and failed at startup for profiles with no MCP servers to declare.

The two flags must stay coupled: dropping one while keeping the other would either lock out MCP discovery (strict alone) or silently re-enable it under the wrong identity (mcp-config alone).

Related: [[2026-05-22-kb-tail-monitor]] (the kb-curator profile was the first plugin-only profile to trip this).
