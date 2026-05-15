package main

// generateSlashCommand is the /generate in-session slash command.
// It teaches Claude how to create or update profile files directly using
// Write/Read/Bash. Profile creation goes through this skill; the old headless
// CLI generator (cmdGenerate) has been removed.
const generateSlashCommand = `---
description: Create or update a claude-profiles profile. Describe what you need; the agent researches MCP servers and plugins, then writes the profile files directly.
argument-hint: [profile-name] [intent...]
allowed-tools: AskUserQuestion, WebFetch, Read, Write, Bash
---
The user typed ` + "`/generate $ARGUMENTS`" + `.

# Phase 1 — Discover

Run this to learn the environment before doing anything else:

` + "```" + `bash
PROFILES_ROOT="${CLAUDE_PROFILES_ROOT:-$HOME/.claude-profiles}"
PROFILES_DIR="$PROFILES_ROOT/profiles"
ls "$PROFILES_DIR" 2>/dev/null
git rev-parse --show-toplevel 2>/dev/null || echo "(not in a git repo)"
` + "```" + `

# Phase 2 — Scope

Use AskUserQuestion to establish the three scope dimensions. Ask only what $ARGUMENTS doesn't already answer.

**2a. Mode** — if $ARGUMENTS is empty or doesn't name an existing profile:
> "Create a new profile or update an existing one?"
> Options: "New profile" + one option per existing profile name.

If the first token of $ARGUMENTS exactly matches a directory under $PROFILES_DIR, skip this question — that is the profile to update.

**2b. Location** — always ask, no exceptions:
> "Where should this profile live?"
> Options:
> - "Local (~/.claude-profiles/profiles/) — personal, never committed"
> - "Project (.claude-profiles/ in the repo) — team-shared, committed to git"

**2c. Intent** — if no free-form intent is in $ARGUMENTS yet:
> "What should this profile do? Describe the tools, services, or workflow."
> (free-text)

# Phase 3 — Research

WebFetch vendor docs for every MCP server or plugin the intent implies. Do NOT assume endpoints or auth — they change.

If WebFetch is unavailable or returns an error, ask the user directly for the endpoint and auth details.

Useful references:
- https://docs.claude.com/en/docs/claude-code/settings — full settings.json schema (sandbox, env, hooks, model, permissions, …)
- https://docs.claude.com/en/docs/claude-code/mcp — MCP config schema and auth patterns
- https://docs.claude.com/en/docs/claude-code/plugins-reference — extraKnownMarketplaces + enabledPlugins keys
- https://docs.claude.com/en/docs/claude-code/slash-commands — slash command file format
- https://docs.claude.com/en/docs/claude-code/hooks — hook types and shell contract
- https://docs.claude.com/en/docs/claude-code/sub-agents — agent definition format
- https://github.com/modelcontextprotocol/servers — MCP directory
- https://github.com/punkpeye/awesome-mcp-servers — community MCP list

Plugin vs MCP heuristic (use to shape research, not to decide for the user):
- Workflow / reasoning / discipline → plugins (` + "`obra/superpowers-marketplace`" + ` is a strong starting point)
- External service access → MCP server or a plugin bundling one
- LSP / code intelligence → plugins from ` + "`claude-plugins-official`" + `
- Analytics / observability → MCP server

Before including any plugin, WebFetch the marketplace index to confirm the exact plugin name.

# Phase 4 — Configure

Present every significant decision as an AskUserQuestion. Do not silently pick defaults.

**4a. MCP servers / plugins** — list what you found and ask:
> "Which MCP servers or plugins should this profile include?"
> Options: one per discovered candidate (name + one-line description). Include "None" if relevant.

**4b. Auth method** — for each selected MCP server that offers multiple options:
> "How should [server] authenticate?"
> Options derived from what the vendor actually supports, ordered best-first:
> - "OAuth via cloud endpoint (recommended — no keys needed)"
> - "API key via HTTP header"
> - "Local stdio with OAuth"
> - "Local stdio with API key in env var"

**4c. Model**:
> "Which model should this profile use?"
> Options:
> - "sonnet — balanced speed and capability (recommended)"
> - "haiku — fastest and cheapest, for simple tasks"
> - "opus — most capable, for complex reasoning"
> - "Inherit from user settings (no override)"

**4d. Permission mode**:
> "How should Claude handle tool permissions?"
> Options:
> - "default — prompt for each tool (recommended)"
> - "acceptEdits — auto-approve file edits, prompt for the rest"
> - "plan — read-only, no edits or commands"
> - "bypassPermissions — auto-approve everything"

**4e. Isolation**:
> "Should this profile load the user's own settings.json?"
> Options:
> - "Blended — merge with user config (recommended)"
> - "Isolated — only this profile's settings, nothing else"

**4f. Quick-start prompts** — only ask if the intent suggests recurring entry points:
> "Add quick-start prompts? (shown as a picker before each session)"
> Options: "Yes", "No"
> If Yes, follow up to collect the prompt names and texts.

# Phase 5 — Name

For new profiles only — propose a slug based on the intent (lowercase, hyphens, no spaces, no slashes, no leading dot; pattern: ` + "`[a-z0-9][a-z0-9-]*`" + `).

Confirm with AskUserQuestion:
> "Profile name: <proposed-name>. Use this or change it?"
> Options: "Use <proposed-name>", "Enter a different name"

# Phase 6 — Write

## Target directory

- Local: ` + "`$PROFILES_DIR/<name>/`" + `
- Project: ` + "`<git-root>/.claude-profiles/<name>/`" + `

For an UPDATE: Read each existing file first, then merge only what changed.

---

### ` + "`profile.json`" + ` — metadata only

` + "```" + `json
{
  "_description": "<one sentence: what this profile does>",
  "_isolated": true,
  "_prompts": [{"name": "<label>", "text": "<ready-to-send message>"}]
}
` + "```" + `

- ` + "`_description`" + ` — required; one sentence.
- ` + "`_isolated`" + ` — include only when the user chose "Isolated" in 4e; omit otherwise.
- ` + "`_prompts`" + ` — include only when the user said Yes in 4f.
- ` + "`_cwd`" + ` — NEVER set manually.
- Omit null / empty / false fields.

---

### ` + "`.mcp.json`" + ` — MCP servers

` + "```" + `json
{
  "mcpServers": {
    "<name>": {"type": "http", "url": "https://..."},
    "<name>": {"type": "stdio", "command": "<binary>", "args": ["..."]}
  }
}
` + "```" + `

- HTTP + OAuth: ` + "`{\"type\":\"http\",\"url\":\"...\"}`" + ` — Claude Code handles the OAuth flow.
- HTTP + API key: add ` + "`\"headers\":{\"Authorization\":\"Bearer ${MY_API_KEY}\"}`" + ` and document the var in ` + "`env`" + `.
- stdio: prefer globally-installed binaries over ` + "`npx`" + `/` + "`uvx`" + `.
- No servers? Write ` + "`{\"mcpServers\":{}}`" + `.

---

### ` + "`settings.json`" + ` — Claude Code settings

Omit the file entirely if no settings are needed. Only include keys that reflect the choices made in Phase 4.

` + "```" + `json
{
  "model": "haiku|sonnet|opus",
  "permissions": {
    "defaultMode": "default|acceptEdits|plan|bypassPermissions",
    "deny": ["mcp__<server>__<tool>"]
  },
  "env": {"MY_API_KEY": ""},
  "sandbox": {
    "filesystem": {
      "allowWrite": ["<path>"],
      "denyWrite": ["~/.ssh", "~/.aws", "~/.config/gh"],
      "denyRead": ["~/.ssh/id_*"]
    },
    "network": {
      "allowedDomains": ["*.example.com"]
    }
  },
  "extraKnownMarketplaces": {
    "<name>": {"source": "github", "github": {"owner": "...", "repo": "..."}}
  },
  "enabledPlugins": {"<plugin>@<marketplace>": true}
}
` + "```" + `

- **sandbox** — include whenever the profile touches external services or the filesystem. ` + "`filesystem`" + ` and ` + "`network`" + ` are nested sub-objects; do NOT flatten their keys to the top level. Always include: ` + "`denyWrite:[\"~/.ssh\",\"~/.aws\",\"~/.config/gh\"]`" + `, ` + "`denyRead:[\"~/.ssh/id_*\"]`" + `.
- **permissions.deny** — MCP tool names follow ` + "`mcp__<server>__<tool>`" + `.
- **plugins** — both ` + "`extraKnownMarketplaces`" + ` AND ` + "`enabledPlugins`" + ` are required; only include plugins confirmed in Phase 3.

---

### Optional: plugin content subdirectories

A profile directory is itself a Claude Code plugin. If the intent calls for it, create ` + "`commands/`" + `, ` + "`skills/`" + `, ` + "`agents/`" + `, or ` + "`hooks/`" + ` subdirectories alongside the three core files — the runner loads them via ` + "`--plugin-dir`" + ` automatically. Fetch the relevant reference doc from Phase 3 before writing any file.

# Phase 7 — Report

After writing all files:

> **Profile ` + "`<name>`" + ` created** at ` + "`<dir>`" + `
> - ` + "`profile.json`" + `: <description>
> - ` + "`.mcp.json`" + `: <N> server(s): <names>
> - ` + "`settings.json`" + `: <key settings, or "none">
>
> Use ` + "`/handoff <name>`" + ` to switch to this profile.
`
