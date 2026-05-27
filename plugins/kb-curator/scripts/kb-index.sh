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

# KB dir resolution priority:
#   1. KB_TAIL_DIR — IS the kb dir directly (no `.kb` appended). Set when
#      the user wants an explicit location (global personal KB or any
#      decoupled use).
#   2. CLAUDE_PROJECT_DIR — set by Claude Code when a hook fires; equals
#      the cwd the session was launched from. We append `.kb` here because
#      this variable names the PROJECT root, not the KB.
#   3. git rev-parse --show-toplevel — repo root fallback; append `.kb`.
if [[ -n "${KB_TAIL_DIR:-}" ]]; then
  kb="$KB_TAIL_DIR"
elif [[ -n "${CLAUDE_PROJECT_DIR:-}" ]]; then
  kb="$CLAUDE_PROJECT_DIR/.kb"
else
  proj=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
  kb="$proj/.kb"
fi
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

# build_tag_index — writes .kb/INDEX-by-tag.md grouping every entry by
# the values of its frontmatter `tags:` array. Inline-array form only:
#   tags: [a, b, c]
# Block-list form (`tags:\n  - a\n  - b`) is intentionally not parsed —
# the kb SKILL prompt mandates the inline form, and parsing both would
# double the bash complexity for no real gain. Empty / missing tags →
# entry simply doesn't appear in this index.
#
# The output exists primarily as a *tag-reuse vocabulary* for the kb
# SKILL and for the SessionStart hook to inject. The curator agent is
# instructed to read this before assigning tags to new entries; without
# it tag names drift (kbcurator / kb-curator / kb_curator) and the index
# becomes useless.
build_tag_index() {
  local out="$kb/INDEX-by-tag.md"
  local rows=()  # "tag<TAB>bucket/basename<TAB>title"

  local bucket dir files f base title tags_line t
  for bucket in decisions fixes sessions; do
    dir="$kb/$bucket"
    [[ -d "$dir" ]] || continue
    files=("$dir"/*.md)
    [[ ${#files[@]} -eq 0 ]] && continue
    for f in "${files[@]}"; do
      base=$(basename "$f")
      [[ "$base" == "INDEX.md" ]] && continue
      title=$(awk '/^# / { sub(/^# /, ""); print; exit }' "$f" 2>/dev/null || true)
      [[ -z "$title" ]] && title="${base%.md}"
      # Pull the first `tags:` line that appears inside the YAML
      # frontmatter (between the first two `---` markers).
      tags_line=$(awk '/^---$/{c++; next} c==1 && /^tags:/{print; exit}' "$f" 2>/dev/null || true)
      [[ -z "$tags_line" ]] && continue
      # Strip `tags:`, surrounding whitespace, and `[ ... ]`.
      tags_line=$(printf '%s' "$tags_line" | sed -E 's/^tags:[[:space:]]*\[?//; s/\]?[[:space:]]*$//')
      IFS=',' read -ra _tag_arr <<<"$tags_line"
      for t in "${_tag_arr[@]}"; do
        # Trim leading/trailing whitespace + any surrounding quotes.
        t="${t#"${t%%[![:space:]]*}"}"
        t="${t%"${t##*[![:space:]]}"}"
        t="${t#\"}"; t="${t%\"}"
        t="${t#\'}"; t="${t%\'}"
        [[ -z "$t" ]] && continue
        rows+=("$t"$'\t'"$bucket/$base"$'\t'"$title")
      done
    done
  done

  if [[ ${#rows[@]} -eq 0 ]]; then
    [[ -f "$out" ]] && rm -f "$out"
    return 0
  fi

  local sorted
  sorted=$(printf '%s\n' "${rows[@]}" | LC_ALL=C sort)

  local tmp="$out.tmp.$$"
  {
    echo "# Tags"
    echo
    echo "_Auto-generated $generated_at. Do not edit by hand._"
    echo "_Reuse these tags when curating new entries. New tag = only when no existing one fits._"
    echo
    local last_tag=""
    local tag path title_field
    while IFS=$'\t' read -r tag path title_field; do
      if [[ "$tag" != "$last_tag" ]]; then
        [[ -n "$last_tag" ]] && echo
        echo "## $tag"
        echo
        last_tag="$tag"
      fi
      echo "- [$title_field]($path)"
    done <<<"$sorted"
  } > "$tmp"
  mv "$tmp" "$out"
}

build_tag_index

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
    if [[ -f "$kb/INDEX-by-tag.md" ]]; then
      echo "- [Tags](INDEX-by-tag.md) — cross-bucket index, reuse before inventing"
    fi
  fi
} > "$root_tmp"
mv "$root_tmp" "$root"

exit 0
