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
    # Persist this specific invocation's args + env so the smoke test can
    # introspect them later. `claude stop` (separate invocation) must NOT
    # clobber these — that's why we don't share a single args.log / env.log.
    printf '%s\n' "\$@" > "$SMOKE/bg-args.log"
    /usr/bin/env > "$SMOKE/bg-env.log"
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

# ---- Step 7: backward-compat — delivered.md (Atto II marker) skips ---------
#
# Upgrade scenario: a delegate from a previous (Atto II) session has a
# delivered.md marker but no delivered.txt. Its bg-session-id.txt still
# points at a state.json that is still terminal (because the supervisor's
# state survives across upgrades). Without backward-compat, the new
# hook would re-inject every such delegate on the first prompt after
# upgrade. delivered.md must be treated as a skip marker.
#
# We pair the negative assertion (old delegate must NOT re-inject) with a
# positive control (fresh undelivered delegate in the same hook call MUST
# inject) so this step also catches "hook always returns empty" regressions
# — a pure negative assertion would silently pass against a broken hook.
echo "==> Step 7: delivered.md backward-compat + positive control"
BC_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
BC_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$BC_DELG_ID"
mkdir -p "$BC_DELG_DIR"
# Same FAKE_BG_ID as Steps 1/4/5, so state.json is still terminal — if the
# hook didn't honour delivered.md it would walk through to extract.
echo "$FAKE_BG_ID" > "$BC_DELG_DIR/bg-session-id.txt"
printf 'old reply from a previous session\n' > "$BC_DELG_DIR/delivered.md"
cat > "$BC_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"old","parent_session":"$PARENT_SID","delegate_id":"$BC_DELG_ID","dir":""}
JSON

# Positive control: a brand-new undelivered delegate that *should* fire.
# If the hook is broken end-to-end (returns empty for everything), this
# assertion fails and the smoke catches it instead of silently passing
# on the negative check.
POS_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
POS_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$POS_DELG_ID"
mkdir -p "$POS_DELG_DIR"
echo "$FAKE_BG_ID" > "$POS_DELG_DIR/bg-session-id.txt"
cat > "$POS_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"fresh","parent_session":"$PARENT_SID","delegate_id":"$POS_DELG_ID","dir":""}
JSON

BC_HOOK_OUT=$(echo "{\"session_id\":\"$PARENT_SID\"}" | "$BIN" _hook-prompt-submit)

# Negative: BC_DELG_ID must NOT appear (delivered.md → skip).
if echo "$BC_HOOK_OUT" | grep -F -q "$BC_DELG_ID"; then
  echo "FAIL: hook re-injected delegate with Atto II delivered.md marker"
  echo "  output: $BC_HOOK_OUT"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: delivered.md (Atto II marker) treated as skip"
fi

# Positive: POS_DELG_ID must appear (hook is processing entries).
if echo "$BC_HOOK_OUT" | grep -F -q "$POS_DELG_ID"; then
  echo "ok: positive control — fresh delegate injected (hook is alive)"
else
  echo "FAIL: positive control — hook did not inject fresh delegate"
  echo "  output: $BC_HOOK_OUT"
  FAILURES=$((FAILURES + 1))
fi

# ---- Step 8: SubagentModel injection (issue #18) ---------------------------
#
# A profile with prefs.SubagentModel set must cause cmdDelegateBgDispatch to
# inject CLAUDE_CODE_SUBAGENT_MODEL=<value> into the bg subprocess env. The
# stub `claude --bg` dumps its env to bg-env.log on every invocation, so we
# can assert it without exercising the real Claude Code daemon.
#
# Symmetric negative control: the existing smoke-bg profile (no prefs entry)
# must NOT have CLAUDE_CODE_SUBAGENT_MODEL in its delegate env — proves we're
# not just always leaking it through os.Environ().
echo "==> Step 8: SubagentModel env injection"

# Negative control first: re-dispatch a delegate against smoke-bg (no prefs
# subagent_model set) and assert CLAUDE_CODE_SUBAGENT_MODEL is absent from
# the bg env. Important: this runs BEFORE we write the prefs file, so the
# absence is unambiguous.
NEG_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
NEG_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$NEG_DELG_ID"
mkdir -p "$NEG_DELG_DIR"
cat > "$NEG_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"neg-control","parent_session":"$PARENT_SID","delegate_id":"$NEG_DELG_ID","dir":""}
JSON
"$BIN" _delegate-bg-dispatch "$NEG_DELG_ID" >/dev/null
if grep -q '^CLAUDE_CODE_SUBAGENT_MODEL=' "$SMOKE/bg-env.log"; then
  echo "FAIL: CLAUDE_CODE_SUBAGENT_MODEL leaked into delegate env without prefs"
  grep '^CLAUDE_CODE_SUBAGENT_MODEL=' "$SMOKE/bg-env.log"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: no CLAUDE_CODE_SUBAGENT_MODEL when prefs.subagent_model unset"
fi

# Positive case A: prefs._subagent_model set (TUI write-through path).
SM_PROFILE_DIR="$CLAUDE_PROFILES_ROOT/profiles/smoke-bg-haiku"
mkdir -p "$SM_PROFILE_DIR"
cat > "$SM_PROFILE_DIR/profile.json" <<'JSON'
{
  "_settings": {}
}
JSON
cat > "$SM_PROFILE_DIR/.mcp.json" <<'JSON'
{"mcpServers": {}}
JSON
cat > "$CLAUDE_PROFILES_ROOT/profile-prefs.json" <<JSON
{
  "$SM_PROFILE_DIR": {
    "_subagent_model": "claude-haiku-4-5-20251001"
  }
}
JSON

SM_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
SM_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$SM_DELG_ID"
mkdir -p "$SM_DELG_DIR"
cat > "$SM_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg-haiku","task":"x","parent_session":"$PARENT_SID","delegate_id":"$SM_DELG_ID","dir":""}
JSON
"$BIN" _delegate-bg-dispatch "$SM_DELG_ID" >/dev/null

if grep -F -q "CLAUDE_CODE_SUBAGENT_MODEL=claude-haiku-4-5-20251001" "$SMOKE/bg-env.log"; then
  echo "ok: prefs._subagent_model injected as CLAUDE_CODE_SUBAGENT_MODEL"
else
  echo "FAIL: CLAUDE_CODE_SUBAGENT_MODEL not present or wrong value in bg env"
  echo "  --- relevant env lines ---"
  grep -E '^CLAUDE_(PROFILES|CODE)_' "$SMOKE/bg-env.log" || echo "  (no CLAUDE_* in env)"
  FAILURES=$((FAILURES + 1))
fi

# Also assert CLAUDE_PROFILES_DELEGATE=1 is still in the env — the
# SubagentModel block must be additive, not replace the distill guard.
if grep -q '^CLAUDE_PROFILES_DELEGATE=1$' "$SMOKE/bg-env.log"; then
  echo "ok: CLAUDE_PROFILES_DELEGATE=1 preserved alongside SubagentModel"
else
  echo "FAIL: CLAUDE_PROFILES_DELEGATE=1 missing after SubagentModel injection"
  FAILURES=$((FAILURES + 1))
fi

# Positive case B: _subagent_model in profile.json directly (no prefs entry).
# The dispatcher reads p.SubagentModel, which loadProfileAt populates from
# whichever source has a value — profile.json wins when prefs is empty.
PJ_PROFILE_DIR="$CLAUDE_PROFILES_ROOT/profiles/smoke-bg-pjson-sub"
mkdir -p "$PJ_PROFILE_DIR"
cat > "$PJ_PROFILE_DIR/profile.json" <<'JSON'
{
  "_settings": {},
  "_subagent_model": "claude-sonnet-4-6"
}
JSON
cat > "$PJ_PROFILE_DIR/.mcp.json" <<'JSON'
{"mcpServers": {}}
JSON
# Important: do NOT add a prefs entry for this profile — we're proving that
# profile.json alone is sufficient. Reset prefs to just the prior entry.
cat > "$CLAUDE_PROFILES_ROOT/profile-prefs.json" <<JSON
{
  "$SM_PROFILE_DIR": {
    "_subagent_model": "claude-haiku-4-5-20251001"
  }
}
JSON

PJ_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
PJ_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$PJ_DELG_ID"
mkdir -p "$PJ_DELG_DIR"
cat > "$PJ_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg-pjson-sub","task":"x","parent_session":"$PARENT_SID","delegate_id":"$PJ_DELG_ID","dir":""}
JSON
"$BIN" _delegate-bg-dispatch "$PJ_DELG_ID" >/dev/null

if grep -F -q "CLAUDE_CODE_SUBAGENT_MODEL=claude-sonnet-4-6" "$SMOKE/bg-env.log"; then
  echo "ok: profile.json _subagent_model injected when prefs is empty"
else
  echo "FAIL: _subagent_model in profile.json was ignored by dispatcher"
  grep -E '^CLAUDE_(PROFILES|CODE)_' "$SMOKE/bg-env.log" || echo "  (no CLAUDE_* in env)"
  FAILURES=$((FAILURES + 1))
fi

# ---- Step 9: _delegate-jsonl helper (Atto IV — Monitor restoration) --------
#
# The helper prints state.linkScanPath so callers can compose tail -F | jq |
# Monitor pipelines without touching the supervisor's state file directly.
# Two paths to cover:
#   - Happy path: bg-session-id.txt exists, state.linkScanPath populated → the
#     helper prints the path and exits 0.
#   - Fail-fast path: dispatch-error.md exists (no bg session was ever started)
#     → the helper exits non-zero immediately, no 30s timeout burn.
echo "==> Step 9: _delegate-jsonl happy + fail-fast"

# Happy path: reuse the already-dispatched DELG_ID from Step 1. Its
# bg-session-id.txt points at $FAKE_BG_ID, whose state.json carries the
# pre-populated linkScanPath.
HELPER_OUT=$("$BIN" _delegate-jsonl "$DELG_ID" 2>/dev/null)
HELPER_RC=$?
if [ "$HELPER_RC" != "0" ]; then
  echo "FAIL: _delegate-jsonl exited rc=$HELPER_RC on happy path"
  FAILURES=$((FAILURES + 1))
else
  assert_eq "$HELPER_OUT" "$JSONL_PATH" "_delegate-jsonl prints state.linkScanPath on happy path"
fi

# Fail-fast path: dispatch-error.md exists (no bg session was ever started).
# The helper must exit non-zero quickly (< 5s; full timeout budget is 30s) —
# no point polling when the dispatcher already gave up.
EARLY_DELG_ID=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
EARLY_DELG_DIR="$CLAUDE_PROFILES_ROOT/delegates/$PARENT_SID/$EARLY_DELG_ID"
mkdir -p "$EARLY_DELG_DIR"
cat > "$EARLY_DELG_DIR/request.json" <<JSON
{"profile":"smoke-bg","task":"x","parent_session":"$PARENT_SID","delegate_id":"$EARLY_DELG_ID","dir":""}
JSON
echo "(simulated dispatch failure)" > "$EARLY_DELG_DIR/dispatch-error.md"

START_T=$(date +%s)
if "$BIN" _delegate-jsonl "$EARLY_DELG_ID" 2>/dev/null; then
  echo "FAIL: _delegate-jsonl should exit non-zero when dispatch-error.md exists"
  FAILURES=$((FAILURES + 1))
else
  ELAPSED=$(( $(date +%s) - START_T ))
  if [ "$ELAPSED" -gt 5 ]; then
    echo "FAIL: _delegate-jsonl fail-fast took ${ELAPSED}s — must short-circuit on dispatch-error.md"
    FAILURES=$((FAILURES + 1))
  else
    echo "ok: _delegate-jsonl exits non-zero on dispatch-error.md (${ELAPSED}s elapsed)"
  fi
fi

# Unknown delegate id: helper must exit non-zero (no request.json under
# delegatesDir() means we can't even find the delegate dir).
if "$BIN" _delegate-jsonl "deadbeef" 2>/dev/null; then
  echo "FAIL: _delegate-jsonl should exit non-zero on unknown delegate id"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: _delegate-jsonl exits non-zero on unknown delegate id"
fi

# Pure-read invariant: the helper must NEVER write inside the delegate dir
# (write would race with the watcher). Snapshot DELG_DIR before/after.
BEFORE=$(ls -1 "$DELG_DIR" | sort)
"$BIN" _delegate-jsonl "$DELG_ID" >/dev/null 2>&1
AFTER=$(ls -1 "$DELG_DIR" | sort)
if [ "$BEFORE" = "$AFTER" ]; then
  echo "ok: _delegate-jsonl is a pure read (no new files in delegate dir)"
else
  echo "FAIL: _delegate-jsonl wrote inside the delegate dir"
  diff <(echo "$BEFORE") <(echo "$AFTER")
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
