# kb-curator — Claude Code plugin

Curates Claude Code transcript Stop events and git commits into a durable, per-repo
knowledge base (`.kb/`) and Claude's auto-memory. Two skills (`kb`, `memory`) write
the actual entries; an orchestrator agent decides what's worth keeping.

The plugin is **standalone**: no Go toolchain, no external services, only Python 3.8+
(stdlib only) and `git`.

## Install

Drop this directory anywhere Claude Code loads plugins from (typically
`~/.claude/plugins/`), or pass `--plugin-dir <path-to-this-plugin>` when launching
`claude`. The plugin contributes one agent, two skills, one Monitor, and one Stop
hook (KB index regeneration).

## Two modes

The Monitor process (`scripts/kb-tail.py`) auto-selects mode based on env vars:

### Repo mode (default — no env vars needed)

Each repo gets its own `.kb/` at the main repo root.

- Watches transcripts for sessions whose cwd is inside this repo (every worktree).
- Watches `git log` of the main checkout for new commits.
- Curator agent reads `./CLAUDE.md` and the per-repo auto-memory as duplication baseline.

### Global mode (personal cross-project KB)

A single `.kb/` collects events from every project on this machine. Set both env vars
before launching Claude Code:

```sh
export KB_TAIL_DIR="$HOME/.kb"           # wherever you want the KB
export KB_TAIL_SCAN_ALL_PROJECTS=1       # scan all ~/.claude/projects/*/
```

Mode auto-applies:

- All `~/.claude/projects/*/` encoded transcript dirs are scanned.
- **Commits are skipped** — a cross-project KB has no single repo to track.
- Each event carries `source_cwd` (the precise cwd of the originating session),
  read from the transcript jsonl itself. The curator uses it to group entries by
  project and pass context to skills.

You can edit `.kb/.focus` (top-level inside `KB_TAIL_DIR/.kb/`) to write a 1–3 line
lens like *"my work progress"* — the curator filters out-of-focus events upfront.

## Env reference

| Var | Effect |
| --- | --- |
| `KB_TAIL_DIR` | Path that owns `.kb/`. Required for global mode; overrides git auto-detect in repo mode. |
| `KB_TAIL_SCAN_ALL_PROJECTS` | `1` / `true` / `yes` enables global mode. |
| `KB_TAIL_SELF_AGENT` | CSV of agent names whose transcripts to ignore (self-curation guard). |

CLI flags on `kb-tail.py` (`--kb-dir`, `--scan-all-projects`, `--self-agent` — repeatable)
override env vars when both are set.

## Files

- `agents/kb-curator.md` — orchestrator agent prompt.
- `skills/kb/SKILL.md` — writes `.kb/{decisions,fixes,sessions}/` entries.
- `skills/memory/SKILL.md` — routes behavioral context across the five memory surfaces.
- `monitors/monitors.json` — declares the `kb-tail` Monitor process.
- `scripts/kb-tail.py` — the Monitor itself.
- `scripts/kb-index.sh` — Stop hook that regenerates `.kb/INDEX.md` files.
- `hooks/hooks.json` — registers the Stop hook.

## Smoke test

Run from anywhere inside a git repo:

```sh
bash <path-to-this-repo>/scripts/smoke-kb-tail.sh
```

Covers: cold start (no retroactive emit), Stop + commit detection, restart
idempotency, self-agent filtering, worktree unification, global mode + source_cwd.
