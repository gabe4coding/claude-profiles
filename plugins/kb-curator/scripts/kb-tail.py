#!/usr/bin/env python3
"""kb-tail: tail Claude Code transcript jsonl + git log into a curation inbox.

Plugin-agnostic Monitor process. A companion curator agent (shipped with this
plugin) drains the inbox and writes durable entries under
<kb-dir>/.kb/{decisions,fixes,sessions}/ via skills.

Run modes:

1. Repo mode (default). The .kb/ lives at the main repo root resolved via
   `git rev-parse --git-common-dir` (worktrees unify to a single .kb/ at
   the main root). Transcripts of every worktree are scanned. Commits to
   main's HEAD are watched.

2. Global mode (--scan-all-projects / KB_TAIL_SCAN_ALL_PROJECTS=1). The
   .kb/ lives at an explicit path supplied via --kb-dir / KB_TAIL_DIR.
   All ~/.claude/projects/*/ encoded transcript dirs are scanned. Git
   ops are skipped: a personal cross-project KB has no single repo to
   watch and per-repo commit enumeration is out of scope.

Config precedence: CLI flags win over env vars. Env vars:
  KB_TAIL_DIR                 → --kb-dir
  KB_TAIL_SCAN_ALL_PROJECTS=1 → --scan-all-projects
  KB_TAIL_SELF_AGENT=a,b,c    → --self-agent a --self-agent b --self-agent c

Invariants (both modes):
- never writes to stdout except one short ping line per emitted event;
- never crashes on transient errors (only stderr + continue);
- on first run records current state without emitting historical events;
- inbox state survives restart via <kb-dir>/.kb/inbox/.kb-tail-state.json.

Stdlib only. Python 3.8+.
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


# ----------------------------- config --------------------------------------


def parse_config(argv: list[str]) -> argparse.Namespace:
    """Resolve CLI + env into a single config. CLI flags override env vars.

    --self-agent can be passed multiple times and accumulates with values
    from KB_TAIL_SELF_AGENT (comma-separated).
    """
    parser = argparse.ArgumentParser(
        prog="kb-tail",
        description="Tail Claude Code transcripts + git into a KB inbox.",
        add_help=True,
    )
    parser.add_argument(
        "--kb-dir",
        default=os.environ.get("KB_TAIL_DIR", "") or None,
        help="Directory that owns .kb/ (defaults to main repo root in repo mode; "
        "REQUIRED in global mode).",
    )
    parser.add_argument(
        "--scan-all-projects",
        action="store_true",
        default=_env_bool(os.environ.get("KB_TAIL_SCAN_ALL_PROJECTS")),
        help="Global mode: scan ~/.claude/projects/*/ instead of git worktrees, "
        "and skip git commit watching.",
    )
    parser.add_argument(
        "--self-agent",
        action="append",
        default=[],
        help="Agent name(s) whose transcripts kb-tail must ignore (repeatable). "
        "Prevents a curator agent from feeding its own end_turn events back "
        "into its own inbox.",
    )

    ns = parser.parse_args(argv)

    # Merge env CSV into the --self-agent list.
    env_csv = os.environ.get("KB_TAIL_SELF_AGENT", "")
    if env_csv:
        ns.self_agent = list(ns.self_agent) + [
            s.strip() for s in env_csv.split(",") if s.strip()
        ]
    ns.self_agent = set(ns.self_agent)

    if ns.scan_all_projects and not ns.kb_dir:
        parser.error(
            "--scan-all-projects requires --kb-dir or KB_TAIL_DIR "
            "(no implicit location for a global KB)"
        )
    return ns


def _env_bool(s: str | None) -> bool:
    if s is None:
        return False
    return s.strip().lower() in {"1", "true", "yes", "on"}


# ----------------------------- git helpers ---------------------------------


def git(*args: str, cwd: str | None = None) -> str:
    """Run `git <args>` and return trimmed stdout. Empty string on error."""
    try:
        out = subprocess.check_output(
            ["git", *args],
            cwd=cwd,
            stderr=subprocess.DEVNULL,
        )
    except (subprocess.CalledProcessError, FileNotFoundError):
        return ""
    return out.decode("utf-8", errors="replace").strip()


def main_repo_root_from_cwd() -> str | None:
    """Resolve the MAIN repo root from cwd, even inside a linked worktree.

    `git rev-parse --git-common-dir` returns the shared common dir
    (typically `<main>/.git`); its parent is the main repo root. Caveat:
    --separate-git-dir layouts (common dir outside the work tree) would
    resolve to the wrong parent — not supported.
    """
    common = git("rev-parse", "--git-common-dir")
    if not common:
        return None
    p = Path(common)
    if not p.is_absolute():
        p = Path.cwd() / p
    return str(p.parent.resolve())


def list_worktrees(main_root: str) -> list[str]:
    """All worktrees (main + linked) of the repo at main_root.

    Re-queried each tick so worktrees added at runtime are picked up.
    """
    out = git("worktree", "list", "--porcelain", cwd=main_root)
    if not out:
        return [main_root]
    paths = [
        line.removeprefix("worktree ")
        for line in out.splitlines()
        if line.startswith("worktree ")
    ]
    return paths or [main_root]


def new_commits_since(from_sha: str, to_sha: str, cwd: str) -> list[str]:
    """Linear chain of commits in (from..to], oldest first."""
    if not from_sha:
        return []
    out = git("rev-list", "--reverse", f"{from_sha}..{to_sha}", cwd=cwd)
    if not out:
        return []
    return [line.strip() for line in out.splitlines() if line.strip()]


# ----------------------------- transcript helpers --------------------------


def transcripts_dir_for(cwd: str) -> Path:
    """The on-disk directory where Claude Code stores transcripts for a cwd.

    Claude Code encodes the cwd by replacing every '/' and '.' with '-'.
    Lossy: it cannot be reliably reversed (which is why we probe the
    transcript itself for the cwd field — see read_transcript_cwd).
    """
    home = Path.home()
    enc = cwd.translate(str.maketrans({"/": "-", ".": "-"}))
    return home / ".claude" / "projects" / enc


def read_transcript_cwd(path: Path) -> str:
    """Probe the first ~30 records for any record with a "cwd":"…" field.

    Claude Code writes cwd into nearly every record (attachment, user,
    assistant, …) so the first matching record is sufficient. Returns ""
    on read errors or when no cwd is found within the probe budget.

    Why probe and cache: the encoded dir name (transcripts_dir_for) is a
    LOSSY transform of the cwd ('/' and '.' both map to '-'), so reading
    the transcript is the only precise way to recover the source cwd —
    used as source_cwd on emitted events for global-mode grouping.
    """
    try:
        with path.open("r", encoding="utf-8", errors="replace") as f:
            for i, line in enumerate(f):
                if i >= 30:
                    break
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(rec, dict) and rec.get("cwd"):
                    return str(rec["cwd"])
    except OSError:
        return ""
    return ""


def is_self_agent_transcript(path: Path, self_agents: set[str]) -> bool:
    """True iff one of the first 40 records identifies this session as
    running one of the configured self-agents.

    Claude Code writes two distinct marker records at the top of session
    transcripts, depending on HOW the agent was selected:

    - `type=agent-name`: written when the session is launched with the
      --agent flag (e.g. `claude --agent foo` or `claude-profiles run foo`
      which sets it). Payload: `{"agentName": "foo"}`.
    - `type=agent-setting`: written when the agent is pinned via a plugin's
      `settings.json` (`"agent": "foo"`). Payload:
      `{"agentSetting": "<plugin>:<agent>"}` — e.g. `kb-curator:kb-curator`
      when the kb-curator plugin's settings pins its own agent.

    Missing the second form is what made the curator self-curate: the
    plugin pins its agent via settings, so the curator's own transcripts
    are tagged with `agent-setting`, not `agent-name`. Matching only
    `agent-name` produced a silent miss → self-wake loop. For
    `agent-setting` we match against the trailing agent name AND the
    plugin prefix AND the full `plugin:agent` string — whichever the
    caller passed as --self-agent works.

    Raises PermissionError if the transcript cannot be opened for reading
    (the caller marks the file as permanently ignored — otherwise kb-tail
    would loop forever re-probing an unreadable file). Other OSErrors
    (e.g. FileNotFoundError from a transcript deleted mid-tick) are
    treated as transient and yield a `False` return so the next tick can
    retry.
    """
    if not self_agents:
        return False
    try:
        with path.open("r", encoding="utf-8", errors="replace") as f:
            for i, line in enumerate(f):
                if i >= 40:
                    break
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(rec, dict):
                    continue
                t = rec.get("type")
                if t == "agent-name":
                    if rec.get("agentName") in self_agents:
                        return True
                elif t == "agent-setting":
                    setting = rec.get("agentSetting")
                    if not isinstance(setting, str) or not setting:
                        continue
                    if setting in self_agents:
                        return True
                    if ":" in setting:
                        plugin, agent = setting.split(":", 1)
                        if agent in self_agents or plugin in self_agents:
                            return True
    except PermissionError:
        # Re-raised so the caller can mark the file as permanently
        # ignored. A transient `return False` would cause kb-tail to
        # re-probe (and fail) every single tick.
        raise
    except OSError:
        return False
    return False


def scan_transcript_stops(
    path: Path,
    from_offset: int,
    source_cwd: str,
) -> tuple[list[dict], int]:
    """Read path from `from_offset`; return (events, new_offset).

    Only assistant records with stop_reason=end_turn become events.
    Partial trailing lines are NOT consumed — the new offset stops at
    the last newline so the next tick re-reads from there.
    """
    events: list[dict] = []
    try:
        f = path.open("rb")
    except OSError:
        return events, from_offset
    try:
        f.seek(from_offset)
        offset = from_offset
        while True:
            line = f.readline()
            if not line:
                break
            if not line.endswith(b"\n"):
                # Partial trailing line — leave offset where it was.
                break
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                offset += len(line)
                continue
            if (
                isinstance(rec, dict)
                and rec.get("type") == "assistant"
                and isinstance(rec.get("message"), dict)
                and rec["message"].get("stop_reason") == "end_turn"
            ):
                ts = _parse_ts(rec.get("timestamp"))
                events.append(
                    {
                        "type": "stop",
                        "timestamp": ts,
                        "session_id": rec.get("sessionId") or "",
                        "uuid": rec.get("uuid") or "",
                        "transcript_path": str(path),
                        "source_cwd": source_cwd,
                    }
                )
            offset += len(line)
    finally:
        f.close()
    return events, offset


def _parse_ts(s: str | None) -> str:
    """Parse an RFC3339 timestamp from a transcript record, normalising to
    UTC ISO8601. Falls back to now() on parse failure or missing value,
    emitting a stderr warning so a debugging operator can tell synthetic
    timestamps apart from real ones (otherwise they're indistinguishable
    inside the inbox event JSON).
    """
    if s:
        try:
            t = datetime.fromisoformat(s.replace("Z", "+00:00"))
            return t.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
        except ValueError:
            print(
                f"kb-tail: unparseable ts {s!r}, using now()",
                file=sys.stderr,
            )
    else:
        print(
            "kb-tail: missing transcript timestamp, using now()",
            file=sys.stderr,
        )
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


# ----------------------------- inbox + state -------------------------------


def write_inbox_event(inbox_dir: Path, ev: dict) -> None:
    """Atomic write of an event JSON file: tmp + rename.

    Filename: <ts>-<type>-<id>.json where id is the first 8 chars of the
    session_id (stop) or first 7 chars of the sha (commit).
    """
    ts_raw = ev["timestamp"]
    ts = ts_raw.replace(":", "-").replace(".", "-")
    if ev["type"] == "stop":
        ident = (ev.get("session_id") or "")[:8] or "unknown"
    elif ev["type"] == "commit":
        ident = (ev.get("sha") or "")[:7] or "unknown"
    else:
        ident = "unknown"
    name = f"{ts}-{ev['type']}-{ident}.json"
    path = inbox_dir / name
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(ev, indent=2, ensure_ascii=False), encoding="utf-8")
    tmp.replace(path)


# State schema (sorted on write for stable diffs):
#   {
#     "offsets": {"<abs jsonl path>": <int>},
#     "last_seen_sha": "<sha>" | "",
#     "ignored_transcripts": {"<abs jsonl path>": true},
#     "source_cwds": {"<abs jsonl path>": "<cwd>"}
#   }


def load_state(path: Path) -> dict:
    if not path.exists():
        return _empty_state()
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return _empty_state()
    if not isinstance(data, dict):
        return _empty_state()
    data.setdefault("offsets", {})
    data.setdefault("last_seen_sha", "")
    data.setdefault("ignored_transcripts", {})
    data.setdefault("source_cwds", {})
    return data


def _empty_state() -> dict:
    return {
        "offsets": {},
        "last_seen_sha": "",
        "ignored_transcripts": {},
        "source_cwds": {},
    }


def save_state(path: Path, state: dict) -> None:
    """Atomic write of sorted state for stable on-disk diffing."""
    out = {
        "offsets": dict(sorted(state.get("offsets", {}).items())),
        "last_seen_sha": state.get("last_seen_sha", ""),
        "ignored_transcripts": dict(
            sorted(state.get("ignored_transcripts", {}).items())
        ),
        "source_cwds": dict(sorted(state.get("source_cwds", {}).items())),
    }
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(out, indent=2, ensure_ascii=False), encoding="utf-8")
    tmp.replace(path)


# ----------------------------- main loop -----------------------------------


def resolve_transcript_dirs(cfg: argparse.Namespace, repo_root: str | None) -> list[Path]:
    """Per-tick: list of encoded transcript dirs to scan.

    - global mode: every direct subdir of ~/.claude/projects/. Picks up
      projects added at runtime without restarting kb-tail.
    - repo mode: encoded dir for every worktree of the main repo.
    """
    if cfg.scan_all_projects:
        projects_root = Path.home() / ".claude" / "projects"
        try:
            entries = [p for p in projects_root.iterdir() if p.is_dir()]
        except OSError:
            return []
        return entries
    if repo_root is None:
        return []
    return [transcripts_dir_for(w) for w in list_worktrees(repo_root)]


def tick(
    transcript_dirs: list[Path],
    inbox_dir: Path,
    repo_root: str | None,
    scan_all_projects: bool,
    state: dict,
    first_scan: bool,
    self_agents: set[str],
) -> None:
    """One iteration: scan all transcript dirs + (repo mode only) check HEAD."""
    # 1. Transcripts. state.offsets keys are absolute paths so cross-dir
    # tracking needs no extra namespacing.
    offsets: dict = state["offsets"]
    ignored: dict = state["ignored_transcripts"]
    source_cwds: dict = state["source_cwds"]

    for tdir in transcript_dirs:
        try:
            entries = [p for p in tdir.iterdir() if p.is_file() and p.suffix == ".jsonl"]
        except OSError:
            continue
        for jsonl in entries:
            full = str(jsonl)
            if ignored.get(full):
                continue
            # Re-probe self-agent on EVERY tick (not just first sight) for
            # two reasons:
            #   1. Race: Claude Code may create the transcript file before
            #      writing the agent-name / agent-setting marker record.
            #      First-sight probing misses, the file is tracked as
            #      non-self, and the curator self-curates from then on.
            #   2. Recovery: a previously-shipped buggy version may have
            #      already tracked a curator transcript as non-self. The
            #      re-probe catches it after the next assistant turn and
            #      moves it into `ignored` without needing manual state
            #      cleanup.
            # Cost: one 40-line read per non-ignored transcript per tick.
            # Negligible compared to the per-tick git calls.
            try:
                is_self = is_self_agent_transcript(jsonl, self_agents)
            except PermissionError:
                # Unreadable forever (chmod / EACCES) — mark permanently
                # ignored so we stop re-probing every tick.
                ignored[full] = True
                offsets.pop(full, None)
                source_cwds.pop(full, None)
                print(
                    f"kb-tail: cannot read {jsonl.name} (permission denied), "
                    "marking as ignored",
                    file=sys.stderr,
                )
                continue
            if is_self:
                ignored[full] = True
                offsets.pop(full, None)
                source_cwds.pop(full, None)
                print(
                    f"kb-tail: ignoring self-agent transcript {jsonl.name}",
                    file=sys.stderr,
                )
                continue
            known = offsets.get(full)
            if first_scan or known is None:
                # New file (and not self-agent per above probe): cache
                # source cwd, record current size, never emit retroactively.
                cwd = read_transcript_cwd(jsonl)
                if cwd:
                    source_cwds[full] = cwd
                try:
                    offsets[full] = jsonl.stat().st_size
                except OSError:
                    pass
                continue
            cwd = source_cwds.get(full, "")
            if not cwd:
                # Symmetric with the self-agent re-probe above: the cwd
                # field may appear past the initial 30-line probe window
                # used at first sight. Try again each tick until we
                # find one, then cache forever.
                cwd = read_transcript_cwd(jsonl)
                if cwd:
                    source_cwds[full] = cwd
            events, new_offset = scan_transcript_stops(jsonl, known, cwd)
            offsets[full] = new_offset
            for ev in events:
                try:
                    write_inbox_event(inbox_dir, ev)
                except OSError as e:
                    print(f"kb-tail: write inbox: {e}", file=sys.stderr)
                    continue
                print(f"kb-tail: +1 stop {ev['session_id'][:8]}", flush=True)

    # 2. Git commits — repo mode only. Global mode has no single repo to
    # watch and per-repo enumeration is intentionally out of scope.
    if scan_all_projects or repo_root is None:
        return
    head = git("rev-parse", "HEAD", cwd=repo_root)
    if not head or head == state["last_seen_sha"]:
        return
    if first_scan or not state["last_seen_sha"]:
        state["last_seen_sha"] = head
        return
    shas = new_commits_since(state["last_seen_sha"], head, repo_root)
    for sha in shas:
        ev = {
            "type": "commit",
            "timestamp": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
            "sha": sha,
            "repo_root": repo_root,
            "source_cwd": repo_root,
        }
        try:
            write_inbox_event(inbox_dir, ev)
        except OSError as e:
            print(f"kb-tail: write inbox: {e}", file=sys.stderr)
            continue
        print(f"kb-tail: +1 commit {sha[:7]}", flush=True)
    state["last_seen_sha"] = head


def run(argv: list[str]) -> int:
    cfg = parse_config(argv)

    # Resolve KB dir + repo root.
    #   - When the user supplies --kb-dir / KB_TAIL_DIR, that path IS the
    #     kb dir (the directory containing decisions/, fixes/, sessions/,
    #     inbox/, .focus). No `.kb` is appended — `KB_TAIL_DIR=$HOME/.kb`
    #     means "my KB lives at $HOME/.kb", not "$HOME/.kb/.kb".
    #   - When neither flag nor env is set (repo mode default), kb_dir is
    #     `<main repo root>/.kb`. The `.kb` subdir is the project-mode
    #     convention; it does NOT apply to the user-supplied path.
    # This eliminates the silent doubled-`.kb` bug previously triggered
    # when a user set KB_TAIL_DIR=$HOME/.kb expecting it to be the kb dir.
    repo_root: str | None
    if cfg.scan_all_projects:
        kb_dir = Path(cfg.kb_dir)  # type: ignore[arg-type]
        repo_root = None
    else:
        rr = main_repo_root_from_cwd()
        if rr is None:
            print("kb-tail: not in a git repo", file=sys.stderr)
            return 1
        repo_root = rr
        kb_dir = Path(cfg.kb_dir) if cfg.kb_dir else Path(rr) / ".kb"
    inbox_dir = kb_dir / "inbox"
    processed_dir = inbox_dir / "processed"
    try:
        processed_dir.mkdir(parents=True, exist_ok=True)
    except OSError as e:
        print(f"kb-tail: mkdir {processed_dir}: {e}", file=sys.stderr)
        return 1

    state_path = inbox_dir / ".kb-tail-state.json"
    state = load_state(state_path)
    first_scan = not state["last_seen_sha"] and not state["offsets"]

    # Graceful shutdown.
    stop = {"flag": False}

    def _on_signal(_signum, _frame):
        stop["flag"] = True

    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)

    self_agents = cfg.self_agent
    exclude_msg = (
        "(no self-agent filter)"
        if not self_agents
        else "self-agents=" + ",".join(sorted(self_agents))
    )
    mode = "global" if cfg.scan_all_projects else "repo"
    print(
        f"kb-tail: mode={mode} kb={kb_dir} (firstScan={first_scan}) {exclude_msg}",
        file=sys.stderr,
    )

    interval = 2.0
    while not stop["flag"]:
        transcript_dirs = resolve_transcript_dirs(cfg, repo_root)
        try:
            tick(
                transcript_dirs,
                inbox_dir,
                repo_root,
                cfg.scan_all_projects,
                state,
                first_scan,
                self_agents,
            )
        except Exception as e:  # never crash the monitor
            print(f"kb-tail: tick error: {e}", file=sys.stderr)
        first_scan = False
        try:
            save_state(state_path, state)
        except OSError as e:
            print(f"kb-tail: save state: {e}", file=sys.stderr)
        # Interruptible sleep.
        for _ in range(int(interval * 10)):
            if stop["flag"]:
                break
            time.sleep(0.1)
    return 0


if __name__ == "__main__":
    sys.exit(run(sys.argv[1:]))
