#!/usr/bin/env bash
# Monitor wrapper for kb-tail.py.
#
# Why this exists: Claude Code Monitor subprocesses do NOT reliably inherit
# env vars from the parent `claude` process. SessionStart / Stop hooks DO,
# but Monitor scripts get a stripped env — so env-driven global mode
# (KB_TAIL_DIR + KB_TAIL_SCAN_ALL_PROJECTS) breaks at the Monitor layer even
# when the user has correctly set the vars in their shell before launching.
#
# This wrapper sources an env file from a canonical location before
# invoking the script, so the env file is the source of truth and the
# Monitor sees its values regardless of how Claude Code spawns it.
#
# File search order (first hit wins):
#   1. $KB_TAIL_ENV_FILE  — explicit override (when caller can set env)
#   2. $HOME/.kb/.kb-tail.env  — canonical user default
#
# Repo-mode usage: leave the env file absent. The wrapper exits the source
# loop, exec's the script, and kb-tail.py defaults to git-detected repo
# mode. No friction for users who don't need global.
#
# Global-mode usage: write $HOME/.kb/.kb-tail.env with:
#   KB_TAIL_DIR="$HOME/.kb"
#   KB_TAIL_SCAN_ALL_PROJECTS=1
# (The `kb-global` shell function in the user's rc creates this on every
# invocation, so the file stays in sync with the function's intent.)

set -u

for candidate in \
  "${KB_TAIL_ENV_FILE:-}" \
  "$HOME/.kb/.kb-tail.env"; do
  if [[ -n "$candidate" && -f "$candidate" ]]; then
    # `set -a` exports every var assigned via source — needed because
    # kb-tail.py reads from os.environ, not from the wrapper's args.
    set -a
    # shellcheck disable=SC1090
    source "$candidate"
    set +a
    break
  fi
done

exec python3 "${CLAUDE_PLUGIN_ROOT}/scripts/kb-tail.py" "$@"
