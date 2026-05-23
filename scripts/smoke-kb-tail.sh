#!/usr/bin/env bash
# Smoke test for `claude-profiles kb-tail`.
#
# What it verifies, in isolation from any real Claude Code session:
#   1. firstScan: with an existing transcript jsonl AND existing git
#      HEAD, kb-tail records state but emits NO inbox events.
#   2. detect Stop: appending an assistant/end_turn line to the jsonl
#      produces one `stop` inbox entry on the next tick.
#   3. detect commit: making a new git commit produces one `commit`
#      inbox entry on the next tick.
#   4. no duplicates: restarting kb-tail with the same on-disk state
#      does not re-emit the same events.
#
# Re-runnable from a clean checkout. Exits non-zero on regression.
# Cleans up the temp repo and the fake ~/.claude/projects/<enc>/ dir
# on exit (success or failure).

set -euo pipefail
shopt -s nullglob

# Counts *.json files at the top of a dir without tripping `set -e` when
# the dir does not exist (which is the legitimate case on first run).
count_inbox() {
  local dir="$1" pat="${2:-*.json}"
  local files=("$dir"/$pat)
  echo "${#files[@]}"
}

repo_root=$(git rev-parse --show-toplevel)
SCRIPT="${KB_TAIL_SCRIPT:-$repo_root/plugins/kb-curator/scripts/kb-tail.py}"
if [[ ! -f "$SCRIPT" ]]; then
  echo "smoke-kb-tail: script not found at $SCRIPT" >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "smoke-kb-tail: python3 not in PATH" >&2
  exit 2
fi
# All "${BIN[@]}" invocations below run the Python script.
BIN=(python3 "$SCRIPT")

tmpdir=$(mktemp -d -t kbtail)
transcripts_dir=""
wt_dir=""
wt_transcripts_dir=""
global_kb=""
global_transcripts_dir=""

cleanup() {
  set +e
  if [[ -n "${KBPID:-}" ]] && kill -0 "$KBPID" 2>/dev/null; then
    kill "$KBPID" 2>/dev/null
    wait "$KBPID" 2>/dev/null
  fi
  cd / 2>/dev/null
  if [[ -n "$tmpdir" && -d "$tmpdir" ]]; then
    rm -rf "$tmpdir"
  fi
  if [[ -n "$wt_dir" && -d "$wt_dir" ]]; then
    rm -rf "$wt_dir"
  fi
  if [[ -n "$transcripts_dir" && -d "$transcripts_dir" ]]; then
    rm -rf "$transcripts_dir"
  fi
  if [[ -n "$wt_transcripts_dir" && -d "$wt_transcripts_dir" ]]; then
    rm -rf "$wt_transcripts_dir"
  fi
  if [[ -n "$global_kb" && -d "$global_kb" ]]; then
    rm -rf "$global_kb"
  fi
  if [[ -n "$global_transcripts_dir" && -d "$global_transcripts_dir" ]]; then
    rm -rf "$global_transcripts_dir"
  fi
}
trap cleanup EXIT INT TERM

# --- 1. Set up fake repo ---------------------------------------------------
cd "$tmpdir"
git init -q
git -c user.email=t@t -c user.name=t commit --allow-empty -m initial -q

# git canonicalises path (handles /tmp → /private/tmp on macOS)
canon=$(git rev-parse --show-toplevel)
# Encode path: replace every / and . with -
enc=$(printf '%s' "$canon" | sed 's|[/.]|-|g')
transcripts_dir="$HOME/.claude/projects/$enc"
mkdir -p "$transcripts_dir"

session_id="00000000-aaaa-bbbb-cccc-111111111111"
jsonl="$transcripts_dir/$session_id.jsonl"

# Seed transcript with a single non-Stop record (a user message).
cat > "$jsonl" <<'EOF'
{"type":"user","sessionId":"00000000-aaaa-bbbb-cccc-111111111111","uuid":"u1","timestamp":"2026-05-22T10:00:00Z","message":{}}
EOF

# --- 2. firstScan: should NOT emit ---------------------------------------
"${BIN[@]}" > "$tmpdir/.kb-tail.stdout" 2> "$tmpdir/.kb-tail.stderr" &
KBPID=$!
sleep 3

count=$(count_inbox "$canon/.kb/inbox")
if [[ "$count" != "0" ]]; then
  echo "FAIL: firstScan emitted $count inbox files (expected 0)" >&2
  cat "$tmpdir/.kb-tail.stderr" >&2
  exit 1
fi
echo "OK: firstScan recorded state without emitting"

# --- 3. Add a Stop event + a commit, expect 2 inbox files ----------------
cat >> "$jsonl" <<'EOF'
{"type":"assistant","sessionId":"00000000-aaaa-bbbb-cccc-111111111111","uuid":"u2","timestamp":"2026-05-22T10:01:00Z","message":{"stop_reason":"end_turn"}}
EOF

echo "smoke content" > "$canon/file.txt"
git add file.txt
git -c user.email=t@t -c user.name=t commit -m "smoke commit" -q

# Wait long enough for at least one tick (interval is 2s).
sleep 5

count=$(count_inbox "$canon/.kb/inbox")
if [[ "$count" != "2" ]]; then
  echo "FAIL: expected 2 inbox files after events, got $count" >&2
  ls -la "$canon/.kb/inbox" >&2 || true
  cat "$tmpdir/.kb-tail.stderr" >&2
  exit 1
fi

stop_count=$(count_inbox "$canon/.kb/inbox" '*-stop-*.json')
commit_count=$(count_inbox "$canon/.kb/inbox" '*-commit-*.json')
if [[ "$stop_count" != "1" || "$commit_count" != "1" ]]; then
  echo "FAIL: expected 1 stop + 1 commit, got stop=$stop_count commit=$commit_count" >&2
  exit 1
fi
echo "OK: detected 1 stop + 1 commit"

# Stdout should have exactly 2 ping lines.
ping_lines=$(grep -c '^kb-tail: +1' "$tmpdir/.kb-tail.stdout" || true)
if [[ "$ping_lines" != "2" ]]; then
  echo "FAIL: expected 2 stdout pings, got $ping_lines" >&2
  cat "$tmpdir/.kb-tail.stdout" >&2
  exit 1
fi
echo "OK: emitted exactly 2 stdout pings"

# --- 4. Restart: state must prevent re-emission --------------------------
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

"${BIN[@]}" > "$tmpdir/.kb-tail2.stdout" 2> "$tmpdir/.kb-tail2.stderr" &
KBPID=$!
sleep 3

count=$(count_inbox "$canon/.kb/inbox")
if [[ "$count" != "2" ]]; then
  echo "FAIL: restart changed inbox count: expected 2, got $count" >&2
  exit 1
fi
ping_lines=$(grep -c '^kb-tail: +1' "$tmpdir/.kb-tail2.stdout" || true)
if [[ "$ping_lines" != "0" ]]; then
  echo "FAIL: restart re-emitted $ping_lines pings (expected 0)" >&2
  cat "$tmpdir/.kb-tail2.stdout" >&2
  exit 1
fi
echo "OK: restart did not re-emit"

# --- 5. --self-agent filter: ignore the curator's own transcript ---------
# Drop the second run; relaunch with --self-agent kb-curator and introduce
# a brand-new transcript file that LOOKS like a kb-curator session (it has
# the agent-name marker at the top). Add an end_turn to it. Expect 0 new
# inbox entries and 0 new pings.
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

curator_session="ffffffff-cccc-dddd-eeee-222222222222"
curator_jsonl="$transcripts_dir/$curator_session.jsonl"
cat > "$curator_jsonl" <<'EOF'
{"type":"agent-name","agentName":"kb-curator","sessionId":"ffffffff-cccc-dddd-eeee-222222222222"}
{"type":"agent-setting","agentSetting":"kb-curator:kb-curator","sessionId":"ffffffff-cccc-dddd-eeee-222222222222"}
EOF

"${BIN[@]}" --self-agent kb-curator > "$tmpdir/.kb-tail3.stdout" 2> "$tmpdir/.kb-tail3.stderr" &
KBPID=$!
sleep 3

# Now append an end_turn that the filter must swallow.
cat >> "$curator_jsonl" <<'EOF'
{"type":"assistant","sessionId":"ffffffff-cccc-dddd-eeee-222222222222","uuid":"x1","timestamp":"2026-05-22T11:00:00Z","message":{"stop_reason":"end_turn"}}
EOF
sleep 5

new_count=$(count_inbox "$canon/.kb/inbox")
if [[ "$new_count" != "2" ]]; then
  echo "FAIL: self-agent filter let through $((new_count - 2)) extra events" >&2
  cat "$tmpdir/.kb-tail3.stderr" >&2
  exit 1
fi
new_pings=$(grep -c '^kb-tail: +1' "$tmpdir/.kb-tail3.stdout" || true)
if [[ "$new_pings" != "0" ]]; then
  echo "FAIL: self-agent run emitted $new_pings pings (expected 0)" >&2
  exit 1
fi
if ! grep -q "ignoring self-agent transcript" "$tmpdir/.kb-tail3.stderr"; then
  echo "FAIL: expected 'ignoring self-agent transcript' on stderr" >&2
  cat "$tmpdir/.kb-tail3.stderr" >&2
  exit 1
fi
echo "OK: --self-agent kb-curator filtered the curator's own transcript"

# --- 5b. agent-setting form: pin via plugin settings.json ----------------
# Real curator transcripts launched via a plugin's settings.json
# (`"agent": "kb-curator"`) carry `type=agent-setting` records, NOT
# `agent-name`. Filter must match both forms. Reproduce the regression:
# fake transcript with ONLY agent-setting + end_turn → expect 0 emits.
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

setting_session="11112222-3333-4444-5555-666666666666"
setting_jsonl="$transcripts_dir/$setting_session.jsonl"
cat > "$setting_jsonl" <<'EOF'
{"type":"custom-title","customTitle":"kb-curator","sessionId":"11112222-3333-4444-5555-666666666666"}
{"type":"agent-setting","agentSetting":"kb-curator:kb-curator","sessionId":"11112222-3333-4444-5555-666666666666"}
{"type":"permission-mode","permissionMode":"auto","sessionId":"11112222-3333-4444-5555-666666666666"}
EOF

"${BIN[@]}" --self-agent kb-curator > "$tmpdir/.kb-tail3b.stdout" 2> "$tmpdir/.kb-tail3b.stderr" &
KBPID=$!
sleep 3

cat >> "$setting_jsonl" <<'EOF'
{"type":"assistant","sessionId":"11112222-3333-4444-5555-666666666666","uuid":"s1","timestamp":"2026-05-22T11:30:00Z","message":{"stop_reason":"end_turn"}}
EOF
sleep 5

setting_count=$(count_inbox "$canon/.kb/inbox")
if [[ "$setting_count" != "2" ]]; then
  echo "FAIL: agent-setting form not recognized — $((setting_count - 2)) extra events leaked through" >&2
  cat "$tmpdir/.kb-tail3b.stderr" >&2
  exit 1
fi
if ! grep -q "ignoring self-agent transcript" "$tmpdir/.kb-tail3b.stderr"; then
  echo "FAIL: agent-setting form did not trigger 'ignoring self-agent transcript'" >&2
  cat "$tmpdir/.kb-tail3b.stderr" >&2
  exit 1
fi
echo "OK: agent-setting form filtered (the curator's real launch path)"

# --- 5c. Race recovery: marker appears AFTER kb-tail has tracked the ----
# file. Claude Code may create the transcript before writing the
# agent-setting record; first-sight probing alone would miss this and
# the file would emit forever. Re-probing each tick must catch the
# marker once it lands and move the transcript into `ignored`.
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

race_session="cccccccc-1111-2222-3333-444444444444"
race_jsonl="$transcripts_dir/$race_session.jsonl"
# Step 1: file exists but the marker isn't written yet.
cat > "$race_jsonl" <<'EOF'
{"type":"custom-title","customTitle":"some-title-no-marker","sessionId":"cccccccc-1111-2222-3333-444444444444"}
EOF

"${BIN[@]}" --self-agent kb-curator > "$tmpdir/.kb-tail3c.stdout" 2> "$tmpdir/.kb-tail3c.stderr" &
KBPID=$!
sleep 3  # kb-tail tracks the file as non-self (no marker yet)

# Step 2: NOW the marker lands.
cat >> "$race_jsonl" <<'EOF'
{"type":"agent-setting","agentSetting":"kb-curator:kb-curator","sessionId":"cccccccc-1111-2222-3333-444444444444"}
EOF
sleep 3  # next tick must re-probe and catch it

# Step 3: append end_turn. If race recovery worked the file is ignored.
cat >> "$race_jsonl" <<'EOF'
{"type":"assistant","sessionId":"cccccccc-1111-2222-3333-444444444444","uuid":"r1","timestamp":"2026-05-22T12:00:00Z","message":{"stop_reason":"end_turn"}}
EOF
sleep 5

race_count=$(count_inbox "$canon/.kb/inbox")
if [[ "$race_count" != "2" ]]; then
  echo "FAIL: race recovery missed — $((race_count - 2)) extra events leaked through" >&2
  cat "$tmpdir/.kb-tail3c.stderr" >&2
  exit 1
fi
if ! grep -q "ignoring self-agent transcript .*$race_session" "$tmpdir/.kb-tail3c.stderr"; then
  echo "FAIL: race recovery did not log 'ignoring self-agent transcript' for late-arrived marker" >&2
  cat "$tmpdir/.kb-tail3c.stderr" >&2
  exit 1
fi
echo "OK: race recovery — re-probe caught marker appended after first sight"

# --- 6. Worktree unification: .kb/ lives at MAIN root, events from a -----
#         linked worktree's encoded transcript dir feed the main inbox.
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

wt_dir="$tmpdir.wt"
git -C "$canon" worktree add -q -b kbtail-wt "$wt_dir"
wt_canon=$(git -C "$wt_dir" rev-parse --show-toplevel)
wt_enc=$(printf '%s' "$wt_canon" | sed 's|[/.]|-|g')
wt_transcripts_dir="$HOME/.claude/projects/$wt_enc"
mkdir -p "$wt_transcripts_dir"

wt_session_id="cafebabe-1111-2222-3333-444444444444"
wt_jsonl="$wt_transcripts_dir/$wt_session_id.jsonl"
cat > "$wt_jsonl" <<'EOF'
{"type":"user","sessionId":"cafebabe-1111-2222-3333-444444444444","uuid":"wu1","timestamp":"2026-05-22T12:00:00Z","message":{}}
EOF

# Launch kb-tail with cwd inside the worktree. State should resolve back
# to <main>/.kb/inbox/.kb-tail-state.json, and the worktree transcript
# should be picked up by listWorktrees() each tick.
( cd "$wt_dir" && "${BIN[@]}" > "$tmpdir/.kb-tail4.stdout" 2> "$tmpdir/.kb-tail4.stderr" ) &
KBPID=$!
sleep 3

# Verify .kb/ was NOT created under the worktree (would mean main-root
# resolution failed and the curator wrote to a gitignored throwaway dir).
if [[ -d "$wt_dir/.kb" ]]; then
  echo "FAIL: worktree spawn created $wt_dir/.kb (should be at main root)" >&2
  exit 1
fi
echo "OK: worktree spawn did not create .kb/ under the worktree"

# Verify startup log shows MAIN repo root (not worktree path).
if ! grep -qE "kb-tail: mode=repo kb=$canon " "$tmpdir/.kb-tail4.stderr"; then
  echo "FAIL: worktree spawn did not resolve to main repo root" >&2
  echo "      expected 'mode=repo kb=$canon ' in stderr; got:" >&2
  cat "$tmpdir/.kb-tail4.stderr" >&2
  exit 1
fi
echo "OK: worktree spawn resolved kb=$canon (main root)"

# Append end_turn to the worktree's transcript: event must land in the
# MAIN inbox (count goes 2 → 3).
cat >> "$wt_jsonl" <<'EOF'
{"type":"assistant","sessionId":"cafebabe-1111-2222-3333-444444444444","uuid":"wu2","timestamp":"2026-05-22T12:01:00Z","message":{"stop_reason":"end_turn"}}
EOF
sleep 5

count=$(count_inbox "$canon/.kb/inbox")
if [[ "$count" != "3" ]]; then
  echo "FAIL: worktree end_turn did not produce 1 new inbox event (expected 3, got $count)" >&2
  ls -la "$canon/.kb/inbox" >&2 || true
  cat "$tmpdir/.kb-tail4.stderr" >&2
  exit 1
fi
echo "OK: worktree transcript event landed in main .kb/inbox"

# --- 7. Global mode: --scan-all-projects + --kb-dir captures any -----------
#         project's transcript into one personal KB, skips git ops, and
#         stamps source_cwd on every event.
kill "$KBPID" 2>/dev/null || true
wait "$KBPID" 2>/dev/null || true
KBPID=""

global_kb=$(mktemp -d -t kbtail-global)
# A brand-new fake project, encoded path under ~/.claude/projects.
fake_project="/tmp/kbtail-fake-project-$$"
fake_enc=$(printf '%s' "$fake_project" | sed 's|[/.]|-|g')
global_transcripts_dir="$HOME/.claude/projects/$fake_enc"
mkdir -p "$global_transcripts_dir"

gl_session_id="deadbeef-5555-6666-7777-888888888888"
gl_jsonl="$global_transcripts_dir/$gl_session_id.jsonl"
# Seed with a record carrying cwd (Claude Code writes cwd into nearly every
# record). kb-tail's readTranscriptCwd should pick this up on first sight.
cat > "$gl_jsonl" <<EOF
{"type":"attachment","cwd":"$fake_project","sessionId":"deadbeef-5555-6666-7777-888888888888","uuid":"a1","timestamp":"2026-05-22T13:00:00Z"}
EOF

# Launch in global mode. cwd can be anything (e.g. canon main repo); the
# KB destination is --kb-dir, not derived from git.
( cd "$canon" && "${BIN[@]}" --kb-dir "$global_kb" --scan-all-projects --self-agent kb-curator > "$tmpdir/.kb-tail5.stdout" 2> "$tmpdir/.kb-tail5.stderr" ) &
KBPID=$!
sleep 3

# Verify startup announced global mode + correct kb path.
if ! grep -qE "kb-tail: mode=global kb=$global_kb " "$tmpdir/.kb-tail5.stderr"; then
  echo "FAIL: global mode did not announce kb=$global_kb" >&2
  cat "$tmpdir/.kb-tail5.stderr" >&2
  exit 1
fi
echo "OK: global mode startup announced kb=$global_kb"

# Append an end_turn — should produce one stop event in the global inbox.
cat >> "$gl_jsonl" <<EOF
{"type":"assistant","sessionId":"deadbeef-5555-6666-7777-888888888888","uuid":"a2","timestamp":"2026-05-22T13:01:00Z","cwd":"$fake_project","message":{"stop_reason":"end_turn"}}
EOF
sleep 5

gl_count=$(count_inbox "$global_kb/.kb/inbox")
if [[ "$gl_count" != "1" ]]; then
  echo "FAIL: global mode expected 1 inbox file, got $gl_count" >&2
  ls -la "$global_kb/.kb/inbox" >&2 || true
  cat "$tmpdir/.kb-tail5.stderr" >&2
  exit 1
fi
echo "OK: global mode wrote 1 stop event into $global_kb/.kb/inbox"

# Verify source_cwd is populated with the real cwd (not the encoded form).
gl_event=$(ls "$global_kb/.kb/inbox"/*.json | head -1)
if ! grep -q "\"source_cwd\": \"$fake_project\"" "$gl_event"; then
  echo "FAIL: source_cwd missing or wrong in $gl_event" >&2
  cat "$gl_event" >&2
  exit 1
fi
echo "OK: source_cwd=$fake_project recorded on event"

# Make a commit on the main repo: in global mode kb-tail must skip git
# ops entirely, so no commit event should land in the global inbox.
echo "global mode commit-skip" > "$canon/skip.txt"
git -C "$canon" add skip.txt
git -C "$canon" -c user.email=t@t -c user.name=t commit -m "skip-in-global" -q
sleep 5

gl_count_after=$(count_inbox "$global_kb/.kb/inbox")
if [[ "$gl_count_after" != "1" ]]; then
  echo "FAIL: global mode emitted commit events ($((gl_count_after - 1)) extra) — should skip git" >&2
  ls -la "$global_kb/.kb/inbox" >&2
  exit 1
fi
echo "OK: global mode skipped git commit watching"

echo
echo "smoke-kb-tail: ALL PASSED"
