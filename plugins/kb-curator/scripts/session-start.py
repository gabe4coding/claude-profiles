#!/usr/bin/env python3
"""SessionStart hook: surface every curated .kb/ relevant to this session.

Fires once per Claude Code session start when the kb-curator plugin is
loaded. Emits a JSON object on stdout with hookSpecificOutput.additionalContext.

Two KBs may be surfaced, in order:

1. LOCAL KB — resolved for the session's cwd:
   priority KB_TAIL_DIR → CLAUDE_PROJECT_DIR/.kb → main-repo-root/.kb.
   Full content: pointer + INDEX.md + active .focus lens + first ~30 lines
   of INDEX-by-tag.md.

2. PERSONAL CROSS-PROJECT KB — discovered via the canonical
   `$HOME/.kb/.kb-tail.env` file (or `$KB_TAIL_ENV_FILE` override) that
   `kb-global` writes. If that file declares a KB_TAIL_DIR distinct from
   the local one AND that location has an INDEX.md, surface it as a
   compact secondary block (pointer + INDEX only). This lets a normal
   non-curator session running in some repo also know that the user's
   personal cross-project KB is queryable on demand.

Bail conditions (silent — print nothing, exit 0):
  - no local KB resolvable (no git, no CLAUDE_PROJECT_DIR, no env) AND
    no global KB declared either.

Stdlib only. Python 3.8+.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path


TAG_INDEX_HEAD_LINES = 30


def main_repo_root_from_cwd() -> str | None:
    """Resolve main repo root from cwd (unifies worktrees)."""
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "--git-common-dir"],
            stderr=subprocess.DEVNULL,
        ).decode("utf-8", errors="replace").strip()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        return None
    if not out:
        return None
    p = Path(out)
    if not p.is_absolute():
        p = Path.cwd() / p
    return str(p.parent.resolve())


def resolve_local_kb_dir() -> Path | None:
    """KB for the session's cwd. See module docstring for priority."""
    if env := os.environ.get("KB_TAIL_DIR"):
        return Path(env)
    if env := os.environ.get("CLAUDE_PROJECT_DIR"):
        return Path(env) / ".kb"
    rr = main_repo_root_from_cwd()
    if rr is None:
        return None
    return Path(rr) / ".kb"


def resolve_global_kb_dir() -> Path | None:
    """Read the canonical env file written by `kb-global` and extract
    KB_TAIL_DIR — the user's declared personal cross-project KB.

    File location priority:
      1. $KB_TAIL_ENV_FILE  — explicit override (also honored by the
         Monitor wrapper, kept consistent here)
      2. $HOME/.kb/.kb-tail.env  — canonical default
    """
    candidates: list[Path] = []
    if v := os.environ.get("KB_TAIL_ENV_FILE"):
        candidates.append(Path(v))
    candidates.append(Path.home() / ".kb" / ".kb-tail.env")
    for env_file in candidates:
        if not env_file.is_file():
            continue
        try:
            raw = env_file.read_text(encoding="utf-8")
        except OSError:
            continue
        for line in raw.splitlines():
            line = line.strip()
            if line.startswith("export "):
                line = line[len("export "):].strip()
            if not (line.startswith("KB_TAIL_DIR=") or line.startswith("KB_TAIL_DIR ")):
                continue
            _, _, value = line.partition("=")
            value = value.strip().strip("\"'")
            # Expand shell variable references ($HOME / ${HOME}) that are
            # common in hand-written env files.  os.path.expandvars() is
            # used so that only complete variable tokens are substituted
            # (e.g. $HOMEPATH is left untouched) and ${VAR} syntax is also
            # handled correctly.
            value = os.path.expandvars(value)
            if value:
                return Path(value)
        # File found but no KB_TAIL_DIR line — stop searching further
        # candidates (the user picked this file, leave it authoritative).
        return None
    return None


def read_active_lens(kb_dir: Path) -> str:
    """Extract a curator lens from <kb>/.focus. Returns "" when no active
    lens (file missing, blank, only comments, or literal `none`).

    `none` is treated as a kill-switch regardless of where it appears in
    the file — any non-comment line equal to `none` (case-insensitive)
    anywhere in the accumulated content disables the lens entirely.
    """
    focus = kb_dir / ".focus"
    if not focus.is_file():
        return ""
    try:
        raw = focus.read_text(encoding="utf-8")
    except OSError:
        return ""
    lines: list[str] = []
    for line in raw.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        lines.append(stripped)
    # Kill-switch: if the combined non-comment content is solely "none",
    # the lens is inactive — checked after collecting all lines so that
    # position doesn't matter.
    combined = "\n".join(lines).strip()
    if combined.lower() == "none":
        return ""
    return combined


def build_local_block(kb_dir: Path) -> str:
    """Full-detail block for the session-local KB: intro + INDEX + lens +
    tag index head + retrieval hint."""
    index = kb_dir / "INDEX.md"
    try:
        index_text = index.read_text(encoding="utf-8")
    except OSError:
        return ""
    parts = [
        f"A curated knowledge base exists at `{kb_dir}/`. Consult it before"
        " answering questions about this repo's decisions, fixes, or past"
        " sessions — the entries capture rationale, invariants, and lessons"
        " that the codebase itself cannot tell on its own.",
        "",
        index_text,
    ]
    lens = read_active_lens(kb_dir)
    if lens:
        parts.append("")
        parts.append("### Active curation lens")
        parts.append(
            "The user has narrowed this KB's scope. Apply this lens when"
            " classifying inbox events: drop out-of-focus events upfront,"
            " and pass the lens text to any skill you invoke so it can"
            " apply its own restrictive secondary check. The lens is"
            " intentionally restrictive — when in doubt, skip."
        )
        parts.append("")
        parts.append("```")
        parts.append(lens)
        parts.append("```")
    by_tag = kb_dir / "INDEX-by-tag.md"
    if by_tag.is_file():
        try:
            head_lines = by_tag.read_text(encoding="utf-8").splitlines()[:TAG_INDEX_HEAD_LINES]
        except OSError:
            head_lines = []
        if head_lines:
            parts.append("")
            parts.append("### Tag index (head)")
            parts.append("Full list at `INDEX-by-tag.md`. Use these tag names"
                         " when reasoning about coverage.")
            parts.append("")
            parts.extend(head_lines)
    parts.append("")
    parts.append(
        "When searching: scan `INDEX.md` for buckets, then `INDEX-by-tag.md`"
        " for cross-bucket topics, then open the specific entry. Cross-links"
        " are written as `[[entry-slug]]` — resolve them by basename under"
        " the matching bucket dir."
    )
    return "\n".join(parts)


def build_global_block(kb_dir: Path) -> str:
    """Compact block for the personal cross-project KB. Just enough for
    the agent to know it exists and how to dive in — full content is read
    on demand to keep the per-session token cost bounded."""
    index = kb_dir / "INDEX.md"
    try:
        index_text = index.read_text(encoding="utf-8")
    except OSError:
        return ""
    parts = [
        f"### Personal cross-project KB at `{kb_dir}/`",
        "",
        "The user maintains a personal wiki here that aggregates curated"
        " entries from work across every project on this machine — patterns"
        " they reuse, lessons learned debugging, references to external"
        " systems, durable project state, and chronological work history."
        " Read it when the user asks about cross-project knowledge, prior"
        " patterns, or what they did in another repo.",
        "",
        index_text,
        "",
        "Cross-bucket topics live in `INDEX-by-tag.md` (read on demand).",
    ]
    return "\n".join(parts)


def main() -> int:
    blocks: list[str] = []

    local = resolve_local_kb_dir()
    if local is not None and (local / "INDEX.md").is_file():
        block = build_local_block(local)
        if block:
            blocks.append(block)

    global_kb = resolve_global_kb_dir()
    if (
        global_kb is not None
        and global_kb != local
        and (global_kb / "INDEX.md").is_file()
    ):
        block = build_global_block(global_kb)
        if block:
            blocks.append(block)

    if not blocks:
        return 0

    context = "\n\n---\n\n".join(blocks)
    payload = {
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": context,
        }
    }
    print(json.dumps(payload))
    return 0


if __name__ == "__main__":
    sys.exit(main())
