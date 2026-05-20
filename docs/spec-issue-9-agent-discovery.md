# Spec: Replace filesystem session scanning with `claude agents --json`

**Issue**: #9  
**Kind**: enhancement  
**Primary affected files**: `cmd/claude-profiles/delegate.go`, `cmd/claude-profiles/running.go` (or new `cmd/claude-profiles/session_discovery.go`)  
**Related**: HANDOFF.md Atto III (demolition of `result.md` / `jsonl-path.txt`)

---

## Problem

`cmdDelegateRunner` discovers the delegate's session JSONL file by:

1. Pre-computing `expectedSessionsDir` via `computeExpectedSessionsDir(workingDir, p.Worktree, worktreeName)` — which encodes the worktree path using Claude Code's internal project-dir naming convention.
2. Diffing `snapshotJSONLInDir(expectedSessionsDir)` before/after the claude process starts to find the new file.

This is fragile:

- `computeExpectedSessionsDir` must stay in sync with Claude Code's internal path encoding. Any change to how Claude Code names project directories is a silent break (session never found → `result.md` falls back to the error string).
- Stale `.jsonl` files from previous dead sessions in the same dir can confuse the diff.
- `snapshotJSONLInDir` is a polling loop with a 30s timeout — slow to report failure.

`run.go`'s `snapshotSessionFiles` / `sessionDirsToWatch` has the same class of problem for the wrapper's own session discovery.

Claude Code v2.1.145 adds `claude agents --json`, which returns live session metadata directly from the Claude Code daemon. This is the authoritative, stable source.

---

## Proposed changes

### Step 1 — `claudeAgentsJSON()` helper

Add a new file `cmd/claude-profiles/session_discovery.go`:

```go
// AgentInfo is a single row from `claude agents --json`.
// Fields are a subset of Claude Code's agent JSON output; unknown fields are ignored.
type AgentInfo struct {
    SessionID  string `json:"session_id"`
    Status     string `json:"status"`   // working | blocked | completed | failed | stopped
    WorkingDir string `json:"working_dir"`
    Name       string `json:"name"`
    JSONLPath  string `json:"jsonl_path,omitempty"` // present when Claude Code includes it
}

// agentLister is the interface used by session discovery; real impl shells out,
// tests can stub it.
type agentLister interface {
    listAgents() ([]AgentInfo, error)
}

type realAgentLister struct{}

func (realAgentLister) listAgents() ([]AgentInfo, error) {
    out, err := exec.Command("claude", "agents", "--json").Output()
    if err != nil {
        return nil, err
    }
    var agents []AgentInfo
    if err := json.Unmarshal(out, &agents); err != nil {
        return nil, err
    }
    return agents, nil
}

// claudeAgentsJSON calls `claude agents --json` and returns the parsed result.
func claudeAgentsJSON() ([]AgentInfo, error) {
    return realAgentLister{}.listAgents()
}
```

> **Open question**: Does `claude agents --json` include `jsonl_path`? Need to run `claude agents --json` against a live session to check the actual schema. If absent, we can still match by `name` (which is `delegate-<id>` for tmux delegates) and fall back to the computed path. Document verified schema in `AgentInfo` struct comment.

### Step 2 — Replace/supplement `announceDelegateJSONLPath`

Current: polls `snapshotJSONLInDir(expectedSessionsDir)` every 300ms for up to 30s.

New approach (keep both paths for resilience):

```
// Try claudeAgentsJSON first (stable API).
// Fall back to filesystem scan if agents --json fails or returns no match.
```

When `claudeAgentsJSON()` returns an entry with `Name == "delegate-<id>"`, write its `JSONLPath` (or derive it via `linkScanPath` from the bg state file) to `jsonl-path.txt`.

If `claudeAgentsJSON()` fails (old Claude Code version, or daemon not running), fall back to the existing `snapshotJSONLInDir` polling — keeping backwards compatibility.

### Step 3 — `cmdDelegateRunner` post-run fallback

Current fallback order:
1. `jsonl-path.txt` if present
2. `snapshotJSONLInDir(expectedSessionsDir)` diff

New fallback order:
1. `jsonl-path.txt` if present
2. `claudeAgentsJSON()` lookup by `Name == "delegate-<id>"`
3. `snapshotJSONLInDir(expectedSessionsDir)` diff (existing, preserved as last resort)

### Step 4 (optional, lower priority) — Hub annotation

In `listAllLocations()` (hub.go), call `claudeAgentsJSON()` once and annotate each profile's `ProfileLocation` with the count of live sessions whose `WorkingDir` matches the profile's cwd. Display in the hub list as `[2 active]` or similar.

This replaces any ad-hoc tmux/process scanning the hub currently does (if any).

---

## Relation to Atto III

HANDOFF.md Atto III plans to:
- Delete `cmdDelegateRunner`, `writeResultOnFirstTurnComplete`, `result.md` as integration point
- Delete `jsonl-path.txt`
- Redesign parent ↔ delegate handoff to read `~/.claude/jobs/<id>/state.json` directly

Issue #9's changes are **Atto II.5** — a resilience improvement to the current tmux path that doesn't change the integration contract. The `claudeAgentsJSON()` helper and `agentLister` interface introduced here are reusable in Atto III for hub annotation and session status queries.

**Do not implement Step 2/3 changes if Atto II (default flip) is imminent** — the tmux path is slated for deletion anyway. Focus on Step 1 (the helper) and Step 4 (hub annotation) as standalone value.

---

## Implementation order

1. Verify `claude agents --json` JSON schema (run against a live session; document `AgentInfo` fields accurately).
2. Add `session_discovery.go` with `AgentInfo`, `agentLister` interface, `claudeAgentsJSON()`.
3. Write a unit test with a stub `agentLister` that returns a fixture and verifies name-match logic.
4. Update `announceDelegateJSONLPath` to try agents JSON first (Step 2), keeping filesystem fallback.
5. Update `cmdDelegateRunner` post-run fallback (Step 3).
6. Build, run `./scripts/smoke-distill.sh`, `./scripts/smoke-delegate-bg.sh`, `./scripts/smoke-ui.sh`.
7. Hub annotation (Step 4) can ship as a follow-on commit.

---

## Risks

- `claude agents --json` may not be available on older Claude Code versions. The fallback to filesystem scan handles this.
- The JSON schema for `claude agents --json` is not yet verified (as of this spec). Implement the helper defensively: if the output doesn't parse, fall back silently.
- Adding `session_discovery.go` increases surface area. Keep it minimal — only the helper and the interface.
