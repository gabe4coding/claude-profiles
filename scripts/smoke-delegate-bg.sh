#!/usr/bin/env bash
# Smoke test for /delegate --bg (Agent View dispatch path).
#
# Drives the bg dispatcher + state watcher against a stub `claude` binary,
# so we exercise the Go code end-to-end without burning subscription quota
# or depending on a real Claude Code daemon being up.
#
# What this validates:
#   1. _delegate-bg-dispatch builds a `claude --bg` command from a profile,
#      captures the short bg id, and writes bg-session-id.txt.
#   2. The dispatcher passes the profile's _settings (with the distill Stop
#      hook entry) via --settings <file> — validating the M-scope "distill
#      validation" without actually running a hook end-to-end.
#   3. _delegate-bg-watcher polls the supervisor's state.json, extracts the
#      last assistant message from linkScanPath, and writes result.md in
#      the layout the existing UserPromptSubmit hook expects.
#   4. The UserPromptSubmit hook, given the parent session id on stdin,
#      reads result.md and emits its content as additionalContext (and
#      renames result.md to delivered.md so it isn't re-injected).
#
# What this does NOT validate (out of scope for M):
#   - Real Claude Code daemon behaviour (subprocess wakeups, worktree
#     creation, model dispatch, plugin loading) — covered manually post-merge.
#   - The distill Stop hook actually firing inside a bg session — Atto II.
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

# ---- Step 2: wait for the watcher to write result.md -----------------------
echo "==> waiting for bg watcher to write result.md (state.json pre-populated)"
WAITED=0
while [ ! -f "$DELG_DIR/result.md" ] && [ $WAITED -lt 30 ]; do
  sleep 1
  WAITED=$((WAITED + 1))
done

if [ ! -f "$DELG_DIR/result.md" ]; then
  echo "FAIL: result.md never appeared after ${WAITED}s"
  echo "--- bg-watcher.log ---"; cat "$DELG_DIR/bg-watcher.log" 2>/dev/null || echo "(no log)"
  echo "--- state.json ---"; cat "$CLAUDE_CONFIG_DIR/jobs/$FAKE_BG_ID/state.json" 2>/dev/null
  FAILURES=$((FAILURES + 1))
else
  echo "ok: result.md appeared after ${WAITED}s"
  assert_file_contains "$DELG_DIR/result.md" "SMOKE_REPLY_OK" "result.md carries the assistant text from the stub JSONL"
fi

# Step 2.b: watcher called `claude stop <bg-id>` after writing result.md.
WAITED=0
while [ ! -f "$SMOKE/stops.log" ] && [ $WAITED -lt 5 ]; do
  sleep 1
  WAITED=$((WAITED + 1))
done
assert_file_contains "$SMOKE/stops.log" "stop $FAKE_BG_ID" "watcher called claude stop after result"

# ---- Step 3: parent UserPromptSubmit hook picks up result.md ---------------
echo "==> invoking _hook-prompt-submit with the parent session id"
HOOK_OUT=$(echo "{\"session_id\":\"$PARENT_SID\"}" | "$BIN" _hook-prompt-submit)
echo "$HOOK_OUT" | head -3

# Step 3.a: hook output includes the assistant text.
if echo "$HOOK_OUT" | grep -q "SMOKE_REPLY_OK"; then
  echo "ok: hook injects result.md content as additionalContext"
else
  echo "FAIL: hook output missing SMOKE_REPLY_OK"
  echo "$HOOK_OUT"
  FAILURES=$((FAILURES + 1))
fi

# Step 3.b: hook renames result.md → delivered.md.
if [ -f "$DELG_DIR/delivered.md" ] && [ ! -f "$DELG_DIR/result.md" ]; then
  echo "ok: hook renamed result.md → delivered.md"
else
  echo "FAIL: hook did not rename result.md → delivered.md"
  ls -la "$DELG_DIR"
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
