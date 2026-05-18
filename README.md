# claude-profiles

A wrapper around the `claude` CLI that lets you keep named **profiles** — each one a bundle of MCP servers, allowed/denied tools, model + permission settings, hooks, plugins, and pinned prompts — and launch Claude with the right bundle for the task at hand.

Think `nvm use` but for your Claude sessions.

---

## Install

The repo is private, so set `GOPRIVATE` once:

```bash
export GOPRIVATE=github.com/gabe4coding/*
# (Optional) make sure git uses SSH for that host
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Then install the binary:

```bash
go install github.com/gabe4coding/claude-profiles-test/cmd/claude-profiles@latest
```

The binary lands in `$(go env GOBIN)` (falls back to `$GOPATH/bin`, usually `~/go/bin`). Make sure that's on your `PATH`.

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
go install github.com/gabe4coding/claude-profiles-test/cmd/claude-profiles@latest
claude-profiles version
```

If something looks off after an upgrade, `claude-profiles doctor` is the first stop.
