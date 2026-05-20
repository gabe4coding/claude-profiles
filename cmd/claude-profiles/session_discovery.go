package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// agentsCommandTimeout bounds the `claude agents --json` subprocess so a
// stuck daemon doesn't freeze the hub's tea.Tick refresh loop. 2s is
// generous for a daemon read but short enough that a hang surfaces
// quickly as a graceful-fallback miss rather than a UI freeze.
const agentsCommandTimeout = 2 * time.Second

// AgentInfo is a single row from `claude agents --json`.
//
// Verified on Claude Code 2.1.145 against live interactive and background
// sessions (2026-05-20). The output is the daemon's authoritative view of
// live sessions — both `kind:"interactive"` (regular `claude` invocations)
// and `kind:"background"` (sessions started with `claude --bg`).
//
// Fields not exposed by `agents --json` and intentionally absent here:
//   - The JSONL transcript path. It lives in
//     ~/.claude/jobs/<daemonShort>/state.json under the `linkScanPath`
//     key — only populated for background sessions.
//   - Per-session profile binding. Use the in-repo `roster.json`-backed
//     loadBackgroundedSessions() for that.
//
// Field-specific gotchas:
//   - `Name` is only present on background rows. Interactive rows omit it.
//   - For claude-profiles-dispatched bg sessions, `Name` is the stable
//     bgDisplayName written via `--name`; for direct `claude --bg` calls
//     without `--name`, the daemon auto-summarises after completion, so
//     don't pin matching strategies on name immutability for arbitrary
//     bg jobs.
//   - `Status` values observed: "busy", "idle". A background job that has
//     reached a terminal state.json.state (e.g. "done", "completed",
//     "blocked") still reports `idle` here until `claude stop` runs.
//     Consumers needing "live and working" vs "done and idle" MUST
//     cross-reference state.json.
type AgentInfo struct {
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`      // "interactive" | "background"
	SessionID string `json:"sessionId"` // full UUID
	Status    string `json:"status"`    // "busy" | "idle"
	StartedAt int64  `json:"startedAt"` // epoch milliseconds
	Name      string `json:"name,omitempty"`
}

// DaemonShort returns the first 8 characters of the session ID — the
// directory name under ~/.claude/jobs/ for background sessions. Returns
// empty if SessionID has fewer than 8 characters (degenerate input or
// uninitialised AgentInfo). Does NOT validate UUID format — Claude Code
// daemon emits UUIDs and the consumer's only contract is the 8-char dir
// name, so format validation would be busywork.
func (a AgentInfo) DaemonShort() string {
	if len(a.SessionID) < 8 {
		return ""
	}
	return a.SessionID[:8]
}

// agentLister is the seam between the real `claude agents --json`
// subprocess and the unit tests. Production code should use claudeAgentsJSON;
// tests construct a stub satisfying this interface.
type agentLister interface {
	listAgents() ([]AgentInfo, error)
}

type realAgentLister struct{}

func (realAgentLister) listAgents() ([]AgentInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agentsCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "agents", "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr in the error so callers (and operators) can tell
		// `claude not on PATH` from `claude rejected the flag` from
		// `daemon refused to answer`. Without this, every failure looks
		// the same as "the helper doesn't work" and is hard to diagnose.
		if s := bytes.TrimSpace(stderr.Bytes()); len(s) > 0 {
			return nil, fmt.Errorf("claude agents --json: %w: %s", err, s)
		}
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var agents []AgentInfo
	if err := json.Unmarshal(trimmed, &agents); err != nil {
		return nil, fmt.Errorf("parse claude agents --json output: %w", err)
	}
	return agents, nil
}

// claudeAgentsJSON shells out to `claude agents --json` and returns the
// parsed list of live sessions known to the Claude Code daemon. Returns:
//   - (agents, nil) on success — agents may be empty when no sessions are
//     live, distinct from the error case below.
//   - (nil, nil) when the daemon emits no/whitespace-only output (treat
//     as "no live sessions known"; not an error).
//   - (nil, err) when the subprocess fails (`claude` not on PATH, older
//     version without the flag, daemon refused, or timeout after
//     agentsCommandTimeout). Callers that want graceful degradation
//     should treat any error as "no live session data available" rather
//     than a hard failure.
func claudeAgentsJSON() ([]AgentInfo, error) {
	return realAgentLister{}.listAgents()
}

// agentsByCwd groups agents by their `cwd` value. Useful for hub-style
// annotation where each profile location wants the set of live sessions
// running under it.
func agentsByCwd(agents []AgentInfo) map[string][]AgentInfo {
	out := map[string][]AgentInfo{}
	for _, a := range agents {
		out[a.Cwd] = append(out[a.Cwd], a)
	}
	return out
}
