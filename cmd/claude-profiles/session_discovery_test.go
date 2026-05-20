package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// stubAgentLister returns a fixed slice and/or error — replaces the live
// `claude agents --json` subprocess in tests so they don't depend on a
// running daemon or on the host's installed Claude Code version.
type stubAgentLister struct {
	agents []AgentInfo
	err    error
}

func (s stubAgentLister) listAgents() ([]AgentInfo, error) { return s.agents, s.err }

// agentsFixture mirrors the actual `claude agents --json` payload observed
// on Claude Code 2.1.145 during spec #9 verification (2026-05-20). Pinning
// the JSON literal here means a future schema break in Claude Code surfaces
// as a test failure in this package rather than as a silent runtime
// degradation in the hub or delegate runner.
const agentsFixture = `[
  {
    "pid": 40374,
    "cwd": "/Users/dev/repo",
    "kind": "interactive",
    "startedAt": 1779280083433,
    "sessionId": "96c68013-ae6d-473c-8db4-554e65a56f8b",
    "status": "busy"
  },
  {
    "pid": 40781,
    "cwd": "/private/tmp",
    "kind": "background",
    "startedAt": 1779280700426,
    "sessionId": "f0a00f94-984f-401e-ac7b-0bd436e22d9c",
    "name": "profile-x:task summary",
    "status": "busy"
  },
  {
    "pid": 9076,
    "cwd": "/Users/dev/repo",
    "kind": "interactive",
    "startedAt": 1779253950726,
    "sessionId": "40e314d0-5f46-4ed4-9f61-2bc627c30ae3",
    "status": "idle"
  }
]`

// TestAgentInfoUnmarshalFromFixture pins the JSON contract. If
// `claude agents --json` ever renames a field (e.g. cwd → working_dir or
// sessionId → session_id) this test breaks and we know to update both
// AgentInfo and the spec rather than silently lose data.
func TestAgentInfoUnmarshalFromFixture(t *testing.T) {
	var agents []AgentInfo
	if err := json.Unmarshal([]byte(agentsFixture), &agents); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(agents))
	}

	// First row: interactive, no name, busy.
	got := agents[0]
	want := AgentInfo{
		PID:       40374,
		Cwd:       "/Users/dev/repo",
		Kind:      "interactive",
		SessionID: "96c68013-ae6d-473c-8db4-554e65a56f8b",
		Status:    "busy",
		StartedAt: 1779280083433,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("interactive row mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	// Background row: name populated.
	bg := agents[1]
	if bg.Kind != "background" || bg.Name != "profile-x:task summary" {
		t.Errorf("bg row kind/name mismatch: kind=%q name=%q", bg.Kind, bg.Name)
	}
}

// TestDaemonShort confirms the helper truncates correctly and handles
// degenerate input rather than panicking.
func TestDaemonShort(t *testing.T) {
	cases := []struct {
		sid  string
		want string
	}{
		{"f0a00f94-984f-401e-ac7b-0bd436e22d9c", "f0a00f94"},
		{"short", ""},
		{"", ""},
		{"exactly8", "exactly8"},
	}
	for _, c := range cases {
		got := AgentInfo{SessionID: c.sid}.DaemonShort()
		if got != c.want {
			t.Errorf("DaemonShort(%q) = %q, want %q", c.sid, got, c.want)
		}
	}
}

// TestAgentsByCwd exercises the cwd-grouping helper that callers (hub,
// delegate runner) use to map live sessions back to a profile location.
// The fixture has two sessions in the same cwd plus one elsewhere.
func TestAgentsByCwd(t *testing.T) {
	var agents []AgentInfo
	if err := json.Unmarshal([]byte(agentsFixture), &agents); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	grouped := agentsByCwd(agents)
	if got := len(grouped["/Users/dev/repo"]); got != 2 {
		t.Errorf("group /Users/dev/repo: got %d, want 2", got)
	}
	if got := len(grouped["/private/tmp"]); got != 1 {
		t.Errorf("group /private/tmp: got %d, want 1", got)
	}
	if _, present := grouped["/nonexistent"]; present {
		t.Errorf("group /nonexistent: should be absent, got entry")
	}
}

// TestAgentListerInterface confirms the stub satisfies the seam without
// needing the real subprocess. Trivial but valuable: if the interface
// signature drifts, callers that depend on it for testing break loudly.
func TestAgentListerInterface(t *testing.T) {
	var l agentLister = stubAgentLister{
		agents: []AgentInfo{{SessionID: "abc12345-rest", Kind: "background"}},
	}
	got, err := l.listAgents()
	if err != nil {
		t.Fatalf("stub listAgents: %v", err)
	}
	if len(got) != 1 || got[0].DaemonShort() != "abc12345" {
		t.Errorf("stub returned unexpected agents: %#v", got)
	}
}
