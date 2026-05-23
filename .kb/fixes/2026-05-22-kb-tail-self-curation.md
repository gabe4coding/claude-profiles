---
type: fix
date: 2026-05-22
sessions: [39da33cd, 24c809bf]
commits: [d500fcc]
tags: [kb-curator, monitors, self-loop, smoke-tests]
---

# kb-tail: prevent self-curation infinite loop

When the kb-curator agent stopped a turn, the Stop hook wrote a new event into `.kb/inbox/`, which woke the curator again to curate its own "no pending work" turn — an endless ping-pong.

Fix: `claude-profiles kb-tail` accepts `--self-agent <name>` (repeatable). For each newly observed transcript jsonl, kb-tail scans the first 40 records looking for a `type=agent-name` metadata entry whose `agentName` matches a configured self-agent. Matches are persisted in `kbTailState.IgnoredTranscripts` (keyed by absolute path) and skipped forever after — one probe per transcript, no re-scanning. `.claude-profiles/kb-curator/monitors/monitors.json` passes `--self-agent kb-curator`.

How to avoid next time: any monitor whose action might produce more events of the same type the monitor watches must have an identity check at the source, not at the consumer. Filtering after the event is written wastes a wake; filtering in kb-tail before emitting to the inbox is the right layer. The 40-line probe budget is small enough that the cost is negligible even for transcripts that never become curator sessions.

Related: [[2026-05-22-kb-tail-monitor]]

## Follow-up: 2026-05-23 (d500fcc)

The original fix only matched `type=agent-name` records. Claude Code actually writes one of TWO marker records depending on how the agent was selected: `agent-name` when launched via `--agent <foo>` (or `claude-profiles run foo`), but `agent-setting` with payload `{"agentSetting":"<plugin>:<agent>"}` when the agent is pinned via a plugin's `settings.json` (`"agent": "kb-curator"` in `plugins/kb-curator/settings.json` — the real launch path). Curator transcripts carried only `agent-setting:kb-curator:kb-curator`, never matched, and kb-tail emitted their Stop events as regular work → curator woke on itself.

The smoke fixture masked the bug: the fake transcript happened to include BOTH record types mixed, so the buggy code passed the test against the `agent-name` record while the production scenario only ever wrote `agent-setting`. Lesson generalizes — *a fixture that conflates multiple input variants can pass even when the implementation only handles one.* The fix split the regression coverage into per-variant fixtures (5b: `agent-setting`-only; 5c: marker appended AFTER first sight, to cover the race).

Two-part patch:
1. `is_self_agent_transcript()` now matches both record types. For `agent-setting:<plugin>:<agent>`, the trailing agent name, the plugin prefix, AND the full `plugin:agent` string all count against `--self-agent` — so the same flag value works regardless of which form Claude Code emits.
2. Re-probe on EVERY tick for non-ignored transcripts (40-line bounded read, negligible vs per-tick git calls). Closes two adjacent issues: the file-created-before-marker-written race, and recovery on upgrade from a previously-buggy version that had already cached a curator transcript as non-self in `state.json` — re-probing reclassifies it without manual state cleanup.

How to avoid next time: when probing a marker that an external system writes, enumerate every variant the producer may emit BEFORE writing the fixture, and write one fixture per variant. A mixed fixture is a false-positive trap. And: any "probe once at first sight" cache should be promoted to "re-probe each tick" unless the per-tick cost is measurably prohibitive — production races against late-arriving metadata are common and the recovery story matters during upgrades.
