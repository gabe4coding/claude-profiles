#!/usr/bin/env python3
"""SessionStart hook: surface the curated .kb/ to the assistant if one exists.

Fires once per Claude Code session start when the kb-curator plugin is
loaded. Emits a JSON object on stdout with hookSpecificOutput.additionalContext
that tells the assistant a curated knowledge base is available for this
repo, plus an inline copy of the top-level INDEX.md and a head of the
tag index (full file lives at .kb/INDEX-by-tag.md for on-demand read).

Bail conditions (silent — print nothing, exit 0):
  - no git repo and no KB_TAIL_DIR override
  - .kb/INDEX.md missing (kb-curator hasn't curated anything yet)

KB root resolution mirrors kb-tail.py / kb-index.sh:
  1. KB_TAIL_DIR — explicit override (global personal KB or any decoupled use)
  2. CLAUDE_PROJECT_DIR — set by Claude Code when the hook fires
  3. main repo root via `git rev-parse --git-common-dir`

The tag index head is truncated to ~30 lines to bound the per-session
context cost. The assistant can read the full file when it needs to.

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


def resolve_kb_dir() -> Path | None:
    """Resolve the KB directory itself (the dir containing INDEX.md,
    decisions/, fixes/, sessions/, inbox/, .focus).

    Priority:
      1. KB_TAIL_DIR — IS the kb dir directly. No `.kb` appended; the
         user picked an explicit path.
      2. CLAUDE_PROJECT_DIR — project root set by Claude Code for hooks;
         append `.kb` (project-mode convention).
      3. main repo root via git — append `.kb`.
    """
    if env := os.environ.get("KB_TAIL_DIR"):
        return Path(env)
    if env := os.environ.get("CLAUDE_PROJECT_DIR"):
        return Path(env) / ".kb"
    rr = main_repo_root_from_cwd()
    if rr is None:
        return None
    return Path(rr) / ".kb"


def read_active_lens(kb_dir: Path) -> str:
    """Extract a curator lens from <kb>/.focus.

    Returns the non-comment / non-empty content joined as a single block,
    or "" when no active lens (file missing, blank, or only comments, or
    the literal word `none`). Always read from the resolved KB root —
    callers must NOT rely on the agent's cwd, which in global mode points
    elsewhere than the KB itself.
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
        if stripped.lower() == "none":
            return ""
        lines.append(stripped)
    return "\n".join(lines).strip()


def build_context(kb_dir: Path) -> str:
    index = kb_dir / "INDEX.md"
    parts = [
        f"A curated knowledge base exists at `{kb_dir}/`. Consult it before"
        " answering questions about this repo's decisions, fixes, or past"
        " sessions — the entries capture rationale, invariants, and lessons"
        " that the codebase itself cannot tell on its own.",
        "",
        index.read_text(encoding="utf-8"),
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
        head_lines = by_tag.read_text(encoding="utf-8").splitlines()[:TAG_INDEX_HEAD_LINES]
        parts.append("")
        parts.append("### Tag index (head)")
        parts.append("Full list at `.kb/INDEX-by-tag.md`. Use these tag names"
                     " when reasoning about coverage.")
        parts.append("")
        parts.extend(head_lines)
    parts.append("")
    parts.append(
        "When searching the KB: scan `.kb/INDEX.md` for buckets, then"
        " `.kb/INDEX-by-tag.md` for cross-bucket topics, then open the"
        " specific entry. Cross-links are written as `[[entry-slug]]` —"
        " resolve them by basename under the matching bucket dir."
    )
    return "\n".join(parts)


def main() -> int:
    kb_dir = resolve_kb_dir()
    if kb_dir is None:
        return 0
    if not (kb_dir / "INDEX.md").is_file():
        return 0
    context = build_context(kb_dir)
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
