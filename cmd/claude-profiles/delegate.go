package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// /delegate runs an interactive claude session in a new tmux window under a
// chosen profile, and on exit injects the delegate's last assistant message
// back into the parent session via a UserPromptSubmit hook.
//
// Lifecycle:
//   1. Slash command (handoff.md sibling, delegate.md) writes a request JSON
//      at ~/.claude-profiles/delegates/<parent>/<id>/request.json and runs
//      `tmux new-window -d -n delegate-<id> "claude-profiles _delegate-runner <id>"`.
//   2. _delegate-runner reads the request, launches claude with the profile's
//      flags + the task as the first user message, waits for claude to exit,
//      parses the session's .jsonl for the last assistant text block, writes
//      result.md beside the request.
//   3. UserPromptSubmit hook (_hook-prompt-submit) in the parent session
//      scans for unread result.md files, emits their contents as additional
//      context, and renames them to delivered.md so they're not re-injected.

const delegateLaunchScript = `#!/bin/bash
# Usage: delegate-launch.sh <profile-id> [--bg] [--dir <path>] <task...>
# Sets up a /delegate request and dispatches the delegate. Two modes:
#
#   tmux mode (default): spawn the delegate in a new tmux window via
#     ` + "`claude-profiles _delegate-runner`" + `. Prints DELEGATE_ID, DELEGATE_WINDOW,
#     DELEGATE_RESULT, DELEGATE_DIR, DELEGATE_JSONL.
#
#   bg mode (--bg): dispatch via ` + "`claude --bg`" + ` and rely on Agent View's
#     supervisor. Prints DELEGATE_ID, DELEGATE_BG_ID, DELEGATE_BG_NAME,
#     DELEGATE_RESULT, DELEGATE_DIR. No DELEGATE_WINDOW / DELEGATE_JSONL.

set -e

if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/delegate only works inside claude-profiles run wrapper" >&2; exit 1; fi
if [ -z "$CLAUDE_PROFILES_WRAPPER_PID" ]; then echo "/delegate cannot find the wrapper PID. Restart your claude-profiles run wrapper to pick up the env var." >&2; exit 1; fi

PROFILE="$1"; shift

# Optional leading flags, in any order:
#   --bg            dispatch via claude --bg (Agent View), no tmux required
#   --dir <path>    resolve to absolute now, while we still have the parent's cwd
BG_MODE=""
REPO_DIR=""
while true; do
  case "${1:-}" in
    --bg)
      BG_MODE=1
      shift
      ;;
    --dir)
      if [ -z "${2:-}" ]; then echo "delegate-launch.sh: --dir requires a path argument" >&2; exit 1; fi
      REPO_DIR=$(cd "$2" && pwd) || { echo "delegate-launch.sh: --dir path does not exist: $2" >&2; exit 1; }
      shift 2
      ;;
    *)
      break
      ;;
  esac
done

# Honour the env-var form too: lets users default the whole hub to bg mode.
if [ "${CLAUDE_PROFILES_DELEGATE_BG:-}" = "1" ]; then
  BG_MODE=1
fi

# tmux mode still requires tmux. bg mode does not — that's its whole point.
if [ -z "$BG_MODE" ] && [ -z "$TMUX" ]; then
  echo "/delegate requires tmux. Run claude-profiles inside a tmux session, or pass --bg." >&2
  exit 1
fi

TASK="$*"
if [ -z "$PROFILE" ] || [ -z "$TASK" ]; then echo "usage: delegate-launch.sh <profile> [--bg] [--dir <path>] <task...>" >&2; exit 1; fi

PARENT_SID=$(jq -r '.session_id // empty' "$HOME/.claude-profiles/run/${CLAUDE_PROFILES_WRAPPER_PID}.json" 2>/dev/null)
if [ -z "$PARENT_SID" ]; then echo "/delegate cannot find the parent session id yet — wait a few seconds and retry." >&2; exit 1; fi

DELG_ID=$(uuidgen | tr A-Z a-z | cut -c1-8)
DIR="$HOME/.claude-profiles/delegates/$PARENT_SID/$DELG_ID"
mkdir -p "$DIR"
jq -nc \
  --arg profile "$PROFILE" \
  --arg task "$TASK" \
  --arg parent "$PARENT_SID" \
  --arg id "$DELG_ID" \
  --arg dir "$REPO_DIR" \
  '{profile: $profile, task: $task, parent_session: $parent, delegate_id: $id, dir: $dir}' \
  > "$DIR/request.json"

echo "DELEGATE_ID=$DELG_ID"
echo "DELEGATE_RESULT=$DIR/result.md"
echo "DELEGATE_DIR=$DIR"

if [ -n "$BG_MODE" ]; then
  # Hand off to Go: it reads request.json, builds the claude --bg
  # invocation from the profile flags, captures the short id, spawns the
  # bg state watcher, and prints DELEGATE_BG_ID / DELEGATE_BG_NAME.
  claude-profiles _delegate-bg-dispatch "$DELG_ID"
  exit 0
fi

# tmux path (default): spawn the delegate's claude in a detached window.
tmux new-window -d -n "delegate-$DELG_ID" "claude-profiles _delegate-runner $DELG_ID"
echo "DELEGATE_WINDOW=delegate-$DELG_ID"

# Wait up to 20s for the runner to discover and announce its .jsonl path.
# Claude can take 10+s to write its first session line on slow startups (MCP
# server boot, network checks, etc.) — 10s was too tight in practice.
for _ in $(seq 1 40); do
  if [ -f "$DIR/jsonl-path.txt" ]; then break; fi
  sleep 0.5
done
if [ -f "$DIR/jsonl-path.txt" ]; then
  echo "DELEGATE_JSONL=$(cat "$DIR/jsonl-path.txt")"
else
  echo "DELEGATE_JSONL="
fi
`

const delegateWatchScript = `#!/bin/bash
# Usage: delegate-watch.sh <delegate-id>
# Polls the delegate's session .jsonl every $WATCH_INTERVAL seconds (default
# 30) and emits one digest line per window summarising new events. On exit
# (the runner writes result.md), emits a final "done" line with a preview of
# the delegate's last reply. Designed to run via Bash run_in_background=true
# + Monitor; one notification per digest, not per raw event.

# Force line-buffered stdout/stderr when stdbuf is available, so each digest
# line arrives immediately at Monitor instead of being block-buffered. The
# CP_WATCH_NOBUF guard prevents the re-exec from looping on itself.
if [ -z "$CP_WATCH_NOBUF" ] && command -v stdbuf >/dev/null 2>&1; then
  export CP_WATCH_NOBUF=1
  exec stdbuf -oL -eL "$0" "$@"
fi

DELG_ID="$1"
if [ -z "$DELG_ID" ]; then echo "usage: delegate-watch.sh <delegate-id>" >&2; exit 2; fi
if [ -z "$CLAUDE_PROFILES_WRAPPER_PID" ]; then echo "delegate-watch.sh: wrapper PID env missing" >&2; exit 2; fi

PARENT_SID=$(jq -r '.session_id // empty' "$HOME/.claude-profiles/run/${CLAUDE_PROFILES_WRAPPER_PID}.json" 2>/dev/null)
if [ -z "$PARENT_SID" ]; then echo "delegate-watch.sh: parent session id missing" >&2; exit 2; fi

DIR="$HOME/.claude-profiles/delegates/$PARENT_SID/$DELG_ID"
PATH_FILE="$DIR/jsonl-path.txt"
RESULT_FILE="$DIR/result.md"

# Wait up to 15s for the runner to announce the delegate's .jsonl path.
for _ in $(seq 1 30); do
  if [ -f "$PATH_FILE" ]; then break; fi
  sleep 0.5
done
if [ ! -f "$PATH_FILE" ]; then echo "delegate-watch.sh: .jsonl path never appeared" >&2; exit 2; fi
JSONL=$(cat "$PATH_FILE")

INTERVAL=${WATCH_INTERVAL:-30}
STUCK_AFTER=${WATCH_STUCK_AFTER:-10}    # consecutive idle intervals before "⚠ stuck" (default: 5 min at 30s)
ABANDON_AFTER=${WATCH_ABANDON_AFTER:-60} # consecutive idle intervals before tmux kill-window (default: 30 min at 30s)
LAST_LINE=0
IDLE_COUNT=0
START_TS=$(date +%s)

# digest_lines builds the per-window summary from new .jsonl content. Returns
# empty string when there are no new events (caller decides whether to emit a
# heartbeat instead).
digest_lines() {
  local from=$1 to=$2
  if [ "$to" -le "$from" ]; then return; fi
  sed -n "$((from + 1)),${to}p" "$JSONL" 2>/dev/null | jq -rRs '
    (split("\n") | map(select(length > 0) | fromjson? // empty)) as $events |
    [$events[] | select(.type == "assistant")] as $ass |
    [$ass[] | .message.content // [] | .[] | select(.type == "tool_use") | .name] as $tools |
    ([$ass[] | .message.content // [] | .[] | select(.type == "text") | .text] | length) as $text_count |
    "\($tools | length) tool call(s)" +
    (if ($tools | length) > 0 then " (\($tools | unique | join(", ")))" else "" end) +
    ", \($text_count) message(s)"
  ' 2>/dev/null
}

emit_periodic() {
  local from=$1 to=$2
  local body
  body=$(digest_lines "$from" "$to")
  local elapsed=$(( $(date +%s) - START_TS ))
  if [ -n "$body" ]; then
    echo "⏳ [${elapsed}s] $body"
  else
    echo "⏳ [${elapsed}s] still working — no new events in last ${INTERVAL}s"
  fi
}

# Heartbeat every INTERVAL seconds OR until result.md shows up. Always emit
# something (a real digest when events landed, a "still working" heartbeat
# when the delegate was idle), so the parent agent always knows we're alive.
while true; do
  if [ -f "$RESULT_FILE" ]; then break; fi
  for _ in $(seq 1 "$INTERVAL"); do
    if [ -f "$RESULT_FILE" ]; then break; fi
    sleep 1
  done
  if [ -f "$RESULT_FILE" ]; then break; fi
  TOTAL_LINES=$(wc -l < "$JSONL" 2>/dev/null | tr -d ' ')
  TOTAL_LINES=${TOTAL_LINES:-0}

  if [ "$TOTAL_LINES" -gt "$LAST_LINE" ]; then
    # Real progress — reset the idle counter and emit a digest.
    IDLE_COUNT=0
    emit_periodic "$LAST_LINE" "$TOTAL_LINES"
    LAST_LINE="$TOTAL_LINES"
    continue
  fi

  # No new events this interval. Decide between a routine heartbeat, a
  # one-shot "stuck" warning, silence (between stuck and abandon), or
  # force-abandon (kill the delegate's tmux window and exit).
  IDLE_COUNT=$((IDLE_COUNT + 1))
  idle_secs=$((IDLE_COUNT * INTERVAL))
  if [ "$IDLE_COUNT" -ge "$ABANDON_AFTER" ]; then
    echo "✗ abandoning — delegate appears hung; killing tmux window (no new events for ${idle_secs}s)"
    tmux kill-window -t "delegate-$DELG_ID" 2>/dev/null
    exit 0
  elif [ "$IDLE_COUNT" -eq "$STUCK_AFTER" ]; then
    # Emit the stuck warning exactly once when we hit the threshold.
    abandon_secs=$((ABANDON_AFTER * INTERVAL))
    echo "⚠ stuck — no new events for ${idle_secs}s (delegate may be waiting on an upstream tool/server). Will force-abandon after ${abandon_secs}s of total idle."
  elif [ "$IDLE_COUNT" -lt "$STUCK_AFTER" ]; then
    # Routine heartbeat — the delegate's just thinking.
    emit_periodic "$LAST_LINE" "$TOTAL_LINES"
  fi
  # Between stuck and abandon: deliberately silent to avoid spamming.
done

# Final flush of anything new since the last digest (no heartbeat needed —
# the "✓ delegate done" line below carries the same liveness signal).
TOTAL_LINES=$(wc -l < "$JSONL" 2>/dev/null | tr -d ' ')
TOTAL_LINES=${TOTAL_LINES:-0}
if [ "$TOTAL_LINES" -gt "$LAST_LINE" ]; then
  body=$(digest_lines "$LAST_LINE" "$TOTAL_LINES")
  if [ -n "$body" ]; then echo "⏳ (final window) $body"; fi
fi

# Done: emit a preview of the delegate's final reply.
if [ -f "$RESULT_FILE" ]; then
  preview=$(head -c 200 "$RESULT_FILE" | tr '\n' ' ' | tr -s ' ')
  echo "✓ delegate done — $preview"
else
  echo "✓ delegate done"
fi
`

const delegateSlashCommand = `---
description: Delegate an interactive subtask to another profile. Default mode opens a tmux window and streams progress live; --bg mode dispatches via Claude Code Agent View (claude --bg) and the result is delivered on your next prompt via the UserPromptSubmit hook.
argument-hint: <profile-id|intent> [--bg] [--dir <path>] [task...]
allowed-tools: AskUserQuestion, Bash
---
The user typed ` + "`/delegate $ARGUMENTS`" + `.

# 1. Pick profile + task

Parse $ARGUMENTS:
- First whitespace-separated token = profile selector (free-form intent → classify against Available profiles list).
- ` + "`--bg`" + ` (anywhere among the leading flags) selects bg mode: the delegate dispatches via ` + "`claude --bg`" + ` and shows up in Agent View. No live progress; the result is delivered on the user's NEXT prompt via the UserPromptSubmit hook. Consume the token.
- ` + "`--dir <path>`" + ` (anywhere among the leading flags) is the target working directory; consume both tokens.
- Everything remaining = task body.

If profile or task is empty, ask the user with AskUserQuestion.

# 2. Launch

Call the Bash tool, synchronously. Pass ` + "`--bg`" + ` and/or ` + "`--dir <path>`" + ` through to the script in the same order they appeared:

  "${CLAUDE_PLUGIN_ROOT}/scripts/delegate-launch.sh" "<profile-id>" [--bg] [--dir "<path>"] "<task body...>"

That script does all the bookkeeping (request file, dispatch, env guards). Output depends on mode.

## tmux mode (default — no --bg flag)

  DELEGATE_ID=<id>
  DELEGATE_RESULT=<path to result.md>
  DELEGATE_DIR=<delegate dir>
  DELEGATE_WINDOW=delegate-<id>
  DELEGATE_JSONL=<path or empty>

Read them out of the script's output and remember the id and the result path. Continue with Step 3 (live watch).

## bg mode (--bg flag)

  DELEGATE_ID=<id>
  DELEGATE_RESULT=<path to result.md>
  DELEGATE_DIR=<delegate dir>
  DELEGATE_BG_ID=<8-char Agent View session id>
  DELEGATE_BG_NAME=<profile>:<truncated-task>

The delegate is already running in the background. **Skip Step 3 entirely** — there is no live watcher subprocess to monitor. Tell the user in 1-2 short lines:

  - which profile + task, and the Agent View id
  - that the result will be delivered on their next prompt (or they can run ` + "`claude attach <DELEGATE_BG_ID>`" + ` to interact directly, or open ` + "`claude agents`" + ` for the full dashboard)

Then go back to whatever the user was working on. Do NOT call delegate-watch.sh in bg mode.

# 3. Stream progress (tmux mode only — skip in bg mode; skip if DELEGATE_JSONL was empty)

Call the Bash tool a SECOND time with run_in_background=true:

  "${CLAUDE_PLUGIN_ROOT}/scripts/delegate-watch.sh" "<id>"

Capture the returned task-id (call it $WATCH_TASK). Then call the Monitor tool on $WATCH_TASK. Each notification is one pre-summarized line like:

  → user prompt
  🔧 mcp__notion__notion-search
  ✎ I found three pages tagged auth...

Echo each summary line to the user (one short message per notification, no commentary unless something interesting happened).

# 4. Stop conditions — LET THE WATCHER EXIT ON ITS OWN

The watcher emits one of four kinds of line; treat each accordingly.

  - ` + "`⏳ …`" + ` — periodic progress or heartbeat digest. The delegate is still working. Echo it to the user (one short line), then keep monitoring.
  - ` + "`⚠ stuck …`" + ` — no new events for several intervals. The delegate is probably waiting on a hung upstream (slow MCP server, network, etc.). Tell the user ONCE and ask whether to keep waiting or abort. **Do NOT keep emitting follow-up "still stuck" messages.** Stay quiet; the watcher will either resume emitting ` + "`⏳`" + ` lines (recovered) or eventually emit ` + "`✗ abandoning`" + ` on its own.
  - ` + "`✗ abandoning …`" + ` — the watcher gave up and already killed the delegate's tmux window. Tell the user the delegate was force-abandoned (cite the elapsed time). Then **clean up the watcher task** (see Step 5).
  - ` + "`✓ delegate done`" + ` — completion. Watcher exited cleanly; the runner auto-killed the delegate's tmux window once the first turn finished. Immediately:
    1. Read DELEGATE_RESULT with the Read tool — that's the delegate's full final reply.
    2. Relay its content to the user in your own next message (you can quote, summarize, or pass through; user's choice of framing).
    3. Rename result.md → delivered.md so the UserPromptSubmit hook doesn't re-inject it later:
         ` + "`mv \"$DELEGATE_DIR/result.md\" \"$DELEGATE_DIR/delivered.md\"`" + `
       (Skip this if the file is already named delivered.md — race with the hook is harmless.)
    4. **clean up the watcher task** (see Step 5).

DO NOT call TaskStop on the watcher mid-stream just because the delegate "looks done" or "has produced enough output". Only ` + "`✓ delegate done`" + ` and ` + "`✗ abandoning`" + ` are real terminal signals — at that point cleanup is not just allowed, it's required.

DO NOT read the delegate's .jsonl yourself to construct a result — read DELEGATE_RESULT (the result.md the runner wrote). Reading the .jsonl directly would force you to re-derive the last assistant message, which is brittle.

# 5. Cleanup (after ` + "`✓ done`" + `, ` + "`✗ abandoning`" + `, or user-requested abort)

Always reap the watcher when you stop monitoring it:

  - ` + "`TaskStop`" + ` $WATCH_TASK — release the background-bash task slot. Safe to call after the watcher already exited on its own; it's a no-op in that case.

The delegate's tmux window is killed automatically (by the runner on ` + "`✓`" + `, by the watcher on ` + "`✗`" + `). You only need to invoke ` + "`tmux kill-window -t delegate-<id>`" + ` yourself if the user explicitly asks to abort mid-stream — in that case run kill-window AND TaskStop.

# 6. Acknowledge

One or two short lines to the user up front: which profile, what task, the delegate id and tmux window name. If DELEGATE_JSONL was empty, mention that live progress isn't available — the final result will still arrive when the watcher reports done (you'll Read DELEGATE_RESULT at that point).

You can keep helping with the main task while the delegate runs.
`

// cmdDelegateRunner is invoked by tmux: `claude-profiles _delegate-runner <id>`.
// It launches the delegate session, blocks until claude exits, extracts the
// last assistant text from the session's .jsonl, and writes result.md.
func cmdDelegateRunner(args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: claude-profiles _delegate-runner <delegate-id>"))
	}
	delegateID := args[0]

	dir, req := findDelegateRequest(delegateID)
	if dir == "" {
		fatal(fmt.Errorf("delegate %s: no request.json found under %s", delegateID, delegatesDir()))
	}

	loc, err := resolveProfileLocation(req.Profile)
	if err != nil {
		writeDelegateResult(dir, fmt.Sprintf("(delegate %s failed: profile %q does not resolve: %v)", delegateID, req.Profile, err))
		fatal(err)
	}
	if loadProfilePrefs(filepath.Dir(loc.JSONPath)).Disabled {
		msg := fmt.Sprintf("(delegate %s failed: profile %q is disabled)", delegateID, req.Profile)
		writeDelegateResult(dir, msg)
		fatal(fmt.Errorf("profile %q is disabled", req.Profile))
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		writeDelegateResult(dir, fmt.Sprintf("(delegate %s failed to load profile: %v)", delegateID, err))
		fatal(err)
	}

	// Workspace binding: a project-local profile with _worktree:true is bound
	// to its owning repo. Reject if --dir points elsewhere — otherwise we'd
	// silently let one repo's profile mutate another's files. User-level and
	// alias/name profiles have OwnerRepo="" so they're always allowed.
	if p.Worktree && loc.OwnerRepo != "" {
		target := req.Dir
		if target == "" {
			// No --dir means the delegate would run in the orchestrator's cwd,
			// which is not the bound repo — reject.
			msg := fmt.Sprintf("(delegate refused: profile %q is bound to %s and requires --dir to point there)", req.Profile, loc.OwnerRepo)
			writeDelegateResult(dir, msg)
			info("Delegate %s rejected — bound profile invoked without --dir", delegateID)
			return
		}
		// Canonicalise target too: a worktree of the bound repo should count
		// as "under" it, so /delegate <profile> --dir <some-worktree-of-owner>
		// is accepted.
		if !isCwdUnder(mainRepoRoot(target), loc.OwnerRepo) {
			msg := fmt.Sprintf("(delegate refused: profile %q is bound to %s and cannot run on %s)", req.Profile, loc.OwnerRepo, target)
			writeDelegateResult(dir, msg)
			info("Delegate %s rejected — --dir %s is outside bound repo %s", delegateID, target, loc.OwnerRepo)
			return
		}
	}

	// We deliberately DON'T inject the SessionStart hook here: the delegate
	// shouldn't see /handoff or /delegate in its profile list (it's a leaf).
	settingsPath := ""
	if len(p.Settings) > 0 {
		// Write inline settings to a temp file so the path-style --settings
		// works the same as the wrapper's normal path.
		f, err := os.CreateTemp("", "claude-profiles-delegate-*.json")
		if err == nil {
			f.Write(p.Settings)
			f.Close()
			settingsPath = f.Name()
			defer os.Remove(settingsPath)
		}
	}

	binary, err := exec.LookPath("claude")
	if err != nil {
		writeDelegateResult(dir, fmt.Sprintf("(delegate %s failed: claude binary not on PATH)", delegateID))
		fatal(err)
	}

	// Mirror cmdRun's split-format detection: prefer .mcp.json when present,
	// fall back to profile.json (old combined format). Without this, profiles
	// that only ship .mcp.json + settings.json (no profile.json) cause claude
	// to fail at --strict-mcp-config because loc.JSONPath points at a missing
	// profile.json — the delegate would exit without writing a session and the
	// result would be "(delegate exited without a final assistant reply)".
	// Built-ins skip --mcp-config entirely so claude uses native discovery.
	claudeArgs := []string{"claude"}
	if loc.Builtin == "" {
		mcpConfigPath := filepath.Join(filepath.Dir(loc.JSONPath), ".mcp.json")
		if _, err := os.Stat(mcpConfigPath); err != nil {
			mcpConfigPath = loc.JSONPath
		}
		claudeArgs = append(claudeArgs, "--strict-mcp-config", "--mcp-config", mcpConfigPath)
	}

	// Working dir the delegate's claude process starts in. Mirrors cmd.Dir set
	// below; we resolve it now to compute the expected session directory.
	workingDir := req.Dir
	if workingDir == "" {
		if cwd, cerr := os.Getwd(); cerr == nil {
			workingDir = cwd
		}
	}

	// Suppress claudeFlags' auto `--worktree` so we can pass an explicit name
	// instead. With a deterministic worktree name we know exactly where Claude
	// will write the session .jsonl, eliminating the "scan every project dir
	// and guess" workaround that drove the old session-discovery code.
	pcopy := *p
	pcopy.Settings = nil
	pcopy.Worktree = false
	claudeArgs = append(claudeArgs, claudeFlags(&pcopy, settingsPath)...)
	worktreeName := "delegate-" + delegateID
	if p.Worktree {
		claudeArgs = append(claudeArgs, "--worktree", worktreeName)
	}
	claudeArgs = append(claudeArgs, "--plugin-dir", wrapperPluginPath())
	if pdir := pluginDirFor(*loc); pdir != "" {
		claudeArgs = append(claudeArgs, "--plugin-dir", pdir)
	}
	// Delegates ALWAYS get a non-prompting permission setup — the user isn't
	// watching the delegate's tmux pane to click "yes", so any profile-default
	// mode that asks for confirmation would stall the delegate indefinitely.
	// We deliberately override the profile's _settings.permissions.defaultMode.
	//
	// For most models: `--permission-mode auto` (auto-approves safe actions,
	// prompts only on actually-risky ones — rare in practice).
	//
	// For Haiku: `auto` isn't supported (no underlying classifier). Naively
	// using `--permission-mode bypassPermissions` would trigger a startup
	// acceptance dialog and stall the delegate. The right form is the
	// --dangerously-skip-permissions FLAG, which is the gate that ENABLES
	// bypassPermissions and starts the session without the dialog. Docs
	// (https://code.claude.com/docs/en/permission-modes) call this out:
	// "you cannot enter bypassPermissions from a session that was started
	// without one of the enabling flags; restart with one to enable it."
	if strings.Contains(strings.ToLower(getModel(parseSettings(p.Settings))), "haiku") {
		claudeArgs = append(claudeArgs, "--dangerously-skip-permissions")
	} else {
		claudeArgs = append(claudeArgs, "--permission-mode", "auto")
	}
	claudeArgs = append(claudeArgs, "--name", "delegate-"+delegateID)
	if req.Task != "" {
		claudeArgs = append(claudeArgs, "--", req.Task)
	}

	// Compute the directory under ~/.claude/projects/ where Claude will write
	// this session's .jsonl. When the profile has Worktree=true we passed a
	// deterministic --worktree name above, so the path is fully predictable:
	// it's the encoded form of <repoRoot>/.claude/worktrees/<name>. When no
	// worktree is requested, Claude writes under the encoded form of its CWD;
	// resolve that via git rev-parse so we match Claude's own git-root logic.
	expectedSessionsDir := computeExpectedSessionsDir(workingDir, p.Worktree, worktreeName)

	before := snapshotJSONLInDir(expectedSessionsDir)
	cmd := exec.Command(binary, claudeArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	// Mark the child so /handoff and /delegate inside the delegate refuse —
	// nested delegation is intentionally not supported.
	cmd.Env = append(os.Environ(), "CLAUDE_PROFILES_DELEGATE=1")

	// Poll for the delegate's session .jsonl to appear, then write its path to
	// jsonl-path.txt inside the delegate dir. The slash command in the parent
	// session reads that file and hands the path to the main agent for live
	// `tail -F` + Monitor streaming.
	go announceDelegateJSONLPath(before, dir, expectedSessionsDir)

	// As soon as the delegate finishes ITS FIRST TURN (claude emits a
	// {"type":"system","subtype":"turn_duration"} event in the .jsonl), capture
	// the last assistant text and write result.md. Don't wait for claude to
	// exit — it stays alive in the tmux pane waiting for follow-up input the
	// user almost never provides. Without this, result.md is never written and
	// the UserPromptSubmit hook has nothing to deliver back to the parent.
	go writeResultOnFirstTurnComplete(dir)

	_ = cmd.Run()

	resultPath := filepath.Join(dir, "result.md")

	// If the first-turn watcher (writeResultOnFirstTurnComplete) already wrote
	// a result, keep it. Without this, the post-run fallback can OVERWRITE a
	// good result with the error fallback when the session produced no text
	// reply yet at fallback time.
	if _, err := os.Stat(resultPath); err == nil {
		fmt.Fprintln(os.Stderr)
		info("Delegate %s done — result written to %s/result.md", delegateID, dir)
		return
	}

	// Otherwise, fall back to finding the session ourselves. Prefer the
	// absolute path from jsonl-path.txt (announce goroutine writes it when the
	// new .jsonl appears under expectedSessionsDir). If that's missing, do a
	// final diff of the expected dir against the pre-launch snapshot — same
	// principle as announce, just synchronous and one-shot.
	var sessionFile string
	if data, err := os.ReadFile(filepath.Join(dir, "jsonl-path.txt")); err == nil {
		sessionFile = strings.TrimSpace(string(data))
	}
	if sessionFile == "" {
		after := snapshotJSONLInDir(expectedSessionsDir)
		var newestMtime int64
		for path, mtime := range after {
			if _, existed := before[path]; existed {
				continue
			}
			if mtime > newestMtime {
				newestMtime = mtime
				sessionFile = path
			}
		}
	}

	var result string
	if sessionFile != "" {
		result = extractLastAssistantFromFile(sessionFile)
	}
	if result == "" {
		result = "(delegate exited without a final assistant reply)"
	}
	writeDelegateResult(dir, result)
	fmt.Fprintln(os.Stderr)
	info("Delegate %s done — result written to %s/result.md", delegateID, dir)
}

// delegateRequest is the on-disk JSON the slash command writes; _delegate-runner
// reads it back to know what profile / task / parent to use.
type delegateRequest struct {
	Profile       string `json:"profile"`
	Task          string `json:"task"`
	ParentSession string `json:"parent_session"`
	DelegateID    string `json:"delegate_id"`
	Dir           string `json:"dir,omitempty"`
}

func findDelegateRequest(delegateID string) (string, delegateRequest) {
	root := delegatesDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", delegateRequest{}
	}
	for _, parent := range entries {
		if !parent.IsDir() {
			continue
		}
		dir := filepath.Join(root, parent.Name(), delegateID)
		data, err := os.ReadFile(filepath.Join(dir, "request.json"))
		if err != nil {
			continue
		}
		var req delegateRequest
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		return dir, req
	}
	return "", delegateRequest{}
}

// resultReminder is appended after the delegate's reply so that — when the
// UserPromptSubmit hook injects result.md as additionalContext on the parent's
// next prompt — the agent is re-told to clean up. The slash command says the
// same thing but is only read at command-load time; once a delegate has been
// streaming for a while the agent has often lost track of Step 5.
const resultReminder = "\n\n---\n" +
	"_Delegate cleanup reminder:_ the delegate's tmux window has already been " +
	"closed automatically. If a watcher subprocess is still tracked on your " +
	"side (Bash `run_in_background` + Monitor), call `TaskStop` on its task " +
	"id now to release the slot. No further action on the delegate itself is " +
	"needed — this reply is final.\n"

func writeDelegateResult(dir, body string) {
	_ = os.WriteFile(filepath.Join(dir, "result.md"), []byte(body+resultReminder), 0o644)
}

// announceDelegateJSONLPath polls the expected sessions dir until a brand-new
// .jsonl appears (or 30s elapse), then writes its absolute path to
// <delegateDir>/jsonl-path.txt for the parent slash command to read.
//
// We deliberately only accept GENUINELY NEW files (paths that weren't in the
// pre-launch snapshot). The shared findNewOrUpdatedSession helper also
// returns mtime-updated files, which is wrong here: the parent's session
// .jsonl can live in the same dir and would otherwise win the race.
//
// expectedSessionsDir is computed from the delegate's working dir + worktree
// name (see computeExpectedSessionsDir). With a deterministic --worktree name
// passed to claude, this is the ONLY dir the session can land in.
func announceDelegateJSONLPath(before map[string]int64, delegateDir, expectedSessionsDir string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		now := snapshotJSONLInDir(expectedSessionsDir)
		for absPath := range now {
			if _, existed := before[absPath]; existed {
				continue
			}
			_ = os.WriteFile(filepath.Join(delegateDir, "jsonl-path.txt"), []byte(absPath), 0o644)
			return
		}
	}
}

// snapshotJSONLInDir records mtimes of every .jsonl in dir. Missing dir
// returns an empty map — the watcher will pick up files when the dir is
// created. Used by the delegate runner to track only the directory where
// Claude will write the session, instead of scanning all project dirs.
func snapshotJSONLInDir(dir string) map[string]int64 {
	out := map[string]int64{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out[filepath.Join(dir, e.Name())] = info.ModTime().UnixNano()
	}
	return out
}

// computeExpectedSessionsDir returns the absolute path of the
// ~/.claude/projects/ subdir where Claude will write the delegate's session
// .jsonl. When p.Worktree is true Claude creates the worktree under
// <repoRoot>/.claude/worktrees/<worktreeName>/, so the encoded path is fully
// predictable. Without --worktree, Claude resolves its project dir from the
// git toplevel of its cwd; if git isn't available we fall back to workingDir.
func computeExpectedSessionsDir(workingDir string, useWorktree bool, worktreeName string) string {
	repoRoot := workingDir
	if workingDir != "" {
		gitCmd := exec.Command("git", "rev-parse", "--show-toplevel")
		gitCmd.Dir = workingDir
		if out, err := gitCmd.Output(); err == nil {
			repoRoot = mainRepoRoot(strings.TrimSpace(string(out)))
		}
	}
	if useWorktree {
		return encodedSessionsDir(filepath.Join(repoRoot, ".claude", "worktrees", worktreeName))
	}
	return encodedSessionsDir(repoRoot)
}

// writeResultOnFirstTurnComplete watches the delegate's .jsonl for the first
// {"type":"system","subtype":"turn_duration"} event — claude emits one at the
// end of every turn — and writes result.md as soon as it sees one. This
// decouples result delivery from the delegate's claude process exiting,
// which usually never happens (the user doesn't /exit; the pane sits idle).
//
// Subsequent turns (if the user actually drives more conversation in the
// delegate pane) are intentionally ignored: result.md is renamed delivered.md
// by the UserPromptSubmit hook after first delivery, so the parent already
// has what it needs.
func writeResultOnFirstTurnComplete(delegateDir string) {
	deadline := time.Now().Add(30 * time.Minute)
	resultPath := filepath.Join(delegateDir, "result.md")
	jsonlPathFile := filepath.Join(delegateDir, "jsonl-path.txt")

	// First: wait for jsonl-path.txt to be announced.
	var jsonlPath string
	for time.Now().Before(deadline) {
		if _, err := os.Stat(resultPath); err == nil {
			return // already written (e.g., by the post-Run() fallback)
		}
		if data, err := os.ReadFile(jsonlPathFile); err == nil {
			jsonlPath = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if jsonlPath == "" {
		return
	}

	// Then: poll the .jsonl for a turn_duration event. As soon as one appears,
	// extract the most recent assistant text and write result.md.
	for time.Now().Before(deadline) {
		if _, err := os.Stat(resultPath); err == nil {
			return
		}
		if jsonlHasTurnDuration(jsonlPath) {
			body := extractLastAssistantFromFile(jsonlPath)
			if body == "" {
				body = "(delegate finished its turn but produced no text reply)"
			}
			writeDelegateResult(delegateDir, body)
			// Tear down the delegate's tmux window now that the reply is on
			// disk — otherwise claude keeps sitting idle in the pane forever
			// and every /delegate call leaks a window. The runner is inside
			// the pane, so kill-window also kills us; that's fine — anything
			// after this point would never run anyway because the post-Run()
			// fallback would see result.md already there and skip.
			killDelegateTmuxWindow(filepath.Base(delegateDir))
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// killDelegateTmuxWindow runs `tmux kill-window -t delegate-<id>`. Best-effort:
// silently noops if tmux is missing or the window already vanished. Safe to
// call from inside the window itself — it terminates everything in the pane
// including the caller.
func killDelegateTmuxWindow(delegateID string) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return
	}
	_ = exec.Command(tmuxBin, "kill-window", "-t", "delegate-"+delegateID).Run()
}

// jsonlHasTurnDuration scans the file for any system/turn_duration event.
// Claude writes one per turn, so its presence == "at least one turn is done".
func jsonlHasTurnDuration(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"turn_duration"`)) {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "system" && ev.Subtype == "turn_duration" {
			return true
		}
	}
	return false
}

// extractLastAssistantFromFile is the underlying scanner used by both the
// pre-exit watcher and the post-exit fallback. Walks the file backward to
// find the most recent assistant text content block, skipping tool_use,
// tool_result, and thinking blocks.
func extractLastAssistantFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type event struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}

	// Slurp the file then scan in reverse — sessions are usually small enough.
	lines := []string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var ev event
		if json.Unmarshal([]byte(lines[i]), &ev) != nil {
			continue
		}
		if ev.Message.Role != "assistant" {
			continue
		}
		var buf strings.Builder
		for _, c := range ev.Message.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				if buf.Len() > 0 {
					buf.WriteString("\n\n")
				}
				buf.WriteString(c.Text)
			}
		}
		if buf.Len() > 0 {
			return strings.TrimSpace(buf.String())
		}
	}
	return ""
}

// ── UserPromptSubmit hook: deliver pending delegate results ──────────────────

// cmdHookPromptSubmit is invoked by claude as a UserPromptSubmit hook on every
// user turn in the parent session. It scans for unread delegate results,
// emits them as additionalContext, and renames them so they're delivered once.
func cmdHookPromptSubmit() {
	// Claude pipes the event JSON on stdin — it carries the firing session's
	// own session_id, which is the authoritative answer to "which session is
	// this hook running for". Parse that instead of guessing via the running-
	// wrappers heuristic (which is wrong when the master has handoff-restarted
	// or when multiple wrappers are alive).
	sessionID := ""
	if body, err := io.ReadAll(os.Stdin); err == nil {
		var event struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(body, &event)
		sessionID = event.SessionID
	}
	if sessionID == "" {
		// Fallback: wrapper PID env var → pidfile → session_id. Useful for
		// hand-testing or if claude ever stops piping session_id.
		if pid := os.Getenv("CLAUDE_PROFILES_WRAPPER_PID"); pid != "" {
			if data, err := os.ReadFile(filepath.Join(runDirPath(), pid+".json")); err == nil {
				var w RunningWrapper
				_ = json.Unmarshal(data, &w)
				sessionID = w.SessionID
			}
		}
	}
	if sessionID == "" {
		emitHookEmpty("UserPromptSubmit")
		return
	}
	dir := filepath.Join(delegatesDir(), sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		emitHookEmpty("UserPromptSubmit")
		return
	}
	var sb strings.Builder
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		req, _ := os.ReadFile(filepath.Join(dir, e.Name(), "request.json"))
		result, err := os.ReadFile(filepath.Join(dir, e.Name(), "result.md"))
		if err != nil {
			continue // not done yet
		}
		var parsed delegateRequest
		_ = json.Unmarshal(req, &parsed)
		profile := parsed.Profile
		if profile == "" {
			profile = "?"
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "[delegate %s · profile %s · completed %s]\n%s",
			e.Name(), profile, time.Now().Format("15:04:05"), string(result))
		// Mark as delivered: rename so we don't re-inject on the next turn.
		_ = os.Rename(
			filepath.Join(dir, e.Name(), "result.md"),
			filepath.Join(dir, e.Name(), "delivered.md"))
	}
	if sb.Len() == 0 {
		emitHookEmpty("UserPromptSubmit")
		return
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": sb.String(),
		},
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

func emitHookEmpty(event string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": event,
		},
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

