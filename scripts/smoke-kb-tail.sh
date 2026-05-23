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

BIN="${BIN:-$HOME/.local/bin/claude-profiles}"
if [[ ! -x "$BIN" ]]; then
  echo "smoke-kb-tail: binary not found at $BIN — run go build first" >&2
  exit 2
fi

tmpdir=$(mktemp -d -t kbtail)
transcripts_dir=""

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
  if [[ -n "$transcripts_dir" && -d "$transcripts_dir" ]]; then
    rm -rf "$transcripts_dir"
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
"$BIN" kb-tail > "$tmpdir/.kb-tail.stdout" 2> "$tmpdir/.kb-tail.stderr" &
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

"$BIN" kb-tail > "$tmpdir/.kb-tail2.stdout" 2> "$tmpdir/.kb-tail2.stderr" &
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

"$BIN" kb-tail --self-agent kb-curator > "$tmpdir/.kb-tail3.stdout" 2> "$tmpdir/.kb-tail3.stderr" &
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

echo
echo "smoke-kb-tail: ALL PASSED"
