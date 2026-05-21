#!/usr/bin/env bash
# Smoke test for /delegate launch-script flag handling after Atto II.
#
# Atto II flipped the default: bg is now the default dispatch path,
# --legacy-tmux is the opt-in for the old tmux runner. The old --bg flag
# and CLAUDE_PROFILES_DELEGATE_BG env var are rejected loudly so users on
# older scripts learn about the flip instead of silently inheriting the
# new default.
#
# This smoke exercises the bash logic in delegateLaunchScript (the part
# smoke-delegate-bg.sh skips because it calls _delegate-bg-dispatch
# directly). It does NOT exercise the dispatcher itself (already covered)
# nor a real claude binary.
#
# Run from the repo root:
#   ./scripts/smoke-delegate-flags.sh
#
# Exit non-zero on any unexpected behavior.

set -euo pipefail

SMOKE=$(mktemp -d)
trap 'chmod -R u+w "$SMOKE" 2>/dev/null; rm -rf "$SMOKE"' EXIT

BIN="$SMOKE/claude-profiles"
export HOME="$SMOKE/home"
export CLAUDE_PROFILES_ROOT="$HOME/.claude-profiles"
mkdir -p "$HOME"

# Build the binary under test and install scripts via the testing hook.
go build -o "$BIN" ./cmd/claude-profiles/
"$BIN" _install-wrapper-plugin

LAUNCH="$HOME/.claude-profiles/claude-profiles/scripts/delegate-launch.sh"
if [ ! -f "$LAUNCH" ]; then
  echo "FAIL: delegate-launch.sh not installed at $LAUNCH" >&2
  exit 1
fi

# Stub the wrapper pidfile so PARENT_SID resolution succeeds (only
# relevant for tests that reach the dispatch step).
WRAPPER_PID=99999
mkdir -p "$HOME/.claude-profiles/run"
cat > "$HOME/.claude-profiles/run/${WRAPPER_PID}.json" <<JSON
{"pid":$WRAPPER_PID,"profile":"smoke","session_id":"parent-smoke-sid","started_at":0,"cwd":"$SMOKE"}
JSON

# Common env for every test case. Each case overrides as needed.
run_launch() {
  unset TMUX
  unset CLAUDE_PROFILES_DELEGATE_BG
  unset CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    HOME="$HOME" \
    PATH="$PATH" \
    "$@" \
    "$LAUNCH" "smoke" --bg "task body"
}

FAILURES=0
expect_rc_and_msg() {
  local name="$1" want_rc="$2" want_needle="$3"; shift 3
  local out rc
  out=$("$@" 2>&1) && rc=$? || rc=$?
  if [ "$rc" != "$want_rc" ]; then
    echo "FAIL: $name — got rc=$rc, want $want_rc"
    echo "  output: $out"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if [ -n "$want_needle" ] && ! echo "$out" | grep -F -q -- "$want_needle"; then
    echo "FAIL: $name — output missing '$want_needle'"
    echo "  output: $out"
    FAILURES=$((FAILURES + 1))
    return
  fi
  echo "ok: $name"
}

# Case 1: --bg flag → loud rejection, exit 2.
expect_rc_and_msg \
  "rejects --bg flag with migration message" \
  2 "bg is the default" \
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    HOME="$HOME" \
    PATH="$PATH" \
    "$LAUNCH" "smoke" --bg "task body"

# Case 2: CLAUDE_PROFILES_DELEGATE_BG env → loud rejection, exit 2.
expect_rc_and_msg \
  "rejects CLAUDE_PROFILES_DELEGATE_BG env" \
  2 "CLAUDE_PROFILES_DELEGATE_BG is no longer honoured" \
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    CLAUDE_PROFILES_DELEGATE_BG=1 \
    HOME="$HOME" \
    PATH="$PATH" \
    "$LAUNCH" "smoke" "task body"

# Case 3: --legacy-tmux without $TMUX → exit 1 with tmux-required message.
expect_rc_and_msg \
  "rejects --legacy-tmux without tmux" \
  1 "--legacy-tmux requires tmux" \
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    HOME="$HOME" \
    PATH="$PATH" \
    "$LAUNCH" "smoke" --legacy-tmux "task body"

# Case 4: --goal with --legacy-tmux → exit 1 (incompatible — goal needs bg).
# Set fake TMUX so the tmux-required check above passes.
expect_rc_and_msg \
  "rejects --goal with --legacy-tmux" \
  1 "--goal is incompatible with --legacy-tmux" \
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    TMUX="/tmp/fake-tmux,1,0" \
    HOME="$HOME" \
    PATH="$PATH" \
    "$LAUNCH" "smoke" --legacy-tmux --goal "my-goal" "task body"

# Case 5: default invocation (no flags) reaches the dispatch step. Without
# a real claude binary on PATH, `_delegate-bg-dispatch` will fail — what we
# want to confirm is that the bash flag-handling passes through cleanly
# (PROFILE/TASK extracted, request.json written, DELEGATE_* lines emitted)
# before the Go layer takes over. We capture the output and check for the
# DELEGATE_ID + DELEGATE_DIR lines that the bash side writes before
# delegating to Go.
STUB_BIN_DIR="$SMOKE/stubbin"
mkdir -p "$STUB_BIN_DIR"
# Stub claude-profiles _delegate-bg-dispatch so we observe the bash path
# was reached. Real binary stays available under another name for the
# launch script's own jq/uuidgen needs.
cat > "$STUB_BIN_DIR/claude-profiles" <<EOF
#!/usr/bin/env bash
if [ "\$1" = "_delegate-bg-dispatch" ]; then
  echo "STUB_BG_DISPATCHED \$2"
  exit 0
fi
exec "$BIN" "\$@"
EOF
chmod +x "$STUB_BIN_DIR/claude-profiles"

out=$(env \
  CLAUDE_PROFILES_RUN=1 \
  CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
  HOME="$HOME" \
  PATH="$STUB_BIN_DIR:$PATH" \
  "$LAUNCH" "smoke" "task body" 2>&1) && rc=$? || rc=$?

if [ "$rc" != "0" ]; then
  echo "FAIL: default-mode launch returned rc=$rc"
  echo "  output: $out"
  FAILURES=$((FAILURES + 1))
elif ! echo "$out" | grep -q '^DELEGATE_ID='; then
  echo "FAIL: default-mode launch did not print DELEGATE_ID"
  echo "  output: $out"
  FAILURES=$((FAILURES + 1))
elif ! echo "$out" | grep -q '^STUB_BG_DISPATCHED'; then
  echo "FAIL: default-mode launch did not invoke _delegate-bg-dispatch"
  echo "  output: $out"
  FAILURES=$((FAILURES + 1))
else
  echo "ok: default-mode invocation reaches bg dispatcher"
fi

# Case 6: CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX=1 without TMUX → exit 1
# with tmux-required message (env-var form of --legacy-tmux).
expect_rc_and_msg \
  "env CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX=1 picks up legacy mode" \
  1 "--legacy-tmux requires tmux" \
  env -u TMUX -u CLAUDE_PROFILES_DELEGATE_BG -u CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX \
    CLAUDE_PROFILES_RUN=1 \
    CLAUDE_PROFILES_WRAPPER_PID="$WRAPPER_PID" \
    CLAUDE_PROFILES_DELEGATE_LEGACY_TMUX=1 \
    HOME="$HOME" \
    PATH="$PATH" \
    "$LAUNCH" "smoke" "task body"

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "PASS — all flag-handling assertions green"
  exit 0
fi
echo "FAIL — $FAILURES assertion(s) failed"
exit 1
