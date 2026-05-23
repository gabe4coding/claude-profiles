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


def resolve_kb_root() -> str | None:
    if env := os.environ.get("KB_TAIL_DIR"):
        return env
    if env := os.environ.get("CLAUDE_PROJECT_DIR"):
        return env
    return main_repo_root_from_cwd()


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
    root = resolve_kb_root()
    if not root:
        return 0
    kb_dir = Path(root) / ".kb"
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
