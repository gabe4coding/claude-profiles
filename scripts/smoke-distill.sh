#!/usr/bin/env bash
# Smoke test for the Stop-hook distill trigger filters.
#
# Spins up an isolated CLAUDE_PROFILES_ROOT and a throwaway git repo,
# then drives `claude-profiles _hook-stop` through the seven scenarios
# the trigger logic must distinguish:
#
#   A — no commits since wrapper start              → empty  (filter #1)
#   B — first non-claude commit                     → block  + bookmark written
#   C — re-fire on same HEAD                        → empty  (filter #4 dedup)
#   D — new non-claude commit                       → block  + bookmark advances
#   E — .claude/-only commit                        → empty  (filter #1)
#   F — uncommitted-only non-claude change          → empty  (only-commit policy)
#   G — orphaned bookmark (rebase/squash sim)       → block  + bookmark self-heals
#   H — background_tasks non-empty                  → empty  (filter: bg in flight)
#   I — session_crons non-empty                     → empty  (filter: cron turn)
#
# Run from the repo root:
#   ./scripts/smoke-distill.sh
#
# Exit code is 0 if every scenario matches its expected outcome, 1 otherwise.
# `set -euo pipefail` is on so infrastructure failures (go build, git
# commits, hook crashes) surface as a non-zero exit rather than silently
# turning into "empty output" that the empty-expectation assertions
# would otherwise accept.

set -euo pipefail

SMOKE=$(mktemp -d)
trap 'cd /; rm -rf "$SMOKE"' EXIT

export CLAUDE_PROFILES_ROOT=$SMOKE/profroot
REPO=$SMOKE/repo
BIN=$SMOKE/claude-profiles
FAILURES=0

go build -o "$BIN" ./cmd/claude-profiles/

mkdir -p "$CLAUDE_PROFILES_ROOT/profiles/smoke"
cat > "$CLAUDE_PROFILES_ROOT/profiles/smoke/profile.json" <<'JSON'
{"mcpServers":{},"_distill":"on"}
JSON

mkdir -p "$REPO"
cd "$REPO"
git init -q
git config user.email smoke@test
git config user.name smoke
# Some sandboxes force commit signing via a wrapper that only works on the
# main repo. Disable it locally so the smoke repo can commit.
git config commit.gpgsign false
git config tag.gpgSign false
echo init > README.md
git add README.md
git commit -q -m "init"

sleep 2
STARTED=$(date +%s)

mkdir -p "$CLAUDE_PROFILES_ROOT/run"
cat > "$CLAUDE_PROFILES_ROOT/run/12345.json" <<JSON
{"pid":12345,"profile":"smoke","cwd":"$REPO","started_at":$STARTED,"updated_at":$STARTED}
JSON
export CLAUDE_PROFILES_WRAPPER_PID=12345

# invoke_hook runs `_hook-stop` for the given session id and prints just
# the meaningful stdout (stripping the one-time migration notice). Returns
# the hook's own exit code so the caller can distinguish "valid empty
# result" from "the binary crashed". stderr is intentionally NOT
# suppressed so panic traces or unexpected diagnostics reach the user.
#
# Optional $2 is extra JSON fields appended to the payload (no leading
# comma). Used by the bg/cron guards which need to inject extra keys.
invoke_hook() {
  local sid=$1 extra=${2:-} raw rc=0 payload
  if [ -n "$extra" ]; then
    payload=$(printf '{"session_id":"%s","stop_hook_active":false,%s}\n' "$sid" "$extra")
  else
    payload=$(printf '{"session_id":"%s","stop_hook_active":false}\n' "$sid")
  fi
  raw=$(printf '%s' "$payload" | "$BIN" _hook-stop) || rc=$?
  if [ "$rc" -ne 0 ]; then
    return "$rc"
  fi
  # grep -v exits 1 when every line is filtered out (legitimate empty
  # result), so don't let that masquerade as a hook failure.
  printf '%s\n' "$raw" | grep -v '^\[claude-profiles\] converted' || true
}

expect_empty() {
  local label=$1 sid=$2 extra=${3:-} out rc=0
  out=$(invoke_hook "$sid" "$extra") || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  FAIL: $label — _hook-stop exited $rc"
    FAILURES=$((FAILURES+1))
    return 0
  fi
  if [ -z "$out" ]; then
    echo "  PASS: $label (no output as expected)"
  else
    echo "  FAIL: $label expected empty, got: $out"
    FAILURES=$((FAILURES+1))
  fi
}

expect_block() {
  local label=$1 sid=$2 out rc=0
  out=$(invoke_hook "$sid") || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  FAIL: $label — _hook-stop exited $rc"
    FAILURES=$((FAILURES+1))
    return 0
  fi
  if echo "$out" | grep -q '"decision":"block"'; then
    echo "  PASS: $label (block emitted)"
  else
    echo "  FAIL: $label expected block JSON, got: $out"
    FAILURES=$((FAILURES+1))
  fi
}

bookmark_sha() {
  if [ -f "$CLAUDE_PROFILES_ROOT/last-distill/smoke.json" ]; then
    sed -n 's/.*"head_sha":"\([a-f0-9]*\)".*/\1/p' "$CLAUDE_PROFILES_ROOT/last-distill/smoke.json"
  fi
}

echo "=== A: no commits since wrapper start → expect empty"
expect_empty "A" "s1"

echo "=== B: first non-claude commit → expect block + bookmark"
sleep 2
echo hello > file.txt
git add file.txt
git commit -q -m "non-claude work"
expect_block "B" "s2"
SHA_B=$(bookmark_sha)
if [ -n "$SHA_B" ]; then
  echo "  PASS: bookmark written ($SHA_B)"
else
  echo "  FAIL: bookmark file missing"
  FAILURES=$((FAILURES+1))
fi

echo "=== C: re-fire on same HEAD → expect empty (dedup)"
expect_empty "C" "s3"

echo "=== D: new non-claude commit → expect block, bookmark advances"
sleep 2
echo more >> file.txt
git add file.txt
git commit -q -m "more work"
expect_block "D" "s4"
SHA_D=$(bookmark_sha)
if [ -n "$SHA_D" ] && [ "$SHA_D" != "$SHA_B" ]; then
  echo "  PASS: bookmark advanced ($SHA_B → $SHA_D)"
else
  echo "  FAIL: bookmark did not advance (still $SHA_D)"
  FAILURES=$((FAILURES+1))
fi

echo "=== E: .claude/-only commit → expect empty (filter #1)"
sleep 2
mkdir -p .claude
echo '{}' > .claude/foo.json
git add .claude/foo.json
git commit -q -m "claude-only"
expect_empty "E" "s5"

echo "=== F: uncommitted-only non-claude → expect empty (only-commit policy)"
echo wip > wip.txt
expect_empty "F" "s6"
rm -f wip.txt

echo "=== G: orphaned bookmark (rebase sim) → expect block + self-heal"
echo '{"head_sha":"deadbeef0000000000000000000000000000dead","stamp_utc":1}' \
  > "$CLAUDE_PROFILES_ROOT/last-distill/smoke.json"
sleep 2
echo really >> file.txt
git add file.txt
git commit -q -m "another"
expect_block "G" "s7"
SHA_G=$(bookmark_sha)
if [ -n "$SHA_G" ] && [ "$SHA_G" != "deadbeef0000000000000000000000000000dead" ]; then
  echo "  PASS: bookmark self-healed to live SHA ($SHA_G)"
else
  echo "  FAIL: bookmark not self-healed (still $SHA_G)"
  FAILURES=$((FAILURES+1))
fi

echo "=== H: background_tasks non-empty → expect empty + bookmark stays"
SHA_BEFORE_H=$(bookmark_sha)
sleep 2
echo h-work >> file.txt
git add file.txt
git commit -q -m "would-distill but bg in flight"
expect_empty "H" "s8" '"background_tasks":[{"id":"abc"}]'
SHA_AFTER_H=$(bookmark_sha)
if [ "$SHA_AFTER_H" = "$SHA_BEFORE_H" ]; then
  echo "  PASS: bookmark did not advance under background_tasks"
else
  echo "  FAIL: bookmark advanced ($SHA_BEFORE_H → $SHA_AFTER_H) despite bg in flight"
  FAILURES=$((FAILURES+1))
fi

echo "=== I: session_crons non-empty → expect empty + bookmark stays"
SHA_BEFORE_I=$(bookmark_sha)
expect_empty "I" "s9" '"session_crons":[{"name":"nightly"}]'
SHA_AFTER_I=$(bookmark_sha)
if [ "$SHA_AFTER_I" = "$SHA_BEFORE_I" ]; then
  echo "  PASS: bookmark did not advance under session_crons"
else
  echo "  FAIL: bookmark advanced ($SHA_BEFORE_I → $SHA_AFTER_I) despite cron turn"
  FAILURES=$((FAILURES+1))
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "OK — all distill-trigger scenarios pass."
  exit 0
else
  echo "FAILED — $FAILURES scenario(s) regressed."
  exit 1
fi
