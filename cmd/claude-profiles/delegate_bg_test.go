package main

import "testing"

// The display-name format is a roundtrip contract between bgDisplayName
// (writer, used at dispatch time) and parseGoalFromName (reader, used by
// `claude-profiles goal list`). If these drift, goal grouping silently
// stops working — this test pins the contract.
func TestBgDisplayNameGoalRoundtrip(t *testing.T) {
	cases := []struct {
		profile, task, goal string
		wantGoal            string
		wantContainsTask    string
	}{
		{"refactor-auth", "rip out the legacy session middleware", "auth-cleanup", "auth-cleanup", "rip out the legacy session middleware"},
		{"refactor-auth", "task", "", "", "task"},
		// A profile or task that legitimately contains " | " must not be
		// mistaken for a goal prefix when no --goal was passed.
		{"weird | profile", "task with | pipe", "", "", "task with | pipe"},
		// Non-ASCII task: truncation must be rune-aware, and the goal prefix
		// must still parse back identically.
		{"emoji", "🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀🚀", "rockets", "rockets", "🚀"},
	}
	for _, c := range cases {
		got := bgDisplayName(c.profile, c.task, c.goal)
		if g := parseGoalFromName(got); g != c.wantGoal {
			t.Errorf("bgDisplayName(%q,%q,%q)=%q → parseGoalFromName=%q, want %q",
				c.profile, c.task, c.goal, got, g, c.wantGoal)
		}
		if c.wantContainsTask != "" && !contains(got, c.wantContainsTask) {
			t.Errorf("bgDisplayName(%q,%q,%q)=%q does not contain task fragment %q",
				c.profile, c.task, c.goal, got, c.wantContainsTask)
		}
	}
}

func TestValidateGoalName(t *testing.T) {
	good := []string{"", "auth-cleanup", "feat42", "ab"}
	for _, n := range good {
		if err := validateGoalName(n); err != nil {
			t.Errorf("validateGoalName(%q) returned %v, want nil", n, err)
		}
	}
	bad := []string{" leading", "trailing ", "with|pipe", "with:colon", "tab\there", "new\nline", "spaces inside"}
	for _, n := range bad {
		if err := validateGoalName(n); err == nil {
			t.Errorf("validateGoalName(%q) returned nil, want error", n)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
