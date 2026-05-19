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
#
# Run from the repo root:
#   ./scripts/smoke-distill.sh
#
# Exit code is 0 if every scenario matches its expected outcome, 1 otherwise.

set -u

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

run_hook() {
  echo '{"session_id":"'"$1"'","stop_hook_active":false}' \
    | "$BIN" _hook-stop 2>/dev/null \
    | grep -v '^\[claude-profiles\] converted' || true
}

expect_empty() {
  local label=$1 out=$2
  if [ -z "$out" ]; then
    echo "  PASS: $label (no output as expected)"
  else
    echo "  FAIL: $label expected empty, got: $out"
    FAILURES=$((FAILURES+1))
  fi
}

expect_block() {
  local label=$1 out=$2
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
expect_empty "A" "$(run_hook s1)"

echo "=== B: first non-claude commit → expect block + bookmark"
sleep 2
echo hello > file.txt && git add file.txt && git commit -q -m "non-claude work"
expect_block "B" "$(run_hook s2)"
SHA_B=$(bookmark_sha)
if [ -n "$SHA_B" ]; then
  echo "  PASS: bookmark written ($SHA_B)"
else
  echo "  FAIL: bookmark file missing"
  FAILURES=$((FAILURES+1))
fi

echo "=== C: re-fire on same HEAD → expect empty (dedup)"
expect_empty "C" "$(run_hook s3)"

echo "=== D: new non-claude commit → expect block, bookmark advances"
sleep 2
echo more >> file.txt && git add file.txt && git commit -q -m "more work"
expect_block "D" "$(run_hook s4)"
SHA_D=$(bookmark_sha)
if [ -n "$SHA_D" ] && [ "$SHA_D" != "$SHA_B" ]; then
  echo "  PASS: bookmark advanced ($SHA_B → $SHA_D)"
else
  echo "  FAIL: bookmark did not advance (still $SHA_D)"
  FAILURES=$((FAILURES+1))
fi

echo "=== E: .claude/-only commit → expect empty (filter #1)"
sleep 2
mkdir -p .claude && echo '{}' > .claude/foo.json && git add .claude/foo.json && git commit -q -m "claude-only"
expect_empty "E" "$(run_hook s5)"

echo "=== F: uncommitted-only non-claude → expect empty (only-commit policy)"
echo wip > wip.txt
expect_empty "F" "$(run_hook s6)"
rm -f wip.txt

echo "=== G: orphaned bookmark (rebase sim) → expect block + self-heal"
echo '{"head_sha":"deadbeef0000000000000000000000000000dead","stamp_utc":1}' \
  > "$CLAUDE_PROFILES_ROOT/last-distill/smoke.json"
sleep 2
echo really >> file.txt && git add file.txt && git commit -q -m "another"
expect_block "G" "$(run_hook s7)"
SHA_G=$(bookmark_sha)
if [ -n "$SHA_G" ] && [ "$SHA_G" != "deadbeef0000000000000000000000000000dead" ]; then
  echo "  PASS: bookmark self-healed to live SHA ($SHA_G)"
else
  echo "  FAIL: bookmark not self-healed (still $SHA_G)"
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
