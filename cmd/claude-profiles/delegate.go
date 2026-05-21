package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// /delegate runs a claude session in the background under a chosen profile
// (Claude Code Agent View / `claude --bg`) and delivers its reply to the
// parent session on the next prompt via a UserPromptSubmit hook.
//
// Lifecycle:
//   1. Slash command (delegate.md) writes request.json at
//      ~/.claude-profiles/delegates/<parent>/<id>/ and invokes
//      `claude-profiles _delegate-bg-dispatch <id>`.
//   2. _delegate-bg-dispatch builds the `claude --bg` command from the
//      profile flags, captures the bg session id, writes bg-session-id.txt,
//      and spawns _delegate-bg-watcher detached. On any dispatch-time
//      failure (profile missing, disabled, claude not on PATH, etc.) it
//      writes dispatch-error.md and exits non-zero.
//   3. _delegate-bg-watcher polls ~/.claude/jobs/<bg-id>/state.json. When
//      the session reaches a terminal state, it calls `claude stop <bg-id>`
//      to free the Agent View slot — that's its only job. On timeout it
//      writes dispatch-error.md.
//   4. UserPromptSubmit hook (_hook-prompt-submit) in the parent walks the
//      delegates dir on the next prompt, reads state.json directly via
//      bg-session-id.txt, extracts the assistant text from linkScanPath,
//      injects it as additionalContext, and writes a delivered.txt marker
//      so subsequent prompts don't re-fire. dispatch-error.md is handled
//      the same way, renamed to delivered-error.md after injection.

const delegateLaunchScript = `#!/bin/bash
# Usage: delegate-launch.sh <profile-id> [--dir <path>] [--goal <name>] <task...>
# Atto III: bg-only dispatch. Writes request.json and hands off to the Go
# bg dispatcher. No tmux, no live progress, no result.md round-trip — the
# parent's UserPromptSubmit hook delivers the delegate's reply by reading
# ~/.claude/jobs/<bg-id>/state.json directly on the next prompt.
# Prints DELEGATE_ID, DELEGATE_DIR, DELEGATE_BG_ID, DELEGATE_BG_NAME.

set -e

if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/delegate only works inside claude-profiles run wrapper" >&2; exit 1; fi
if [ -z "$CLAUDE_PROFILES_WRAPPER_PID" ]; then echo "/delegate cannot find the wrapper PID. Restart your claude-profiles run wrapper to pick up the env var." >&2; exit 1; fi

PROFILE="$1"; shift

# Optional leading flags, in any order:
#   --dir <path>    target working directory (resolved to absolute now)
#   --goal <name>   tag the bg session for grouping via ` + "`claude-profiles goal`" + `
#                   (name cannot contain |, :, or whitespace)
REPO_DIR=""
GOAL_NAME=""
while true; do
  case "${1:-}" in
    --bg)
      echo "/delegate: --bg is no longer recognised; bg is the only mode after Atto III. Drop the flag." >&2
      exit 2
      ;;
    --legacy-tmux)
      echo "/delegate: --legacy-tmux is no longer recognised — the tmux runner was demolished in Atto III." >&2
      exit 2
      ;;
    --dir)
      if [ -z "${2:-}" ]; then echo "delegate-launch.sh: --dir requires a path argument" >&2; exit 1; fi
      REPO_DIR=$(cd "$2" && pwd) || { echo "delegate-launch.sh: --dir path does not exist: $2" >&2; exit 1; }
      shift 2
      ;;
    --goal)
      if [ -z "${2:-}" ]; then echo "delegate-launch.sh: --goal requires a name argument" >&2; exit 1; fi
      GOAL_NAME="$2"
      shift 2
      ;;
    *)
      break
      ;;
  esac
done

# Reject env-var forms of the now-removed flags loudly so users with these
# in their shell rc learn about the demolition instead of seeing nothing
# change.
if [ -n "${CLAUDE_PROFILES_DELEGATE_BG:-}" ]; then
  echo "/delegate: CLAUDE_PROFILES_DELEGATE_BG is no longer honoured; bg is the only mode after Atto III." >&2
  exit 2
fi
if [ -n "${CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX:-}" ]; then
  echo "/delegate: CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX is no longer honoured — the tmux runner was demolished in Atto III." >&2
  exit 2
fi

TASK="$*"
if [ -z "$PROFILE" ] || [ -z "$TASK" ]; then echo "usage: delegate-launch.sh <profile> [--dir <path>] [--goal <name>] <task...>" >&2; exit 1; fi

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
  --arg goal "$GOAL_NAME" \
  '{profile: $profile, task: $task, parent_session: $parent, delegate_id: $id, dir: $dir, goal: $goal}' \
  > "$DIR/request.json"

echo "DELEGATE_ID=$DELG_ID"
echo "DELEGATE_DIR=$DIR"

# Hand off to Go's bg dispatcher: builds the claude --bg invocation,
# captures the short id, spawns the bg state watcher, and prints
# DELEGATE_BG_ID / DELEGATE_BG_NAME.
claude-profiles _delegate-bg-dispatch "$DELG_ID"
`

const delegateSlashCommand = `---
description: Delegate a subtask to another profile via Claude Code Agent View (claude --bg). The result is delivered on the user's next prompt via the UserPromptSubmit hook — no live progress streaming. Skill is also good when you want a deliberate "send-this-and-walk-away" pattern: the orchestrator keeps working while the delegate runs.
argument-hint: <profile-id|intent> [--dir <path>] [--goal <name>] [task...]
allowed-tools: AskUserQuestion, Bash
---
The user typed ` + "`/delegate $ARGUMENTS`" + `.

# 1. Pick profile + task

Parse $ARGUMENTS:
- First whitespace-separated token = profile selector (free-form intent → classify against Available profiles list).
- ` + "`--dir <path>`" + ` (anywhere among the leading flags) is the target working directory; consume both tokens.
- ` + "`--goal <name>`" + ` (anywhere among the leading flags) tags the bg session with a goal label so ` + "`claude-profiles goal list`" + ` can group it later. The name cannot contain ` + "`|`" + `, ` + "`:`" + `, or whitespace; pick a short kebab-case label (e.g. ` + "`refactor-auth`" + `). Consume both tokens.
- ` + "`--bg`" + ` and ` + "`--legacy-tmux`" + ` are no longer accepted (Atto III removed both — bg is the only mode now). The launch script rejects them loudly.
- Everything remaining = task body.

If profile or task is empty, ask the user with AskUserQuestion.

# 2. Launch

Call the Bash tool, synchronously. Pass ` + "`--dir <path>`" + ` and/or ` + "`--goal <name>`" + ` through to the script in the same order they appeared:

  "${CLAUDE_PLUGIN_ROOT}/scripts/delegate-launch.sh" "<profile-id>" [--dir "<path>"] [--goal "<name>"] "<task body...>"

That script writes request.json and hands off to the Go bg dispatcher. Output:

  DELEGATE_ID=<id>
  DELEGATE_DIR=<delegate dir>
  DELEGATE_BG_ID=<8-char Agent View session id>
  DELEGATE_BG_NAME=[goal:<name> | ]<profile>: <truncated-task>

The delegate is already running in the background. **You do NOT monitor it** — there is no live watcher, no tmux window, no Monitor task to track. The parent's UserPromptSubmit hook delivers the delegate's reply on the user's next prompt by reading ` + "`~/.claude/jobs/<DELEGATE_BG_ID>/state.json`" + ` directly. After delivery the hook writes a ` + "`delivered.txt`" + ` marker so the same reply isn't re-injected later.

# 3. Acknowledge

Tell the user in 1-2 short lines:

  - which profile + task, and the Agent View id (DELEGATE_BG_ID)
  - that the result will arrive on their next prompt — they can also run ` + "`claude attach <DELEGATE_BG_ID>`" + ` to interact directly, or open ` + "`claude agents`" + ` for the full dashboard

Then go back to whatever the user was working on. Do NOT read the delegate's state.json or jsonl yourself: let the hook handle it on the next prompt.
`

// delegateRequest is the on-disk JSON the slash command writes;
// _delegate-bg-dispatch reads it back to know what profile / task / parent
// to use.
type delegateRequest struct {
	Profile       string `json:"profile"`
	Task          string `json:"task"`
	ParentSession string `json:"parent_session"`
	DelegateID    string `json:"delegate_id"`
	Dir           string `json:"dir,omitempty"`
	Goal          string `json:"goal,omitempty"`
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

// extractLastAssistantFromFile scans the bg session's .jsonl for the most
// recent assistant text content block, skipping tool_use, tool_result, and
// thinking blocks. Used by both the bg watcher (when emitting result hints
// on edge cases) and the parent UserPromptSubmit hook (the primary reader,
// via state.json's linkScanPath).
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
// cmdHookPromptSubmit is the parent's UserPromptSubmit hook. After Atto III,
// it reads the delegate's outcome directly from the bg supervisor's
// ~/.claude/jobs/<bg-id>/state.json (no more result.md round-trip) and
// records a delivered.txt marker so subsequent prompts don't re-inject the
// same reply.
//
// Two delivery shapes:
//
//   - successful bg turn: bg-session-id.txt → state.json (terminal) → extract
//     last assistant text from linkScanPath → inject → write delivered.txt.
//   - failure-before-session-start (profile missing, disabled, dispatch
//     failed): the dispatcher leaves dispatch-error.md next to request.json
//     → inject its contents → rename to delivered-error.md.
//
// Per-delegate marker files mean the hook is purely state-derived: no
// mutation of session state, no race with the watcher.
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
		body, profile := collectDelegateForInjection(filepath.Join(dir, e.Name()), e.Name())
		if body == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "[delegate %s · profile %s · completed %s]\n%s",
			e.Name(), profile, time.Now().Format("15:04:05"), body)
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

// collectDelegateForInjection inspects one delegate dir and returns
// (body, profile) to inject — or ("", "") if nothing should be injected
// (already delivered, dispatch still pending, or bg session not yet
// terminal). Has the side effect of writing the appropriate delivered
// marker (delivered.txt for success, delivered-error.md for dispatch
// failure) before returning a non-empty body.
func collectDelegateForInjection(delDir, delID string) (body, profile string) {
	// Already delivered? Either the success marker or the error marker
	// being present means we've already injected for this delegate.
	if _, err := os.Stat(filepath.Join(delDir, "delivered.txt")); err == nil {
		return "", ""
	}
	if _, err := os.Stat(filepath.Join(delDir, "delivered-error.md")); err == nil {
		return "", ""
	}

	profile = "?"
	if reqBytes, err := os.ReadFile(filepath.Join(delDir, "request.json")); err == nil {
		var parsed delegateRequest
		if json.Unmarshal(reqBytes, &parsed) == nil && parsed.Profile != "" {
			profile = parsed.Profile
		}
	}

	// Dispatch-error.md takes priority: it means the dispatcher gave up
	// before bg-session-id.txt was even written (profile missing, disabled,
	// claude binary not on PATH, etc.). Rename to delivered-error.md so we
	// don't re-fire on the next prompt.
	errPath := filepath.Join(delDir, "dispatch-error.md")
	if data, err := os.ReadFile(errPath); err == nil {
		_ = os.Rename(errPath, filepath.Join(delDir, "delivered-error.md"))
		return strings.TrimRight(string(data), "\n"), profile
	}

	// Successful path: bg-session-id.txt → state.json terminal → extract.
	idBytes, err := os.ReadFile(filepath.Join(delDir, "bg-session-id.txt"))
	if err != nil {
		return "", "" // dispatcher hasn't gotten there yet
	}
	bgID := strings.TrimSpace(string(idBytes))
	if bgID == "" {
		return "", ""
	}
	state, err := readBgJobState(filepath.Join(claudeJobsDir(), bgID, "state.json"))
	if err != nil || !isBgFirstTurnDone(state) {
		return "", ""
	}
	if state.LinkScanPath != "" {
		body = extractLastAssistantFromFile(state.LinkScanPath)
	}
	if body == "" {
		// Supervisor's short status as a last resort — better than an empty
		// injection in the rare case where the JSONL hasn't been flushed.
		body = strings.TrimSpace(state.Detail)
	}
	if body == "" {
		body = fmt.Sprintf("(delegate %s finished but produced no assistant text; state=%s)", delID, state.State)
	}
	// Marker file: mtime + ISO timestamp inside, for debugging. The hook
	// only checks for the file's existence on subsequent invocations.
	_ = os.WriteFile(filepath.Join(delDir, "delivered.txt"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	return body, profile
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

