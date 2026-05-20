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

### Verified schema (observed 2026-05-20 on Claude Code 2.1.145)

`claude agents --json` against two live **interactive** sessions returned:

```json
[
  {
    "pid": 40374,
    "cwd": "/Users/gabrielepavanello/Repositories/claude-profiles",
    "kind": "interactive",
    "startedAt": 1779280083433,
    "sessionId": "96c68013-ae6d-473c-8db4-554e65a56f8b",
    "status": "busy"
  },
  { "pid": 9076, "cwd": "...", "kind": "interactive",
    "startedAt": 1779253950726, "sessionId": "40e314d0-...", "status": "idle" }
]
```

Key observations:
- Field naming is **camelCase** (`sessionId`, `startedAt`), not snake_case.
- The working-directory field is **`cwd`**, not `working_dir`.
- Status values observed: `busy`, `idle`. Documented bg states (`working|blocked|completed|failed|stopped`) are **not present** on interactive rows — they may apply only to `kind:"background"` rows, but no bg session was running at the time so this is **unverified**.
- **No `name` field** on interactive rows.
- **No `jsonl_path` field**.
- `pid`, `kind` fields not previously anticipated.

The bg-session shape — including whether `name`, `jsonl_path`, or status values like `blocked` appear — must be verified against a live `claude --bg` session before committing to a matching strategy. **Do not start implementation until this is done.**

### Step 1 — `claudeAgentsJSON()` helper

Add a new file `cmd/claude-profiles/session_discovery.go`:

```go
// AgentInfo is a single row from `claude agents --json`.
// Verified against interactive sessions on Claude Code 2.1.145 (see spec
// header). Background-session fields (name, jsonl_path, expanded status
// values) are unverified and MUST be confirmed before use.
type AgentInfo struct {
    PID        int    `json:"pid"`
    Cwd        string `json:"cwd"`
    Kind       string `json:"kind"`        // "interactive" | "background" (assumed; verify)
    SessionID  string `json:"sessionId"`
    Status     string `json:"status"`      // interactive: "busy" | "idle". bg: TBD.
    StartedAt  int64  `json:"startedAt"`   // epoch ms

    // Fields below are SPECULATIVE — present in some Claude Code outputs
    // but not observed on interactive rows. Treat zero-value as "not present".
    Name      string `json:"name,omitempty"`
    JSONLPath string `json:"jsonlPath,omitempty"` // camelCase guess; verify exact key
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

### Step 2 — Replace/supplement `announceDelegateJSONLPath`

Current: polls `snapshotJSONLInDir(expectedSessionsDir)` every 300ms for up to 30s.

New approach (keep both paths for resilience):

```
// Try claudeAgentsJSON first (stable API).
// Fall back to filesystem scan if agents --json fails or returns no match.
```

**Matching strategy** depends on the live-bg schema verification (see header). Candidate strategies, in order of preference:
1. If bg rows expose a stable `name` field set to `delegate-<id>`, match by name (cleanest).
2. Otherwise match by `cwd` + earliest `startedAt` after the dispatch timestamp (the runner already knows both).
3. Last resort: match by `pid` — `cmdDelegateRunner` can capture the spawned `claude` PID and look it up directly.

Once matched, derive the JSONL path. If `claude agents --json` exposes it directly, use that. Otherwise read `linkScanPath` from `~/.claude/jobs/<id>/state.json` (already the bg watcher's source of truth — see HANDOFF.md "Hot spots"). Write the resolved path to `jsonl-path.txt`.

If `claudeAgentsJSON()` fails (old Claude Code version, or daemon not running), fall back to the existing `snapshotJSONLInDir` polling — keeping backwards compatibility.

### Step 3 — `cmdDelegateRunner` post-run fallback

Current fallback order:
1. `jsonl-path.txt` if present
2. `snapshotJSONLInDir(expectedSessionsDir)` diff

New fallback order:
1. `jsonl-path.txt` if present
2. `claudeAgentsJSON()` lookup using whichever matching strategy survives schema verification (see Step 2)
3. `snapshotJSONLInDir(expectedSessionsDir)` diff (existing, preserved as last resort)

### Step 4 (optional, lower priority) — Hub annotation

In `listAllLocations()` (hub.go), call `claudeAgentsJSON()` once and annotate each profile's `ProfileLocation` with the count of live sessions whose `cwd` matches the profile's cwd. Display in the hub list as `[2 active]` or similar.

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

1. **Verify the bg-session schema** by dispatching a real `claude --bg` job and capturing `claude agents --json` output while it's working AND while it's blocked. Confirm whether `name`, `jsonl_path` (or `jsonlPath`), and bg-specific status values exist. Update the `AgentInfo` struct accordingly and pick the matching strategy (see Step 2).
2. Add `session_discovery.go` with `AgentInfo`, `agentLister` interface, `claudeAgentsJSON()`.
3. Write a unit test with a stub `agentLister` that returns a fixture and verifies the chosen match strategy.
4. Update `announceDelegateJSONLPath` to try agents JSON first (Step 2), keeping filesystem fallback.
5. Update `cmdDelegateRunner` post-run fallback (Step 3).
6. Build, run `./scripts/smoke-distill.sh`, `./scripts/smoke-delegate-bg.sh`, `./scripts/smoke-ui.sh`.
7. Hub annotation (Step 4) can ship as a follow-on commit.

---

## Risks

- `claude agents --json` may not be available on older Claude Code versions. The fallback to filesystem scan handles this.
- **The bg-session schema is unverified.** Interactive-session schema is verified (see header) but does NOT expose `name` or `jsonl_path`. If bg rows also lack `name`, the cleanest matching strategy collapses and we fall back to `cwd`+`startedAt` or `pid` matching, both of which add coupling to `cmdDelegateRunner` internals.
- Implement the helper defensively: if the output doesn't parse, fall back silently to the filesystem scan.
- Adding `session_discovery.go` increases surface area. Keep it minimal — only the helper and the interface.
