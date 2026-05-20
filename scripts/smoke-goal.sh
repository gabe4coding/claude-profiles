#!/usr/bin/env bash
# Smoke test for `claude-profiles goal list` / `goal show`.
#
# These commands are a read-only view over Agent View's state files. Rather
# than dispatching real bg sessions, we stage fixture state.json files under
# a tmp CLAUDE_CONFIG_DIR/jobs/<id>/state.json layout that mirrors what the
# real Claude Code supervisor writes, then assert the grouping output.
#
# What this validates:
#   1. `goal list` reads every jobs/<id>/state.json, parses the "goal:<name> |"
#      prefix from .name, groups sessions by goal, and prints one
#      summary row per goal.
#   2. State counts (working/blocked/completed) are tallied correctly.
#   3. Sessions without a goal prefix are silently skipped (not surfaced as
#      a phantom "" group).
#   4. `goal show <name>` filters to one goal and lists each row with
#      bg-id, state, and the rest of the display name (profile + task).
#   5. `goal show <missing-name>` returns a clean "no sessions tagged" line.
#   6. `goal show <invalid-name>` rejects (validation matches dispatcher).
#
# Run from the repo root:
#   ./scripts/smoke-goal.sh
#
# Exit non-zero if any assertion fails.

set -euo pipefail

SMOKE=$(mktemp -d)
trap 'cd /; rm -rf "$SMOKE"' EXIT

BIN="$SMOKE/claude-profiles"
export CLAUDE_CONFIG_DIR="$SMOKE/claude"
mkdir -p "$CLAUDE_CONFIG_DIR/jobs"

# Build the binary under test. `goal` is a read-only command and does not
# touch CLAUDE_PROFILES_ROOT, but we set it anyway so any stray write would
# land in the tmpdir rather than the developer's real profile root.
export CLAUDE_PROFILES_ROOT="$SMOKE/profroot"
mkdir -p "$CLAUDE_PROFILES_ROOT"
go build -o "$BIN" ./cmd/claude-profiles/

FAILURES=0

assert_contains() {
  local got="$1" needle="$2" what="$3"
  if grep -qF "$needle" <<<"$got"; then
    echo "ok: $what"
  else
    echo "FAIL: $what — '$needle' not in output:"
    echo "----"
    echo "$got"
    echo "----"
    FAILURES=$((FAILURES + 1))
  fi
}

assert_not_contains() {
  local got="$1" needle="$2" what="$3"
  if grep -qF "$needle" <<<"$got"; then
    echo "FAIL: $what — '$needle' was unexpectedly in output:"
    echo "----"
    echo "$got"
    echo "----"
    FAILURES=$((FAILURES + 1))
  else
    echo "ok: $what"
  fi
}

# ---- Stage fixture state.json files ----------------------------------------
#
# Two goals (auth-cleanup, perf), one ungrouped session that should be ignored.
# Display names match the bgDisplayName format: "goal:<g> | <profile>: <task>".

stage_state() {
  local id="$1" state="$2" name="$3"
  local dir="$CLAUDE_CONFIG_DIR/jobs/$id"
  mkdir -p "$dir"
  cat > "$dir/state.json" <<JSON
{
  "state": "$state",
  "detail": "fixture",
  "linkScanPath": "$SMOKE/stub-${id}.jsonl",
  "sessionId": "stub-session-$id",
  "daemonShort": "$id",
  "name": "$name",
  "intent": "smoke"
}
JSON
}

stage_state "auth1aaa" "working"   "goal:auth-cleanup | refactor-auth: rip out legacy session middleware"
stage_state "auth2bbb" "blocked"   "goal:auth-cleanup | refactor-auth: replace JWT validation path"
stage_state "auth3ccc" "completed" "goal:auth-cleanup | refactor-auth: delete dead helpers"
stage_state "perf1ddd" "working"   "goal:perf | bench: profile hot path"
stage_state "noneeee" "working"   "lint: tidy imports"
# Empty .name and missing state.json (corrupt entry) — must be skipped silently.
mkdir -p "$CLAUDE_CONFIG_DIR/jobs/emptyfff"
cat > "$CLAUDE_CONFIG_DIR/jobs/emptyfff/state.json" <<'JSON'
{"state":"working","name":""}
JSON
mkdir -p "$CLAUDE_CONFIG_DIR/jobs/missingggg"

# ---- Step 1: `goal list` ---------------------------------------------------
echo "==> goal list"
LIST_OUT=$("$BIN" goal list)
echo "$LIST_OUT"
echo

# auth-cleanup: 3 total · 1 working, 1 blocked, 1 completed
assert_contains "$LIST_OUT" "auth-cleanup → 3 total · 1 working, 1 blocked, 1 completed" "auth-cleanup summary"
# perf: 1 total · 1 working
assert_contains "$LIST_OUT" "perf → 1 total · 1 working, 0 blocked, 0 completed" "perf summary"
# Ungrouped session must NOT appear as its own goal row.
assert_not_contains "$LIST_OUT" "lint" "ungrouped session not surfaced"
# Empty-name and missing-state.json entries must NOT raise phantom rows.
assert_not_contains "$LIST_OUT" "emptyfff" "empty .name skipped"
assert_not_contains "$LIST_OUT" "missingggg" "missing state.json skipped"

# ---- Step 2: `goal show auth-cleanup` --------------------------------------
echo "==> goal show auth-cleanup"
SHOW_OUT=$("$BIN" goal show auth-cleanup)
echo "$SHOW_OUT"
echo

assert_contains "$SHOW_OUT" "goal:auth-cleanup — 3 session(s)" "show header"
assert_contains "$SHOW_OUT" "auth1aaa" "show lists session 1"
assert_contains "$SHOW_OUT" "auth2bbb" "show lists session 2"
assert_contains "$SHOW_OUT" "auth3ccc" "show lists session 3"
assert_contains "$SHOW_OUT" "rip out legacy session middleware" "show preserves task text"
assert_not_contains "$SHOW_OUT" "perf1ddd" "show filters out other goal"
assert_not_contains "$SHOW_OUT" "noneeee" "show filters out ungrouped"

# ---- Step 3: `goal show <unknown>` -----------------------------------------
echo "==> goal show nonesuch"
MISS_OUT=$("$BIN" goal show nonesuch)
echo "$MISS_OUT"
echo
assert_contains "$MISS_OUT" "No bg sessions tagged goal:nonesuch" "show missing goal clean message"

# ---- Step 4: `goal show <invalid>` rejects --------------------------------
echo "==> goal show 'has:colon' (must fail)"
if "$BIN" goal show 'has:colon' 2>/dev/null; then
  echo "FAIL: goal show accepted an invalid name"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: goal show rejects names containing a colon"
fi

# ---- Step 5: empty jobs dir prints a friendly nothing-here message --------
echo "==> goal list with empty jobs dir"
rm -rf "$CLAUDE_CONFIG_DIR/jobs"
mkdir -p "$CLAUDE_CONFIG_DIR/jobs"
EMPTY_OUT=$("$BIN" goal list)
echo "$EMPTY_OUT"
assert_contains "$EMPTY_OUT" "No goal-tagged bg sessions" "empty list message"

# ---- Summary ---------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
  echo "PASS — all goal smoke assertions green"
  exit 0
fi
echo "FAIL — $FAILURES assertion(s) failed"
exit 1
