#!/usr/bin/env bash
# Smoke test for /delegate (Agent View dispatch path, bg-only after Atto III).
#
# Drives the bg dispatcher + state watcher + parent hook against a stub
# `claude` binary, so we exercise the Go code end-to-end without burning
# subscription quota or depending on a real Claude Code daemon being up.
#
# What this validates:
#   1. _delegate-bg-dispatch builds a `claude --bg` command from a profile,
#      captures the short bg id, and writes bg-session-id.txt.
#   2. The dispatcher passes the profile's _settings (with the distill Stop
#      hook entry) via --settings <file>.
#   3. _delegate-bg-watcher polls state.json and calls `claude stop <bg-id>`
#      once the session reaches a terminal state. After Atto III the
#      watcher does NOT write any result file — the hook reads state.json
#      directly.
#   4. The UserPromptSubmit hook, given the parent session id on stdin,
#      reads each delegate's bg-session-id.txt → state.json, extracts the
#      assistant text from linkScanPath, emits it as additionalContext,
#      and writes a delivered.txt marker so subsequent prompts don't
#      re-fire the same reply.
#   5. The dispatcher applies the goal-name prefix to --name (Issue #18
#      Atto I.5 invariant).
#   6. Slash-command tasks (req.Task starting with "/") are forwarded to
#      `claude --bg` unmodified (Issue #17 v2.1.146+ regression guard).
#
# What this does NOT validate:
#   - Real Claude Code daemon behaviour (subprocess wakeups, worktree
#     creation, model dispatch, plugin loading) — covered manually
#     post-merge.
#
# Run from the repo root:
#   ./scripts/smoke-delegate-bg.sh
#
# Exit non-zero if any assertion fails.

set -euo pipefail

SMOKE=$(mktemp -d)
trap 'cd /; rm -rf "$SMOKE"' EXIT

BIN="$SMOKE/claude-profiles"
export CLAUDE_PROFILES_ROOT="$SMOKE/profroot"
export CLAUDE_CONFIG_DIR="$SMOKE/claude"
export HOME="$SMOKE/home"
mkdir -p "$HOME" "$CLAUDE_PROFILES_ROOT" "$CLAUDE_CONFIG_DIR/jobs"

# Build the binary under test.
go build -o "$BIN" ./cmd/claude-profiles/

# ---- Stub `claude` binary ---------------------------------------------------
#
# Behaviour:
#   - `claude --bg <flags> <prompt>`: dumps argv to args.log, prints
#     `backgrounded · <FAKE_BG_ID>` plus the documented helper lines, AND
#     pre-populates state.json + jsonl so the watcher can extract immediately.
#   - `claude stop <id>`: appends "stop <id>" to stops.log, prints "stopped <id>".
#   - Anything else: exit 0.
FAKE_BG_ID="abc12345"
PROJECT_DIR="$CLAUDE_CONFIG_DIR/projects/-smoke"
mkdir -p "$PROJECT_DIR"
JSONL_PATH="$PROJECT_DIR/$FAKE_BG_ID-stub.jsonl"

STUB_BIN_DIR="$SMOKE/stubbin"
mkdir -p "$STUB_BIN_DIR"
cat > "$STUB_BIN_DIR/claude" <<EOF
#!/usr/bin/env bash
LOG="$SMOKE/stub.log"
echo "claude invoked: \$*" >> "\$LOG"
case "\$1" in
  --bg)
    # Persist this specific invocation's args so the smoke test can
    # introspect them later. `claude stop` (separate invocation) must NOT
    # clobber this — that's why we don't share a single args.log.
    printf '%s\n' "\$@" > "$SMOKE/bg-args.log"
    JOB_DIR="$CLAUDE_CONFIG_DIR/jobs/$FAKE_BG_ID"
    mkdir -p "\$JOB_DIR"
    cat > "\$JOB_DIR/state.json" <<JSON
{
  "state": "blocked",
  "detail": "stub finished one turn",
  "linkScanPath": "$JSONL_PATH",
  "sessionId": "stub-session-uuid",
  "daemonShort": "$FAKE_BG_ID",
  "name": "stub session",
  "intent": "smoke test prompt"
}
JSON
    cat > "$JSONL_PATH" <<'JSONL'
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"SMOKE_REPLY_OK: the delegate completed successfully."}]}}
{"type":"system","subtype":"turn_duration","duration_ms":1234}
JSONL
    echo "backgrounded · $FAKE_BG_ID"
    echo "  claude agents             list sessions"
    echo "  claude attach $FAKE_BG_ID    open in this terminal"
    echo "  claude logs $FAKE_BG_ID      show recent output"
    echo "  claude stop $FAKE_BG_ID      stop this session"
    exit 0
    ;;
  stop)
    echo "stop \$2" >> "$SMOKE/stops.log"
    echo "stopped \$2"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
EOF
chmod +x "$STUB_BIN_DIR/claude"
export PATH="$STUB_BIN_DIR:$PATH"

# ---- Profile with distill enabled ------------------------------------------
PROFILE_DIR="$CLAUDE_PROFILES_ROOT/profiles/smoke-bg"
mkdir -p "$PROFILE_DIR"
# Two formats are supported: a single profile.json with embedded _settings,
# or split files (.mcp.json + settings.json). We use the split form so the
# dispatcher exercises the .mcp.json + profile.json branch.
cat > "$PROFILE_DIR/profile.json" <<'JSON'
{
  "_settings": {
    "hooks": {
      "Stop": [{
        "matcher": "",
        "hooks": [{
          "type": "command",
          "command": "claude-profiles _hook-stop"
        }]
      }]
    }
  },
  "_distill": "on"
}
JSON
cat > "$PROFILE_DIR/.mcp.json" <<'JSON'
{"mcpServers": {}}
JSON

# ---- Fake parent session ----------------------------------------------------
PARENT_SID="parent-smoke-sid-0001"
WRAPPER_PID=99999
mkdir -p "$CLAUDE_PROFILES_ROOT/run"
cat > "$CLAUDE_PROFILES_ROOT/run/${WRAPPER_PID}.json" <<JSON
{"pid":$WRAPPER_PID,"profile":"smoke-bg","session_id":"$PARENT_SID","started_at":0,"cwd":"$SMOKE"}
JSON
export CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID"

# ---- Hand-craft the delegate request (skip launch script for unit clarity) -
DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$DELG_ID"
mkdir -p "$DELG_DIR"
cat > "$DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"echo smoke test","parent_session":"$PARENT_SID","delegate_id":"$DELG_ID","dir":""}
JSON

FAILURES=0

assert_eq() {
  local got="$1" want="$2" what="$3"
  if [ "$got" != "$want" ]; then
    echo "FAIL: $what — got '$got', want '$want'"
    FAILURES=$((FAILURES + 1))
  else
    echo "ok: $what"
  fi
}

assert_file_contains() {
  local file="$1" needle="$2" what="$3"
  if [ ! -f "$file" ]; then
    echo "FAIL: $what — file not found: $file"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if grep -q "$needle" "$file"; then
    echo "ok: $what"
  else
    echo "FAIL: $what — '$needle' not in $file"
    FAILURES=$((FAILURES + 1))
  fi
}

# ---- Step 1: dispatch -------------------------------------------------------
echo "==> dispatching delegate $DELG_ID"
DISPATCH_OUT=$("$BIN" _delegate-bg-dispatch "$DELG_ID")
echo "$DISPATCH_OUT"

# Step 1.a: dispatcher prints DELEGATE_BG_ID with the stub's id.
got_bg_id=$(echo "$DISPATCH_OUT" | sed -n 's/^DELEGATE_BG_ID=//p')
assert_eq "$got_bg_id" "$FAKE_BG_ID" "dispatcher prints DELEGATE_BG_ID"

# Step 1.b: dispatcher writes bg-session-id.txt with the same id.
on_disk_bg_id=$(cat "$DELG_DIR/bg-session-id.txt" 2>/dev/null | tr -d '\n')
assert_eq "$on_disk_bg_id" "$FAKE_BG_ID" "bg-session-id.txt contents"

# Step 1.c: dispatcher passes --settings <path> to claude --bg, and that
# path's contents include the distill Stop hook entry.
settings_path=$(awk '/^--settings$/{getline; print; exit}' "$SMOKE/bg-args.log")
if [ -z "$settings_path" ] || [ ! -f "$settings_path" ]; then
  echo "FAIL: dispatcher did not pass --settings <existing-file> (args:)"
  cat "$SMOKE/bg-args.log" | head -20
  FAILURES=$((FAILURES + 1))
else
  echo "ok: dispatcher passes --settings $settings_path"
  assert_file_contains "$settings_path" '"Stop"' "settings file carries Stop hook"
  assert_file_contains "$settings_path" '_hook-stop' "settings file carries distill _hook-stop command"
fi

# Step 1.d: dispatcher passes --add-dir, --plugin-dir, --permission-mode, --name.
assert_file_contains "$SMOKE/bg-args.log" '^--name$' "dispatcher passes --name"
assert_file_contains "$SMOKE/bg-args.log" '^--plugin-dir$' "dispatcher passes --plugin-dir"
assert_file_contains "$SMOKE/bg-args.log" '^--permission-mode$' "dispatcher passes --permission-mode"

# ---- Step 2: wait for the watcher to call `claude stop` --------------------
#
# After Atto III the watcher's only job is freeing the Agent View slot via
# `claude stop` once the session reaches a terminal state. It writes NO
# result file — the parent hook reads state.json directly on the next
# UserPromptSubmit (Step 3).
echo "==> waiting for bg watcher to call claude stop (state.json pre-populated)"
WAITED=0
while [ ! -f "$SMOKE/stops.log" ] && [ $WAITED -lt 30 ]; do
  sleep 1
  WAITED=$((WAITED + 1))
done

if [ ! -f "$SMOKE/stops.log" ]; then
  echo "FAIL: claude stop never invoked after ${WAITED}s"
  echo "--- bg-watcher.log ---"; cat "$DELG_DIR/bg-watcher.log" 2>/dev/null || echo "(no log)"
  echo "--- state.json ---"; cat "$CLAUDE_CONFIG_DIR/jobs/$FAKE_BG_ID/state.json" 2>/dev/null
  FAILURES=$((FAILURES + 1))
else
  echo "ok: claude stop invoked after ${WAITED}s"
  assert_file_contains "$SMOKE/stops.log" "stop $FAKE_BG_ID" "watcher called claude stop on terminal state"
fi

# Step 2.b: watcher must NOT have written result.md (Atto III removed it).
if [ -f "$DELG_DIR/result.md" ]; then
  echo "FAIL: result.md was written — Atto III watcher should not write any result file"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: no result.md written (watcher is hook-driven now)"
fi

# ---- Step 3: parent UserPromptSubmit hook reads state.json directly --------
echo "==> invoking _hook-prompt-submit with the parent session id"
HOOK_OUT=$(echo "{\"session_id\":\"$PARENT_SID\"}" | "$BIN" _hook-prompt-submit)
echo "$HOOK_OUT" | head -3

# Step 3.a: hook output includes the assistant text from state.json's linkScanPath.
if echo "$HOOK_OUT" | grep -q "SMOKE_REPLY_OK"; then
  echo "ok: hook injects state.json content as additionalContext"
else
  echo "FAIL: hook output missing SMOKE_REPLY_OK"
  echo "$HOOK_OUT"
  FAILURES=$((FAILURES + 1))
fi

# Step 3.b: hook wrote delivered.txt marker.
if [ -f "$DELG_DIR/delivered.txt" ]; then
  echo "ok: hook wrote delivered.txt marker"
else
  echo "FAIL: hook did not write delivered.txt marker"
  ls -la "$DELG_DIR"
  FAILURES=$((FAILURES + 1))
fi

# Step 3.c: hook is idempotent — a second invocation must not re-inject.
HOOK_OUT2=$(echo "{\"session_id\":\"$PARENT_SID\"}" | "$BIN" _hook-prompt-submit)
if echo "$HOOK_OUT2" | grep -q "SMOKE_REPLY_OK"; then
  echo "FAIL: hook re-injected SMOKE_REPLY_OK after delivered.txt was written"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: hook is idempotent (delivered.txt blocks re-injection)"
fi

# ---- Step 4: goal-tagged dispatch writes the right --name -------------------
#
# Exercises the second leg of the bgDisplayName ↔ parseGoalFromName roundtrip:
# request.json{goal:"X"} should result in `--name "goal:X | <profile>: <task>"`
# being passed to claude --bg. Pure write-path assertion — we don't run the
# watcher for this delegate (the previous one already covered watcher → hook).
echo "==> Step 4: dispatching delegate with goal-tagged request"
GOAL_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
GOAL_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$GOAL_DELG_ID"
mkdir -p "$GOAL_DELG_DIR"
cat > "$GOAL_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"echo goal smoke","parent_session":"$PARENT_SID","delegate_id":"$GOAL_DELG_ID","dir":"","goal":"smoke-goal"}
JSON

"$BIN" _delegate-bg-dispatch "$GOAL_DELG_ID" >/dev/null

# bg-args.log is written by the stub on every `claude --bg` invocation: one
# arg per line. The value passed to --name is on the line right after the
# literal `--name` token.
name_value=$(awk '/^--name$/{getline; print; exit}' "$SMOKE/bg-args.log")
expected_prefix="goal:smoke-goal | smoke-bg: echo goal smoke"
assert_eq "$name_value" "$expected_prefix" "dispatcher applies goal prefix to --name"

# ---- Step 5: dispatch with a slash-command task (v2.1.146+ regression guard) -
#
# Before v2.1.146, `claude --bg` refused sessions whose only input was a skill
# or slash command. Our code passes req.Task as a positional arg unchanged; this
# step asserts that a task starting with "/" is forwarded to claude --bg without
# any filtering or rejection on the claude-profiles side.
echo "==> Step 5: dispatching delegate with slash-command task"
SLASH_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
SLASH_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$SLASH_DELG_ID"
mkdir -p "$SLASH_DELG_DIR"
cat > "$SLASH_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"/work-on-goal ship the feature","parent_session":"$PARENT_SID","delegate_id":"$SLASH_DELG_ID","dir":""}
JSON

"$BIN" _delegate-bg-dispatch "$SLASH_DELG_ID" >/dev/null

# The stub claude always exits 0 regardless of the task content, mirroring the
# v2.1.146+ fixed behaviour. We verify our code forwarded the task unmodified.
slash_task_in_args=$(grep -c "^/work-on-goal" "$SMOKE/bg-args.log" || true)
if [ "$slash_task_in_args" -ge 1 ]; then
  echo "ok: slash-command task forwarded to claude --bg unmodified"
else
  echo "FAIL: slash-command task was not forwarded (or was filtered)"
  cat "$SMOKE/bg-args.log"
  FAILURES=$((FAILURES + 1))
fi

# bg-session-id.txt must be written (dispatch succeeded).
slash_bg_id=$(cat "$SLASH_DELG_DIR/bg-session-id.txt" 2>/dev/null | tr -d '\n')
assert_eq "$slash_bg_id" "$FAKE_BG_ID" "slash-command dispatch writes bg-session-id.txt"

# ---- Step 6: dispatch-error.md path (Atto III hook handles dispatch failure) -
#
# When the dispatcher fails before bg-session-id.txt is written (profile
# missing, disabled, claude not on PATH, etc.) it leaves dispatch-error.md
# behind. The parent hook delivers that file's content and renames it to
# delivered-error.md so it isn't re-fired on subsequent prompts.
echo "==> Step 6: dispatch with unknown profile triggers dispatch-error.md"
ERR_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
ERR_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$ERR_DELG_ID"
mkdir -p "$ERR_DELG_DIR"
cat > "$ERR_DELG_DIR/request.json" <<JSON
{"profile":"this-profile-does-not-exist","task":"echo nope","parent_session":"$PARENT_SID","delegate_id":"$ERR_DELG_ID","dir":""}
JSON

# Expected: dispatch exits non-zero (profile resolve failure) and writes
# dispatch-error.md. Wrap with `if ! …; then` to keep `set -e` happy.
if "$BIN" _delegate-bg-dispatch "$ERR_DELG_ID" 2>/dev/null; then
  echo "FAIL: dispatch with unknown profile should exit non-zero"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: dispatch with unknown profile exited non-zero"
fi
assert_file_contains "$ERR_DELG_DIR/dispatch-error.md" "does not resolve" "dispatch-error.md written with resolve error"

ERR_HOOK_OUT=$(echo "{\"session_id\":\"$PARENT_SID\"}" | "$BIN" _hook-prompt-submit)
if echo "$ERR_HOOK_OUT" | grep -q "does not resolve"; then
  echo "ok: hook delivered dispatch-error.md content"
else
  echo "FAIL: hook did not deliver dispatch-error.md content"
  echo "$ERR_HOOK_OUT" | head -5
  FAILURES=$((FAILURES + 1))
fi
if [ -f "$ERR_DELG_DIR/delivered-error.md" ] && [ ! -f "$ERR_DELG_DIR/dispatch-error.md" ]; then
  echo "ok: hook renamed dispatch-error.md → delivered-error.md"
else
  echo "FAIL: hook did not rename dispatch-error.md → delivered-error.md"
  ls -la "$ERR_DELG_DIR"
  FAILURES=$((FAILURES + 1))
fi

# ---- Summary ---------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
  echo "PASS — all assertions green"
  exit 0
fi
echo "FAIL — $FAILURES assertion(s) failed"
exit 1
