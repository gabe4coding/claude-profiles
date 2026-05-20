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

### Verified schema (live runs on Claude Code 2.1.145, 2026-05-20)

#### Interactive rows
```json
{ "pid": 40374, "cwd": "/Users/.../claude-profiles", "kind": "interactive",
  "startedAt": 1779280083433, "sessionId": "96c68013-...", "status": "busy" }
```
- **No `name` field.**
- **No `jsonl_path` field.**
- Status observed: `busy`, `idle`.

#### Background rows (live `claude --bg` job)
```json
{ "pid": 40781, "cwd": "/private/tmp", "kind": "background",
  "startedAt": 1779280700426, "sessionId": "f0a00f94-...",
  "name": "write cheese poem", "status": "busy" }
```
- **`name` IS present on bg rows.**
- When `claude --bg` is invoked **without `--name`**, the daemon auto-generates the name (`nameSource:"auto"` in `state.json`). While the job is `busy` it shows the raw prompt; after completion it gets summarised to a short string.
- When `claude-profiles --bg` dispatches (via `cmdDelegateBgDispatch` → `delegate_bg.go:195`), it **always passes `--name <bgDisplayName>`**, so the bg row's `name` field is **stable and grep-able** — format `<profile>:<task>` or `goal:<g> | <profile>:<task>` (see `bgDisplayName` for the contract).
- **Still no `jsonl_path` field.** The JSONL path lives in `~/.claude/jobs/<daemonShort>/state.json` as the `linkScanPath` key. `daemonShort` is the first 8 hex chars of `sessionId`.
- Status observed: `busy`, `idle`. A bg job that has reached a terminal `state.json.state` (the bg watcher in `delegate_bg.go:364` treats `blocked|completed|failed|stopped` as terminal; values like `done` have also been observed in the wild) still shows as `status:"idle"` in `agents --json` until the daemon is killed (e.g. via `claude stop`). `agents --json` status alone cannot distinguish "live and waiting" from "terminated but not stopped"; consumers must cross-reference `state.json.state`.

#### Consequences for matching strategy

The original spec assumed `name == "delegate-<id>"`. Reality is different:

| Delegate path | `kind` | `name` available? | Recommended match |
|---|---|---|---|
| `claude-profiles --bg` | `background` | YES — `bgDisplayName` writes it via `--name` | **Match by `name` prefix** (`goal:<g> \| <profile>:` or `<profile>:`) plus `cwd`, then read `linkScanPath` from `state.json` for JSONL |
| `cmdDelegateRunner` (tmux) | `interactive` | NO | **Match by `pid`** captured from the spawned `claude` subprocess; fall back to `cwd` + earliest `startedAt` after dispatch timestamp |

There is **no path** where `agents --json` directly hands us `jsonl_path`. The JSONL discovery is always a second step: state.json for bg, filesystem scan (current behaviour) for tmux.

### Step 1 — `claudeAgentsJSON()` helper

Add a new file `cmd/claude-profiles/session_discovery.go`:

```go
// AgentInfo is a single row from `claude agents --json`.
// Verified on Claude Code 2.1.145 against both interactive and background
// sessions (see "Verified schema" above). `name` is only present when
// kind=="background"; `jsonl_path` is never present (read linkScanPath from
// the per-job state.json instead).
type AgentInfo struct {
    PID       int    `json:"pid"`
    Cwd       string `json:"cwd"`
    Kind      string `json:"kind"`      // "interactive" | "background"
    SessionID string `json:"sessionId"`
    Status    string `json:"status"`    // "busy" | "idle"
    StartedAt int64  `json:"startedAt"` // epoch ms
    Name      string `json:"name,omitempty"` // bg only; empty for interactive
}

// DaemonShort returns the first 8 chars of the session ID — the directory
// name under ~/.claude/jobs/ for bg jobs.
func (a AgentInfo) DaemonShort() string {
    if len(a.SessionID) < 8 {
        return ""
    }
    return a.SessionID[:8]
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

**Matching strategy** (verified):
- `cmdDelegateRunner` is the tmux path → spawns `kind:"interactive"` rows with no `name`. Match by **PID** captured from `exec.Command("claude", ...).Process.Pid` after start. Fallback: `cwd` (worktree path) + earliest `startedAt` after the runner's dispatch timestamp.

Once matched, the JSONL path is NOT in `agents --json` (verified absent). For the interactive case the path is what `computeExpectedSessionsDir` + the single new file in that dir already gives us — so for the tmux path, agents-JSON matching mostly serves as **confirmation that the right session is running**, not as a replacement for the filesystem scan. The real value of #9 for the tmux path is **early failure detection** (no row matches → fail fast instead of polling for 30s) and **stale-file disambiguation** (use the matched session's `sessionId` to filter the snapshot diff).

If `claudeAgentsJSON()` fails (old Claude Code version, or daemon not running), fall back to the existing `snapshotJSONLInDir` polling — keeping backwards compatibility.

### Step 3 — `cmdDelegateRunner` post-run fallback

Current fallback order:
1. `jsonl-path.txt` if present
2. `snapshotJSONLInDir(expectedSessionsDir)` diff

New fallback order:
1. `jsonl-path.txt` if present
2. `claudeAgentsJSON()` lookup by **PID** of the spawned claude process; from the matched row, derive the JSONL path under `~/.claude/projects/<encoded-cwd>/<sessionId>.jsonl`
3. `snapshotJSONLInDir(expectedSessionsDir)` diff (existing, preserved as last resort)

Step 2 needs the runner to capture the spawned subprocess PID — currently `cmdDelegateRunner` shells out via tmux, so the in-process PID isn't easily available. Two options:
- (a) Write the PID into a file the runner can read post-launch (e.g. tmux `pane_pid` query, or `claude --pid-file`).
- (b) Skip PID matching and use `cwd` + `startedAt` window matching instead — works but coupling to timing.

Pick (b) for the first implementation; revisit (a) if false matches show up in practice.

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

Schema verification is **complete** (see "Verified schema" above; runs captured 2026-05-20).

1. Add `session_discovery.go` with `AgentInfo`, `agentLister` interface, `claudeAgentsJSON()`, `DaemonShort()` helper.
2. Write a unit test with a stub `agentLister` returning fixtures for both `kind:"interactive"` and `kind:"background"` rows, asserting cwd/pid filtering.
3. Update `announceDelegateJSONLPath` to use agents JSON for early-fail / disambiguation (Step 2). Keep filesystem scan as primary discovery for the tmux path.
4. Update `cmdDelegateRunner` post-run fallback (Step 3) with cwd+startedAt window matching.
5. Build, run `./scripts/smoke-distill.sh`, `./scripts/smoke-delegate-bg.sh`, `./scripts/smoke-ui.sh`.
6. Hub annotation (Step 4) can ship as a follow-on commit.

---

## Risks

- `claude agents --json` may not be available on older Claude Code versions. Defensive fallback to filesystem scan handles this.
- **Bg status `idle` is ambiguous.** A live-but-waiting bg job and a done-but-not-stopped bg job both show `status:"idle"`. Consumers that want "still working" must cross-reference `~/.claude/jobs/<daemonShort>/state.json` (`state` field).
- **Bg `name` is only stable when `--name` is passed.** `claude-profiles --bg` always passes it; direct `claude --bg` invocations let the daemon auto-summarise after completion. Don't pin a matching strategy that assumes name immutability for non-claude-profiles bg jobs.
- **`name` is absent on interactive rows.** Tmux-path matching must use PID or cwd+timing — neither is as clean as name matching, and both have failure modes (PID racing, multiple concurrent delegates with the same cwd).
- Adding `session_discovery.go` increases surface area. Keep it minimal — helper + interface + DaemonShort, nothing else.
