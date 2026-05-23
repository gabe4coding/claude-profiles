---
type: fix
date: 2026-05-22
sessions: [39da33cd]
commits: []
tags: [kb-curator, monitors, self-loop]
---

# kb-tail: prevent self-curation infinite loop

When the kb-curator agent stopped a turn, the Stop hook wrote a new event into `.kb/inbox/`, which woke the curator again to curate its own "no pending work" turn — an endless ping-pong.

Fix: `claude-profiles kb-tail` accepts `--self-agent <name>` (repeatable). For each newly observed transcript jsonl, kb-tail scans the first 40 records looking for a `type=agent-name` metadata entry whose `agentName` matches a configured self-agent. Matches are persisted in `kbTailState.IgnoredTranscripts` (keyed by absolute path) and skipped forever after — one probe per transcript, no re-scanning. `.claude-profiles/kb-curator/monitors/monitors.json` passes `--self-agent kb-curator`.

How to avoid next time: any monitor whose action might produce more events of the same type the monitor watches must have an identity check at the source, not at the consumer. Filtering after the event is written wastes a wake; filtering in kb-tail before emitting to the inbox is the right layer. The 40-line probe budget is small enough that the cost is negligible even for transcripts that never become curator sessions.

Related: [[2026-05-22-kb-tail-monitor]]
