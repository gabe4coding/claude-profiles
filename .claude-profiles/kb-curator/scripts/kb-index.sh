#!/usr/bin/env bash
# Regenerate every INDEX.md under <repo>/.kb/.
#
# Layout produced:
#   .kb/INDEX.md                 # root meta-index, links to each bucket
#   .kb/decisions/INDEX.md       # full list, newest first
#   .kb/fixes/INDEX.md           # full list, newest first
#   .kb/sessions/INDEX.md        # full list, newest first
#
# Triggered by the kb-curator plugin's Stop hook (hooks/hooks.json), so it
# fires after every assistant turn in the curator session. Idempotent —
# safe to run any time, no harm if called twice in a row.
#
# Exit semantics: always 0. The hook must not block the assistant turn
# even if the KB is empty, malformed, or this script has a bug. Silent
# failure is the right trade because the index is a convenience, not a
# contract.

set -uo pipefail
shopt -s nullglob

repo_root="${CLAUDE_PROJECT_DIR:-}"
if [[ -z "$repo_root" ]]; then
  repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
fi
kb="$repo_root/.kb"
[[ -d "$kb" ]] || exit 0

generated_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

# bucket_entries <subdir>
# Emits one line per .md file under .kb/<subdir>/ (excluding INDEX.md
# itself), tab-separated as "<YYYY-MM-DD>\t<title>\t<basename.md>", sorted
# newest first. Echoes nothing for empty/missing buckets — callers check
# with `[[ -n "$out" ]]`.
bucket_entries() {
  local bucket="$1"
  local dir="$kb/$bucket"
  [[ -d "$dir" ]] || return 0

  local files=("$dir"/*.md)
  [[ ${#files[@]} -eq 0 ]] && return 0

  local entries=()
  local f
  for f in "${files[@]}"; do
    local base
    base=$(basename "$f")
    [[ "$base" == "INDEX.md" ]] && continue
    local stem="${base%.md}"
    local title
    title=$(awk '/^# / { sub(/^# /, ""); print; exit }' "$f" 2>/dev/null || true)
    [[ -z "$title" ]] && title="$stem"
    local date_prefix="0000-00-00"
    if [[ "$stem" =~ ^([0-9]{4}-[0-9]{2}-[0-9]{2}) ]]; then
      date_prefix="${BASH_REMATCH[1]}"
    fi
    entries+=("$date_prefix"$'\t'"$title"$'\t'"$base")
  done

  [[ ${#entries[@]} -eq 0 ]] && return 0

  printf '%s\n' "${entries[@]}" | sort -r
}

# write_bucket_index <subdir> <human title>
# If the bucket has entries, writes .kb/<subdir>/INDEX.md and prints a
# line on stdout: "<count>\t<subdir>\t<human>" so the caller can build
# the root index. If the bucket is empty, removes any stale INDEX.md
# left behind from earlier runs and prints nothing.
write_bucket_index() {
  local bucket="$1" human="$2"
  local dir="$kb/$bucket"
  local out="$dir/INDEX.md"

  local entries
  entries=$(bucket_entries "$bucket")
  if [[ -z "$entries" ]]; then
    [[ -f "$out" ]] && rm -f "$out"
    return 0
  fi

  local tmp="$out.tmp.$$"
  {
    echo "# $human"
    echo
    echo "_Auto-generated $generated_at. Do not edit by hand._"
    echo
    while IFS=$'\t' read -r d t fname; do
      echo "- [$t]($fname) — $d"
    done <<<"$entries"
  } > "$tmp"
  mv "$tmp" "$out"

  local count
  count=$(printf '%s\n' "$entries" | wc -l | tr -d ' ')
  printf '%s\t%s\t%s\n' "$count" "$bucket" "$human"
}

# Write per-bucket indexes and collect summary lines for the root.
summary=""
for spec in "decisions|Decisions" "fixes|Fixes" "sessions|Sessions"; do
  bucket="${spec%|*}"
  human="${spec#*|}"
  line=$(write_bucket_index "$bucket" "$human")
  [[ -n "$line" ]] && summary+="$line"$'\n'
done

# Root meta-index. Always rewritten so the timestamp tracks the run.
root="$kb/INDEX.md"
root_tmp="$root.tmp.$$"
{
  echo "# KB Index"
  echo
  echo "_Auto-generated $generated_at. Regenerated on every Stop in the kb-curator session — do not edit by hand._"
  echo
  if [[ -z "$summary" ]]; then
    echo "_KB is empty. The kb-curator agent will populate it as events arrive in_ \`.kb/inbox/\`."
  else
    while IFS=$'\t' read -r count bucket human; do
      [[ -z "$count" ]] && continue
      echo "- [$human]($bucket/INDEX.md) — $count entr$([[ $count -eq 1 ]] && echo y || echo ies)"
    done <<<"$summary"
  fi
} > "$root_tmp"
mv "$root_tmp" "$root"

exit 0
