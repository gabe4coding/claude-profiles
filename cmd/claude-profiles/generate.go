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

# Step 1 — resolve profiles root and determine mode

Run this first to discover existing profiles and the root path:

` + "```" + `bash
PROFILES_ROOT="${CLAUDE_PROFILES_ROOT:-$HOME/.claude-profiles}"
PROFILES_DIR="$PROFILES_ROOT/profiles"
ls "$PROFILES_DIR" 2>/dev/null && echo "PROFILES_DIR=$PROFILES_DIR"
` + "```" + `

Parse $ARGUMENTS:
- Empty → ask with AskUserQuestion: "Create a new profile or update an existing one?" Options: every name from ` + "`ls \"$PROFILES_DIR\"`" + ` plus "New profile".
- First token exactly matches a directory under $PROFILES_DIR → UPDATE that profile; rest of $ARGUMENTS = update intent.
- Otherwise → all tokens are the intent for a NEW profile.

Profile scope — ask only when ambiguous:
- **Local** (` + "`$PROFILES_DIR/<name>/`" + `) — personal, available in all projects, never committed.
- **Project** (` + "`.claude-profiles/<name>/`" + ` under the nearest git root) — team-shared, committed to the repo.

Default to local. Suggest project only when the user is inside a git repo and the profile is clearly project-specific.

# Step 2 — gather missing details

If the intent leaves key decisions open, ask with AskUserQuestion (≤ 3 questions at a time):
- Which external services or tools? (determines MCP servers and/or plugins)
- Isolated (no user settings merged) or blended with user config?
- Model preference: haiku (fast/cheap), sonnet (balanced), opus (complex reasoning)?
- Quick-start prompts to show before session launch?

Prefer sensible defaults. Skip any question whose answer is already clear from context.

# Step 3 — research (required before writing)

For EVERY MCP server you plan to include, WebFetch the vendor's official docs to verify the current endpoint URL and auth mechanism. Do NOT rely on training-data assumptions — vendors change endpoints and auth.

If WebFetch fails or is unavailable, ask the user for the endpoint and auth details instead.

Auth preference (always pick the highest available):
1. Cloud HTTP + OAuth — STRONGLY preferred (no local install, no API keys)
2. Cloud HTTP + API key headers
3. Local stdio + OAuth
4. Local stdio + env-var API key — last resort

Plugin vs MCP heuristics:
- Workflow / reasoning / discipline (planning, TDD, code-review patterns) → plugins first; ` + "`obra/superpowers-marketplace`" + ` is a strong default.
- External service access (GitHub, Slack, DBs, APIs) → MCP server or a plugin bundling one.
- LSP / code intelligence → plugins from ` + "`claude-plugins-official`" + `.
- Analytics / observability → MCP server.

Before enabling any plugin, WebFetch the marketplace index to confirm the plugin name exists exactly.

Useful references (fetch if unsure about schema or availability):

Settings and MCP:
- https://docs.claude.com/en/docs/claude-code/settings — full settings.json schema including sandbox, env, hooks, statusLine, and all advanced keys
- https://docs.claude.com/en/docs/claude-code/mcp — MCP config schema and auth patterns
- https://docs.claude.com/en/docs/claude-code/plugins-reference — extraKnownMarketplaces + enabledPlugins keys

Plugin content (commands / skills / agents / hooks):
- https://docs.claude.com/en/docs/claude-code/slash-commands — authoring slash commands (frontmatter, $ARGUMENTS, allowed-tools)
- https://docs.claude.com/en/docs/claude-code/hooks — hook types (PreToolUse, PostToolUse, UserPromptSubmit, SessionStart, Stop) and shell contract
- https://docs.claude.com/en/docs/claude-code/sub-agents — authoring agent definitions
- https://docs.claude.com/en/docs/claude-code/skills-and-skills — skills frontmatter and invocation

MCP discovery:
- https://github.com/modelcontextprotocol/servers — MCP server directory
- https://github.com/punkpeye/awesome-mcp-servers — community MCP list

# Step 4 — choose a name (new profiles only)

Propose a name based on the intent: lowercase, hyphens only, no spaces, no slashes, no leading dot.
Valid pattern: ` + "`[a-z0-9][a-z0-9-]*`" + `. Confirm with the user if non-obvious.

# Step 5 — write the three profile files

## Target directory

- Local: ` + "`$PROFILES_DIR/<name>/`" + `
- Project: ` + "`<git-root>/.claude-profiles/<name>/`" + ` (run ` + "`git rev-parse --show-toplevel`" + `; fall back to cwd if not in a repo)

For an UPDATE: Read each existing file before merging changes.

---

## File 1: ` + "`profile.json`" + ` — metadata only

Contains ONLY metadata. Never put MCP servers or settings here.

` + "```" + `json
{
  "_description": "<one sentence: what the profile does>",
  "_isolated": true,
  "_prompts": [{"name": "<label>", "text": "<ready-to-send message>"}]
}
` + "```" + `

Rules:
- ` + "`_description`" + ` — required; one clear sentence explaining the profile's purpose.
- ` + "`_isolated`" + ` — ` + "`true`" + ` only when a clean-room environment is needed (no user settings merged); omit or ` + "`false`" + ` otherwise.
- ` + "`_prompts`" + ` — include only if there are obvious recurring starting points; omit entirely if not useful.
- ` + "`_cwd`" + ` — NEVER set manually (auto-managed by the hub when the user pins a profile to a directory).
- Omit null / empty / false fields entirely.

---

## File 2: ` + "`.mcp.json`" + ` — MCP servers

` + "```" + `json
{
  "mcpServers": {
    "<server-name>": {"type": "http", "url": "https://..."},
    "<server-name>": {"type": "stdio", "command": "<binary>", "args": ["..."]}
  }
}
` + "```" + `

Rules:
- Use only the endpoint/command verified in Step 3 — never training-data guesses.
- HTTP + OAuth: ` + "`{\"type\": \"http\", \"url\": \"...\"}`" + ` only — Claude Code handles OAuth automatically.
- HTTP + API key: add ` + "`\"headers\": {\"Authorization\": \"Bearer ${MY_API_KEY}\"}`" + ` and list the var under ` + "`env`" + ` in settings.json.
- stdio: prefer globally-installed binaries over ` + "`npx`" + `/` + "`uvx`" + ` when reasonable.
- No MCP servers needed? Write ` + "`{\"mcpServers\": {}}`" + `.

---

## File 3: ` + "`settings.json`" + ` — Claude Code settings

Include only keys relevant to this profile. Omit the file entirely if no settings are needed.

` + "```" + `json
{
  "model": "haiku|sonnet|opus",
  "permissions": {
    "defaultMode": "default|acceptEdits|plan|bypassPermissions",
    "deny": ["mcp__<server>__<tool>"]
  },
  "env": {
    "MY_API_KEY": ""
  },
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
  "enabledPlugins": {
    "<plugin>@<marketplace>": true
  }
}
` + "```" + `

Key rules:
- **sandbox** — include whenever the profile touches external services or writes files. IMPORTANT: sandbox uses nested sub-objects ` + "`filesystem`" + ` and ` + "`network`" + ` — do NOT flatten their keys to the sandbox top level. Default protections (always include): ` + "`denyWrite: [\"~/.ssh\",\"~/.aws\",\"~/.config/gh\"]`" + `, ` + "`denyRead: [\"~/.ssh/id_*\"]`" + `.
- **model** — haiku for fast/simple, sonnet for balanced, opus for complex reasoning.
- **permissions.deny** — MCP tools follow the ` + "`mcp__<server>__<tool>`" + ` pattern. List only what needs to be blocked.
- **env** — placeholder ` + "`\"\"`" + ` values for API keys the user must fill in.
- **plugins** — both ` + "`extraKnownMarketplaces`" + ` AND ` + "`enabledPlugins`" + ` are required to activate a plugin; only enable plugins that materially serve the intent.

---

## Optional: Bundle plugin content into the profile

A profile directory is itself a Claude Code plugin: if the user asks for slash commands, skills, agents, or hooks specific to this profile, create the corresponding ` + "`commands/`" + `, ` + "`skills/`" + `, ` + "`agents/`" + `, or ` + "`hooks/`" + ` subdirectories alongside the three core files. The runner loads them automatically via ` + "`--plugin-dir`" + `. Fetch the relevant doc pages from Step 3 for the exact file format. Only create plugin content the intent genuinely calls for.

# Step 6 — report

After writing all files, summarize:

> **Profile ` + "`<name>`" + ` created** at ` + "`<dir>`" + `
> - ` + "`profile.json`" + `: <description>
> - ` + "`.mcp.json`" + `: <N> server(s): <names>
> - ` + "`settings.json`" + `: <key settings in one line, or "none">
>
> Use ` + "`/handoff <name>`" + ` to switch to this profile.
`
