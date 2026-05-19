# claude-profiles

**Switch Claude's MCP servers, model, permission mode, hooks, and prompts in one keystroke — without restarting your session.**

`claude` is one binary, but the way you want to use it changes by the hour: a tight allow-list while editing prod code, a wide-open agent loop while prototyping, a Jira+GitHub MCP combo when triaging, a clean isolated config when reviewing a teammate's plugin. claude-profiles lets you bundle each of those into a named **profile** and pick the right one at launch — or swap mid-conversation with `/switch` and keep your context.

Think `nvm use`, but for everything around `claude`.

---

## What makes it different

**1. `/switch` mid-conversation — change the toolbox without losing the thread.**
Type `/switch release-notes` inside any session and claude resumes the *same* conversation under a different profile: different model, different MCP servers, different permission mode. Start in "explore" mode, harden into "ship" mode without re-explaining context.

**2. `ask` — fuzzy launch by intent, not by name.**
```bash
claude-profiles ask "diagnose this RabbitMQ binding"
```
Classifies the prompt against every profile you have, picks the best match, launches it with the prompt pre-filled. Drops the "which profile was this again?" tax.

**3. Profiles as a shared git repo — your team's setup in one command.**
```bash
claude-profiles repo add git@github.com:acme/claude-profiles.git --alias acme
claude-profiles acme/release-notes
```
Auto-syncs every 5 min. `copy` a teammate's profile to fork it locally — the original stays read-only. Or commit a project-scoped profile at `<repo>/.claude-profiles/<name>/` and it auto-appears for anyone running `claude-profiles` in that checkout.

Plus: per-launch git **worktrees** (parallel agents without stepping on each other), **isolated** mode (clean `~/.claude` for safe testing), a session **distill** hook, **analytics** on context-window burn and cache hit rate, and a TUI **hub** where pin / edit / copy / export are single keystrokes.

---

## Who this is for

- **Solo developer juggling multiple stacks** — one profile per `(repo, mode)` combo, `cwd` pins each to the right directory, the hub becomes your launcher.
- **Teams that want reproducible Claude setup** — commit profiles to a shared git repo or directly into `.claude-profiles/` in the project. Onboarding becomes `repo add`.
- **MCP power-users** — per-profile MCP servers with per-server allow/deny lists, `probe` for raw error messages when a server breaks, `analytics` to spot which servers are inflating your context.

---

## Install

The repo is private, so do this once — tell `go` to skip the public proxy, and tell `git` to use SSH for `github.com`:

```bash
export GOPRIVATE=github.com/gabe4coding/*
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Then install:

```bash
go install github.com/gabe4coding/claude-profiles/cmd/claude-profiles@latest
```

Make sure `~/go/bin` is on your `PATH`.

Sanity check:

```bash
claude-profiles version
claude-profiles doctor    # verifies claude binary, hooks, profile dirs
```

### Shell completion (optional but worth it)

```bash
# zsh
eval "$(claude-profiles completion zsh)"

# bash
eval "$(claude-profiles completion bash)"
```

Drop the `eval` line into your `~/.zshrc` / `~/.bashrc`.

---

## Quick start

```bash
# 1. Open the interactive hub (the main entry point)
claude-profiles

# 2. Or create a profile from scratch
claude-profiles new

# 3. List what you have
claude-profiles list

# 4. Launch claude with a specific profile
claude-profiles dev-toolkit
#   ↑ shorthand for `claude-profiles run dev-toolkit`

# 5. Pass extra args straight through to claude
claude-profiles dev-toolkit --resume <session-id>
claude-profiles dev-toolkit -p "summarise this repo"
```

The **hub** is where most of the day-to-day work happens: it lists every profile (local + project + shared repos), and single-key shortcuts launch, pin, edit, delete, copy, export, import, or open analytics.

---

## What lives in a profile

A profile is a directory under `~/.claude-profiles/<name>/` containing a `profile.json` plus optional `prompts/`, plugin assets, and per-profile state. Each profile can carry any combination of:

| Concept           | What it does                                                                  |
| ----------------- | ----------------------------------------------------------------------------- |
| **MCP servers**   | stdio or HTTP servers wired into the session                                  |
| **Allow / deny**  | Per-server tool filters — keep only what you actually want exposed            |
| **Settings**      | Model, permission mode, env vars, hooks, statusLine, plugins (full settings.json overlay) |
| **Prompts**       | Reusable starter prompts you can pin to the hub                               |
| **Isolated**      | Run claude with a clean `~/.claude` so the profile doesn't see your defaults  |
| **Worktree**      | Auto-create a git worktree for each launch (great for parallel agents)        |
| **Distill**       | Run the session-distillation hook at stop                                     |
| **Cwd**           | Pin the working directory so the profile always lands in the right repo      |

Inspect any profile in detail:

```bash
claude-profiles show dev-toolkit
```

---

## Daily commands

```bash
claude-profiles                 # hub
claude-profiles <name>          # launch (shorthand for `run`)
claude-profiles run <name>      # launch with the wrapper loop (enables /switch)
claude-profiles exec <name> …   # replace this process with claude (CI / scripts)
claude-profiles ask "<prompt>"  # classify the prompt → best profile → launch it
claude-profiles list            # list all profiles (local + project + repos)
claude-profiles show <name>     # detailed view of one profile
claude-profiles new             # interactive create
claude-profiles edit <name>     # interactive edit (tools, settings, prompts, plugins, $EDITOR)
claude-profiles delete <name>   # remove a local profile
claude-profiles copy alias/name # copy a shared repo profile down to local
claude-profiles export <name>   # print profile JSON to stdout
claude-profiles import file.json
claude-profiles probe <name> [server]  # diagnose an MCP server (raw error + tool list)
claude-profiles doctor          # environment + config sanity checks
claude-profiles analytics       # context-window usage, cache stats, recommendations
```

### Switching profiles **inside** a session

`claude-profiles run` (and the hub launch) wrap claude in a small loop. Inside the session you can type:

- `/switch <name>` — swap to another profile and `--resume` the current conversation under it
- `/switch` — open the picker

This is the killer feature: change MCP servers, model, or permission mode mid-conversation without losing context.

---

## Sharing profiles across machines / teammates

Register a git repo full of profiles and they show up alongside your local ones:

```bash
claude-profiles repo add git@github.com:acme/claude-profiles.git --alias acme
claude-profiles repo list
claude-profiles repo sync           # foreground refresh (auto-sync runs every 5min)
claude-profiles repo remove acme
```

Launch a shared profile by its qualified ID:

```bash
claude-profiles acme/release-notes
```

Want to tweak someone else's profile? Copy it down — the original stays read-only:

```bash
claude-profiles copy acme/release-notes my-release-notes
claude-profiles edit my-release-notes
```

### Project-scoped profiles

Drop a profile directory at `<repo-root>/.claude-profiles/<name>/` and it auto-appears when you run `claude-profiles` from inside that repo — tagged `[project]`. Useful for committing a team-shared profile alongside the code it operates on.

---

## Tips & gotchas

- **The hub is the entry point.** Most flows (edit, pin, analytics, repo browser, export/import) are one keystroke away from the hub — don't memorise subcommands you can press a key for.
- **Pin your favourites.** From the hub, press the pin key on a profile (optionally with a starter prompt) and it floats to the top across sessions.
- **`ask` for fuzzy launch.** `claude-profiles ask "diagnose this RabbitMQ binding"` will classify the prompt against all your profiles and launch the best match with the prompt pre-filled.
- **`exec` for CI/automation.** No tmux, no wrapper loop, no hooks — just `claude` with the profile's flags applied. Args after the profile pass through verbatim:
  ```bash
  claude-profiles exec my-profile -p "do the thing"
  ```
- **Skip tmux** with `--no-tmux` or `CLAUDE_PROFILES_NO_TMUX=1` if you're driving claude from a non-interactive shell or just don't want a tmux session.
- **Move the whole config** with `CLAUDE_PROFILES_ROOT=/path claude-profiles` — handy for sandboxing or per-laptop overrides.
- **Isolated mode** is the safest way to test a fresh profile — it bypasses your global `~/.claude/CLAUDE.md`, hooks, and skills, so you see exactly what the profile defines and nothing more.
- **Worktree mode** spins up a fresh git worktree on each launch, so running three agents on the same repo doesn't have them stepping on each other.
- **`probe`** prints the raw MCP error when a server fails to load — much more useful than the truncated message you see in the edit menu.
- **`analytics`** flags profiles burning context. If your cache hit rate is poor on a profile, that's your cue to trim its allow-list or split it.

---

## Updating

```bash
go install github.com/gabe4coding/claude-profiles/cmd/claude-profiles@latest
claude-profiles version
```

If something looks off after an upgrade, `claude-profiles doctor` is the first stop.
