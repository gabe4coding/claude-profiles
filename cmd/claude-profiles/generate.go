package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

// generateSlashCommand is the /generate in-session slash command.
// It teaches Claude how to create or update profile files directly using
// Write/Read/Bash, as opposed to cmdGenerate which uses a headless agent.
// The knowledge here mirrors generatorSystemPrompt but targets the split-file
// format that saveProfileAt produces, not the combined profile.json the CLI flow uses.
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
- https://docs.claude.com/en/docs/claude-code/mcp — MCP config schema
- https://docs.claude.com/en/docs/claude-code/settings — settings.json full schema (sandbox is nested: filesystem + network sub-objects)
- https://docs.claude.com/en/docs/claude-code/plugins-reference — extraKnownMarketplaces + enabledPlugins keys
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

# Step 6 — report

After writing all files, summarize:

> **Profile ` + "`<name>`" + ` created** at ` + "`<dir>`" + `
> - ` + "`profile.json`" + `: <description>
> - ` + "`.mcp.json`" + `: <N> server(s): <names>
> - ` + "`settings.json`" + `: <key settings in one line, or "none">
>
> Use ` + "`/handoff <name>`" + ` to switch to this profile.
`

// ── AI profile generator ──────────────────────────────────────────────────────
//
// Given a free-form intent ("I want a profile for triaging GitHub issues"), an
// agent (sonnet + WebFetch) researches MCP servers and Claude Code settings,
// then proposes a profile. The agent may ask up to a few rounds of clarifying
// questions; the final draft is rendered to an HTML recap the user reviews
// before saving.

const maxGeneratorRounds = 4

type genTurn struct {
	role    string // "user" | "assistant"
	content string
}

type genQuestion struct {
	Key     string   `json:"key"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options,omitempty"`
}

type genResponse struct {
	Questions []genQuestion    `json:"questions,omitempty"`
	Profile   *json.RawMessage `json:"profile,omitempty"`
	Rationale string           `json:"rationale,omitempty"`
}

func cmdGenerate(initialIntent string) {
	intent := strings.TrimSpace(initialIntent)
	if intent == "" {
		intent = readGenerateIntent()
	}
	if intent == "" {
		return
	}

	history := []genTurn{{role: "user", content: intent}}
	var draft *Profile
	var rationale string

	for round := 1; round <= maxGeneratorRounds; round++ {
		info("Generating with sonnet (round %d/%d) — this may take a minute…", round, maxGeneratorRounds)
		resp, err := callGenerator(history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Generator failed: %v\n", err)
			return
		}

		if len(resp.Questions) > 0 {
			answers := askGeneratorQuestions(resp.Questions)
			if answers == nil {
				return // user aborted
			}
			history = append(history, genTurn{role: "assistant", content: marshalAssistantTurn(resp)})
			history = append(history, genTurn{role: "user", content: marshalAnswers(answers)})
			continue
		}

		if resp.Profile != nil {
			var p Profile
			if err := json.Unmarshal(*resp.Profile, &p); err != nil {
				fmt.Fprintf(os.Stderr, "Agent returned invalid profile JSON: %v\n", err)
				return
			}
			draft = &p
			rationale = resp.Rationale
			break
		}

		warn("Agent reply had neither questions nor a profile — aborting.")
		return
	}

	if draft == nil {
		warn("Generator did not converge after %d rounds.", maxGeneratorRounds)
		return
	}

	if err := showRecap(draft, rationale); err != nil {
		fmt.Fprintf(os.Stderr, "Could not render recap: %v\n", err)
	}

	if !confirm("Save this profile?") {
		info("Discarded.")
		return
	}

	name := strings.ReplaceAll(prompt("Profile name"), " ", "-")
	if name == "" {
		warn("Empty name — discarded.")
		return
	}
	if profileExists(name) {
		if !confirm(fmt.Sprintf("Profile %q already exists. Overwrite?", name)) {
			return
		}
	}
	if err := saveProfile(name, draft); err != nil {
		fatal(err)
		return
	}
	success("Saved %q → %s", name, profilePath(name))
	if confirm("Launch now?") {
		cmdRun([]string{name})
	}
}

// readGenerateIntent prompts for the user's intent when none was provided.
func readGenerateIntent() string {
	if !isTTY() {
		return promptLine("Profile intent: ")
	}
	var out string
	err := runField(huh.NewText().
		Title("Generate a profile with AI").
		Description("Describe what this profile should do. The agent will research relevant MCP servers and propose a profile.").
		Value(&out).
		CharLimit(4000))
	if err != nil {
		handleAbort(err)
		return ""
	}
	return strings.TrimSpace(out)
}

// askGeneratorQuestions prompts the user for each question the agent raised.
// Returns nil if the user aborted any prompt.
func askGeneratorQuestions(qs []genQuestion) map[string]string {
	answers := map[string]string{}
	for _, q := range qs {
		clean := dedupOptions(q.Options)
		var ans string
		if len(clean) >= 2 {
			ans = askGenSelect(q.Prompt, clean)
		} else {
			ans = prompt(q.Prompt)
		}
		if ans == "" && hubMode {
			return nil
		}
		answers[q.Key] = ans
	}
	return answers
}

func dedupOptions(opts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		out = append(out, o)
	}
	return out
}

func askGenSelect(title string, opts []string) string {
	if !isTTY() {
		fmt.Fprintln(os.Stderr, title)
		for i, o := range opts {
			fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, o)
		}
		raw := promptLine("Select [1-" + fmt.Sprintf("%d", len(opts)) + "]: ")
		var n int
		fmt.Sscanf(strings.TrimSpace(raw), "%d", &n)
		if n < 1 || n > len(opts) {
			return ""
		}
		return opts[n-1]
	}
	options := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		options[i] = huh.NewOption(o, o)
	}
	var sel string
	err := runField(huh.NewSelect[string]().
		Title(title).
		Options(options...).
		Value(&sel))
	if err != nil {
		handleAbort(err)
	}
	return sel
}

// ── Agent invocation ─────────────────────────────────────────────────────────

func callGenerator(history []genTurn) (*genResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	emptyMCP := filepath.Join(os.TempDir(), "claude-profiles-generator-mcp.json")
	if err := os.WriteFile(emptyMCP, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		return nil, err
	}

	full := buildGeneratorPrompt(history)

	cmd := exec.CommandContext(ctx, "claude",
		"-p", "--model", "sonnet",
		"--strict-mcp-config", "--mcp-config", emptyMCP,
		"--allowedTools", "WebFetch",
		"--", full)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseGenResponse(string(out))
}

const generatorSystemPrompt = `You design Claude Code profiles. The user describes what they want; you research, then produce a runnable JSON profile.

Profile JSON shape:
{
  "_description": "<one sentence purpose>",
  "mcpServers": {
    "<name>": {"type": "http", "url": "https://..."},
    "<name>": {"type": "stdio", "command": "<binary>", "args": ["..."]}
  },
  "_settings": {
    "model": "haiku"|"sonnet"|"opus",
    "permissions": {"defaultMode": "default"|"acceptEdits"|"plan"|"bypassPermissions"},
    "sandbox": { ... REQUIRED — see SANDBOX section below ... },
    "extraKnownMarketplaces": { ... when enabling plugins ... },
    "enabledPlugins": { "<plugin-name>@<marketplace-name>": true }
  }
}
Settings keys other than the above are also valid (env, hooks, statusLine, etc.) — include them when the intent calls for it.

PLUGINS — consider alongside MCP servers, and search beyond the official marketplace.
Claude Code plugins (skills, slash commands, agents, hooks, LSP integrations) often fit an intent better than a raw MCP server — and well-curated community marketplaces frequently beat the official one for skill-heavy intents (spec-driven dev, TDD, debugging discipline, code review).

To enable plugins, both settings keys are usually required:
- "extraKnownMarketplaces": registers a marketplace by source (github / git-subdir / npm / url / local path). REQUIRED for any marketplace that is not reserved/auto-available.
- "enabledPlugins": map of "plugin-name@marketplace-name" → true

Discovery — do NOT restrict yourself to the Anthropic-official marketplace. The community ecosystem is often a better fit for skill-heavy intents. Curated starting set (verify each before enabling — names and plugin lists drift):

| Marketplace | Source | Strongest fit |
|---|---|---|
| anthropics/claude-plugins-official | claude-plugins-official (reserved) | Cloud (AWS/Azure), DevOps (GitHub/Jira), observability (Datadog), LSP servers, frontend-design |
| obra/superpowers-marketplace | obra/superpowers-marketplace | Disciplined methodology — TDD, brainstorming, writing-plans, systematic-debugging, subagent-driven-development, verification-before-completion |
| xiaolai/claude-plugin-marketplace | xiaolai/claude-plugin-marketplace | Quality gates (LOC limits, docs staleness), design-token enforcement, deep code review |
| ananddtyagi/cc-marketplace | ananddtyagi/cc-marketplace | Multi-agent workflows (ultrathink), security audit, accessibility, prompt optimization |

Discovery references (use when the curated set above doesn't obviously fit):
- https://docs.claude.com/en/docs/claude-code/plugins-reference — settings.json keys + marketplace source schema.
- https://docs.claude.com/en/docs/claude-code/plugin-marketplaces — registration mechanics, reserved names.
- https://claudemarketplaces.com and https://github.com/Chat2AnyLLM/awesome-claude-plugins — broader directories.

Before recommending any plugin, WebFetch the marketplace's repo or plugin index to confirm the plugin still exists under the exact name you'll write into enabledPlugins. Stale names break the profile silently.

Heuristics for plugins vs MCP servers:
- Workflow / reasoning / discipline (planning, TDD, code review patterns) → plugins/skills first; obra/superpowers-marketplace is a strong default for spec-driven / TDD intents.
- External service access (GitHub, Slack, DBs) → MCP server, OR a plugin that bundles the MCP (often cleaner).
- LSP / code intelligence → plugins (typescript-lsp, pyright-lsp, etc.) from claude-plugins-official.
- Pure analytics / observability → MCP server, plugins rarely apply.

Only enable plugins that materially serve the intent — empty enabledPlugins beats padding with plausible-sounding plugins.

SANDBOX — REQUIRED in every profile you produce, and tailored to the intent.
Look up the sandbox schema at https://docs.claude.com/en/docs/claude-code/settings (section "sandbox") via WebFetch if you don't already know the exact keys. Note: sandbox has nested "filesystem" and "network" sub-objects — allowWrite/denyWrite/denyRead live under "filesystem", allowedDomains/deniedDomains under "network". Do NOT flatten those keys to the sandbox top level. Then pick values that fit the intent:
- Web / research / fetching: enabled, no filesystem writes, allowedDomains scoped to relevant hosts.
- Coding / git / build: enabled, allowWrite the project root, allowedDomains for the relevant package registries and git host.
- Data / SQL / analytics: enabled, allowWrite scoped to output dirs, allowedDomains for DB hosts / cloud APIs.
- Planning / read-only / review: enabled, no allowWrite, minimal allowedDomains.
Default protections regardless of intent: enabled=true, denyWrite includes ["~/.ssh","~/.aws","~/.config/gh"], denyRead hides private keys. Only deviate if the intent explicitly requires it (then state why in rationale).

You have WebFetch. Use it.

REQUIRED: for EVERY MCP server you intend to recommend, fetch the vendor's official docs to verify (a) the current endpoint URL or install command and (b) the authentication mechanism (API key headers, OAuth, none, …). Do NOT rely on training-data assumptions for endpoints or auth — vendors change them, and getting either wrong leaves the profile broken.

Preference order — always pick the most authentication-friendly option the vendor offers:
1. Cloud-hosted HTTP MCP with OAuth (no local install, no API keys in env) — STRONGLY preferred when available.
2. Cloud-hosted HTTP MCP with API key headers.
3. Local stdio MCP with OAuth.
4. Local stdio MCP with API key / token via env vars — last resort.
If the vendor offers OAuth via a cloud-hosted endpoint, use it; do not fall back to a stdio CLI or API-key path just because the stdio variant is mentioned in the docs. OAuth + HTTP is the default.

You may also use these for discovery / settings reference:
- https://github.com/modelcontextprotocol/servers
- https://github.com/punkpeye/awesome-mcp-servers
- https://docs.claude.com/en/docs/claude-code/mcp
- https://docs.claude.com/en/docs/claude-code/settings
- https://docs.claude.com/en/docs/claude-code/plugins
Skip discovery if the intent already names the services; per-server verification fetches are still required.

Output protocol — your final reply MUST be a single JSON object, nothing else.

Two valid shapes:

1) If clarification is needed:
   {"questions": [{"key": "<short_id>", "prompt": "<human question>", "options": ["opt1","opt2"]}]}
   At most 3 questions per round. "options" is optional (free-text if omitted). Only ask things you genuinely need to decide; do not ask about MCP server identity if you can pick a sensible default.

2) When ready:
   {"profile": {...}, "rationale": "<2-3 sentence explanation of choices>"}
   The "rationale" field is REQUIRED — explain why you picked these MCP servers and settings so the user can sanity-check.

Prefer producing a profile in 1 round when possible. Hard cap: 3 question rounds.`

func buildGeneratorPrompt(history []genTurn) string {
	var sb strings.Builder
	sb.WriteString(generatorSystemPrompt)
	sb.WriteString("\n\n--- Conversation so far ---\n")
	for i, t := range history {
		fmt.Fprintf(&sb, "\n[turn %d · %s]\n%s\n", i+1, t.role, t.content)
	}
	sb.WriteString("\n--- End conversation ---\n\nReply now with the JSON object only.")
	return sb.String()
}

func marshalAssistantTurn(r *genResponse) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func marshalAnswers(answers map[string]string) string {
	b, _ := json.Marshal(map[string]any{"answers": answers})
	return string(b)
}

// parseGenResponse extracts a JSON object from the model output. The envelope
// spec says "no markdown", but models occasionally wrap in ```json. Pull the
// first `{` to its matching `}` and unmarshal that.
func parseGenResponse(raw string) (*genResponse, error) {
	jsonText := extractJSONObject(raw)
	if jsonText == "" {
		return nil, fmt.Errorf("no JSON object found in response: %s", trim(raw, 200))
	}
	var resp genResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON envelope: %w (raw: %s)", err, trim(jsonText, 300))
	}
	return &resp, nil
}

// extractJSONObject scans `raw` for the first balanced {...} object, ignoring
// braces inside string literals. Returns "" if none found.
func extractJSONObject(raw string) string {
	start := strings.Index(raw, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1]
			}
		}
	}
	return ""
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Recap (HTML) ─────────────────────────────────────────────────────────────

func showRecap(p *Profile, rationale string) error {
	htmlText, err := renderRecapHTML(p, rationale)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "claude-profile-recap-*.html")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(htmlText); err != nil {
		f.Close()
		return err
	}
	f.Close()
	info("Recap → %s", f.Name())
	// Best-effort open; user can read in terminal if it fails.
	_ = exec.Command("open", f.Name()).Start()
	return nil
}

func renderRecapHTML(p *Profile, rationale string) (string, error) {
	profileJSON, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}

	var servers strings.Builder
	for name, cfg := range p.McpServers {
		fmt.Fprintf(&servers, "<tr><td><code>%s</code></td><td>%s</td><td><code>%s</code></td></tr>",
			html.EscapeString(name),
			html.EscapeString(cfg.Type),
			html.EscapeString(serverTarget(cfg)))
	}
	if servers.Len() == 0 {
		servers.WriteString("<tr><td colspan=3><em>(no MCP servers)</em></td></tr>")
	}

	var settingsBlock string
	if len(p.Settings) > 0 {
		var pretty []byte
		var tmp any
		if json.Unmarshal(p.Settings, &tmp) == nil {
			pretty, _ = json.MarshalIndent(tmp, "", "  ")
		} else {
			pretty = []byte(p.Settings)
		}
		settingsBlock = fmt.Sprintf("<h2>Settings</h2><pre>%s</pre>", html.EscapeString(string(pretty)))
	}

	var deniedBlock string
	if len(p.DeniedTools) > 0 {
		deniedBlock = fmt.Sprintf(
			"<h2>Denied tools</h2><p><code>%s</code></p>",
			html.EscapeString(strings.Join(p.DeniedTools, ", ")))
	}

	rationaleBlock := ""
	if rationale != "" {
		rationaleBlock = fmt.Sprintf("<div class=\"rationale\"><strong>Rationale.</strong> %s</div>",
			html.EscapeString(rationale))
	}

	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>Profile recap</title>
<style>
  :root { --coral: #C96442; --cream: #F0EEE6; --ink: #1F1F1F; --muted: #6B6B6B; }
  body { font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         color: var(--ink); background: var(--cream); margin: 0;
         padding: 2rem 3rem; max-width: 880px; margin: 0 auto; }
  h1 { color: var(--coral); border-bottom: 2px solid var(--coral); padding-bottom: .3em; }
  h2 { color: var(--coral); margin-top: 1.8em; }
  .rationale { background: #fff; border-left: 4px solid var(--coral);
               padding: 1rem 1.2rem; margin: 1.5rem 0; border-radius: 4px; }
  .desc { font-size: 1.05em; color: var(--muted); margin: 0 0 1.5rem; }
  table { border-collapse: collapse; width: 100%%; margin-top: .8rem; }
  th, td { text-align: left; padding: .5rem .7rem; border-bottom: 1px solid #ddd; }
  th { color: var(--muted); font-weight: 600; font-size: .85em; text-transform: uppercase; }
  code { font: 13px/1 SFMono-Regular, Menlo, monospace; background: #fff;
         padding: 1px 5px; border-radius: 3px; }
  pre { background: #fff; padding: 1rem; border-radius: 6px; overflow-x: auto;
        border: 1px solid #ddd; font: 13px/1.45 SFMono-Regular, Menlo, monospace; }
  footer { color: var(--muted); margin-top: 2rem; font-size: .9em;
           border-top: 1px solid #ddd; padding-top: 1rem; }
</style></head><body>
<h1>Profile Recap</h1>
<p class="desc">%s</p>
%s
<h2>MCP servers</h2>
<table><thead><tr><th>name</th><th>type</th><th>target</th></tr></thead><tbody>%s</tbody></table>
%s
%s
<h2>Full JSON</h2>
<pre>%s</pre>
<footer>Return to your terminal to accept or discard this profile.</footer>
</body></html>`,
		html.EscapeString(p.Description),
		rationaleBlock,
		servers.String(),
		settingsBlock,
		deniedBlock,
		html.EscapeString(string(profileJSON))), nil
}

func serverTarget(c ServerConfig) string {
	if c.URL != "" {
		return c.URL
	}
	if c.Command != "" {
		if len(c.Args) > 0 {
			return c.Command + " " + strings.Join(c.Args, " ")
		}
		return c.Command
	}
	return ""
}
