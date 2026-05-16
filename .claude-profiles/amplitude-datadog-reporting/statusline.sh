#!/bin/bash
# Status line for the amplitude-datadog-reporting profile.
# Reads Claude Code's session JSON on stdin and prints two lines:
#   line 1 — identity: 🔒 [profile · EU] <model> · <ctx%> · $<cost>
#   line 2 — activity: 🔌 amp:<N>  dd:<N>  · ⏱ <minutes>m
#
# `set -e` is intentionally OFF: status-line scripts must never abort
# mid-render (a partial first line is worse than a missing second one).

set -u

input=$(cat)

MODEL=$(jq -r '.model.display_name // "?"' <<<"$input")
PCT=$(jq -r '.context_window.used_percentage // 0' <<<"$input" | awk '{printf "%d", $1}')
COST=$(jq -r '.cost.total_cost_usd // 0' <<<"$input")
DUR_MS=$(jq -r '.cost.total_duration_ms // 0' <<<"$input")
TRANSCRIPT=$(jq -r '.transcript_path // empty' <<<"$input")

# Per-server MCP call counts from the transcript (JSONL — one event per line).
# Matches both `mcp__<server>__<tool>` (documented) and `mcp:<server>:<tool>`
# (legacy) naming.
count_mcp_calls() {
  local server=$1 file=$2 n
  [[ -n "$file" && -f "$file" ]] || { echo 0; return; }
  # grep -c exits 1 when there are zero matches but still prints "0", so we
  # capture the count and then swallow the non-zero exit with `|| n=0`.
  n=$(grep -cE "\"name\":\"mcp__${server}__|\"name\":\"mcp:${server}:" "$file" 2>/dev/null) || n=0
  echo "$n"
}

AMP=$(count_mcp_calls amplitude "$TRANSCRIPT")
DD=$(count_mcp_calls datadog "$TRANSCRIPT")

MIN=$(( DUR_MS / 60000 ))
COST_FMT=$(printf "%.2f" "$COST")

# ANSI styling — terminals that don't support it drop the codes silently.
DIM=$'\033[2m'
GREEN=$'\033[32m'
YELLOW=$'\033[33m'
CYAN=$'\033[36m'
RESET=$'\033[0m'

printf "🔒 ${GREEN}[amplitude-datadog-reporting · EU]${RESET} ${CYAN}%s${RESET} ${DIM}·${RESET} %s%% ctx ${DIM}·${RESET} \$%s\n" \
  "$MODEL" "$PCT" "$COST_FMT"
printf "🔌 ${YELLOW}amp:%s  dd:%s${RESET}  ${DIM}· ⏱ %sm${RESET}\n" \
  "$AMP" "$DD" "$MIN"
