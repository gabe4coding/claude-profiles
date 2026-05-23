---
type: session
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [monitors, profile-subsystem, kb-curator]
---

# Implement kb-tail monitor sub-command and kb-curator profile

Completed implementation of `claude-profiles kb-tail` sub-command to tail the `.kb/inbox/` event stream. Created the `.claude-profiles/kb-curator/` profile with embedded monitor configuration that auto-wakes the agent whenever transcripts or commits are captured. The agent implements curation logic: reads event JSON, resolves transcripts/diffs, decides whether entries merit the KB (decisions/, fixes/, or sessions/ buckets), and moves processed files to `.kb/inbox/processed/`. Passed smoke-test verification of no-emit on cold start, detection of stop + commit events, and idempotency on restart.

**To test end-to-end:** `claude-profiles run kb-curator` in one session; then make a commit or close a turn in another session within this repo. The curator should wake and drain the inbox.

**Key files changed:**
- `cmd/claude-profiles/kbtail.go` — new kb-tail sub-command
- `cmd/claude-profiles/main.go:101`, `profile.go:393` — wiring and profile discovery
- `.claude-profiles/kb-curator/` — profile, monitor config, curator agent
- `scripts/smoke-kb-tail.sh` — verification script (PASSED)

Follow-ups in same session: [[2026-05-22-mcp-config-optional]] (made `.mcp.json` optional so the curator profile needs no placeholder), [[2026-05-22-kb-tail-self-curation]] (added `--self-agent` to break the curator's self-wake loop).

Related: [[kb-curator-agent-spec]]
