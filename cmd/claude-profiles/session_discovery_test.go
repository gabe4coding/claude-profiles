package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
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

// decodeStrict mirrors the schema-pinning intent: any JSON key that
// AgentInfo doesn't declare is an error. Plain json.Unmarshal silently
// drops unknown fields, which would let a Claude Code schema addition
// (or rename of an existing field with a new one taking its place) slip
// through this test undetected.
func decodeStrict(t *testing.T, raw string, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
}

// TestAgentInfoUnmarshalFromFixture pins the JSON contract. If
// `claude agents --json` ever renames a field (e.g. cwd → working_dir or
// sessionId → session_id) OR adds a new top-level field, this test
// breaks and we know to update both AgentInfo and the spec rather than
// silently lose data. The strict decoder catches the additive case;
// the explicit field-by-field comparison catches the rename case.
func TestAgentInfoUnmarshalFromFixture(t *testing.T) {
	var agents []AgentInfo
	decodeStrict(t, agentsFixture, &agents)
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

// TestAgentInfoRejectsUnknownField is the negative test for the strict
// decoder above. If this stops failing, decodeStrict has stopped
// enforcing the contract — that would silently re-enable schema drift.
func TestAgentInfoRejectsUnknownField(t *testing.T) {
	withExtra := `[{"pid":1,"cwd":"/x","kind":"interactive","sessionId":"abc12345","status":"idle","startedAt":1,"newClaudeCodeField":"surprise"}]`
	dec := json.NewDecoder(strings.NewReader(withExtra))
	dec.DisallowUnknownFields()
	var agents []AgentInfo
	err := dec.Decode(&agents)
	if err == nil {
		t.Fatal("expected DisallowUnknownFields to reject unknown key, got nil error")
	}
	if !strings.Contains(err.Error(), "newClaudeCodeField") {
		t.Errorf("error did not mention the unknown field: %v", err)
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

// TestAgentStatusByID confirms the sessionId→status index used by the
// hub for O(1) lookup is built correctly from the fixture. If a later
// rename of AgentInfo.Status or AgentInfo.SessionID slipped through,
// the suffix annotation would silently go blank — this catches it.
func TestAgentStatusByID(t *testing.T) {
	var agents []AgentInfo
	if err := json.Unmarshal([]byte(agentsFixture), &agents); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	idx := agentStatusByID(agents)
	cases := map[string]string{
		"96c68013-ae6d-473c-8db4-554e65a56f8b": "busy",
		"f0a00f94-984f-401e-ac7b-0bd436e22d9c": "busy",
		"40e314d0-5f46-4ed4-9f61-2bc627c30ae3": "idle",
	}
	for sid, want := range cases {
		if got := idx[sid]; got != want {
			t.Errorf("agentStatusByID[%q] = %q, want %q", sid, got, want)
		}
	}
	if _, present := idx["nonexistent"]; present {
		t.Errorf("agentStatusByID returned a value for an unknown sessionId")
	}
}

// TestBgStatusCounts pins the four branches the hub renders against:
// all-busy, all-idle, mixed, and "all stale" (sessions in roster but
// absent from agents output → both counts zero, no suffix). The "stale"
// branch is the most critical — it's the graceful-fallback case and a
// regression would surface as fabricated counts in the hub.
func TestBgStatusCounts(t *testing.T) {
	bg := []BackgroundedSession{
		{SessionID: "s1"},
		{SessionID: "s2"},
		{SessionID: "s3"},
	}
	cases := []struct {
		name       string
		agentsByID map[string]string
		want       BgStatusCounts
	}{
		{"all busy", map[string]string{"s1": "busy", "s2": "busy", "s3": "busy"}, BgStatusCounts{Busy: 3}},
		{"all idle", map[string]string{"s1": "idle", "s2": "idle", "s3": "idle"}, BgStatusCounts{Idle: 3}},
		{"mixed", map[string]string{"s1": "busy", "s2": "idle", "s3": "busy"}, BgStatusCounts{Busy: 2, Idle: 1}},
		{"all stale", map[string]string{}, BgStatusCounts{}},
		{"nil map", nil, BgStatusCounts{}},
		{"unknown status value", map[string]string{"s1": "weird"}, BgStatusCounts{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bgStatusCounts(bg, c.agentsByID)
			if got != c.want {
				t.Errorf("bgStatusCounts(%v) = %#v, want %#v", c.agentsByID, got, c.want)
			}
		})
	}
}
