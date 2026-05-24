package main

import "testing"

// TestParseClaudeVersion exercises parseClaudeVersion against the exact formats
// emitted by the Claude Code binary. The parser must handle "2.1.147 (Claude Code)"
// as well as the bare "2.1.147" form.
func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		input      string
		wantMaj    int
		wantMin    int
		wantPat    int
		wantOK     bool
	}{
		{"2.1.147 (Claude Code)", 2, 1, 147, true},
		{"2.1.146", 2, 1, 146, true},
		{"2.1.148", 2, 1, 148, true},
		{"claude 2.1.150", 2, 1, 150, true},
		{"", 0, 0, 0, false},
		{"not-a-version", 0, 0, 0, false},
		{"v2.1.147", 0, 0, 0, false}, // leading 'v' — Sscanf won't match %d
	}
	for _, c := range cases {
		maj, min, pat, ok := parseClaudeVersion(c.input)
		if ok != c.wantOK || maj != c.wantMaj || min != c.wantMin || pat != c.wantPat {
			t.Errorf("parseClaudeVersion(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.input, maj, min, pat, ok,
				c.wantMaj, c.wantMin, c.wantPat, c.wantOK)
		}
	}
}

// TestVersionAtLeast verifies the comparison helper against boundary values
// near the known thresholds (v2.1.146 minimum, v2.1.147 known-bad).
func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		maj, min, pat             int
		needMaj, needMin, needPat int
		want                      bool
	}{
		{2, 1, 148, 2, 1, 148, true},
		{2, 1, 148, 2, 1, 149, false},
		{2, 1, 146, 2, 1, 146, true},
		{2, 1, 145, 2, 1, 146, false},
		{3, 0, 0, 2, 1, 148, true},
		{1, 9, 999, 2, 0, 0, false},
		{2, 2, 0, 2, 1, 148, true},
	}
	for _, c := range cases {
		got := versionAtLeast(c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat)
		if got != c.want {
			t.Errorf("versionAtLeast(%d.%d.%d >= %d.%d.%d) = %v, want %v",
				c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat, got, c.want)
		}
	}
}

// TestCheckClaudeBinaryVersionLogic exercises the version-classification rules
// used by checkClaudeBinary, via parseClaudeVersion + versionAtLeast directly
// (no subprocess needed — we're testing the logic, not the PATH lookup).
func TestCheckClaudeBinaryVersionLogic(t *testing.T) {
	cases := []struct {
		version    string
		wantStatus string // "ok", "warn", or "fail"
	}{
		{"2.1.148 (Claude Code)", "ok"},  // above known-bad, above min-delegate
		{"2.1.150", "ok"},                 // current production version
		{"2.1.147 (Claude Code)", "fail"}, // known-bad: Bash tool regression
		{"2.1.146", "ok"},                 // minimum reliable delegate baseline
		{"2.1.145", "warn"},               // below minimum — permission re-prompting risk
		{"2.1.100", "warn"},               // well below minimum
	}
	for _, c := range cases {
		got := classifyVersionString(c.version)
		if got != c.wantStatus {
			t.Errorf("classifyVersionString(%q) = %q, want %q", c.version, got, c.wantStatus)
		}
	}
}

// classifyVersionString extracts the version-check logic from checkClaudeBinary
// into a pure function so it can be unit-tested without needing a real claude
// binary on PATH.
func classifyVersionString(version string) string {
	maj, min, pat, ok := parseClaudeVersion(version)
	if !ok {
		return "warn" // unparseable → warn (mirrors checkClaudeBinary)
	}
	for _, bad := range knownBadVersions {
		inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
			!versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
		if inRange {
			return "fail"
		}
	}
	const minDelegateMaj, minDelegateMin, minDelegatePat = 2, 1, 146
	if !versionAtLeast(maj, min, pat, minDelegateMaj, minDelegateMin, minDelegatePat) {
		return "warn"
	}
	return "ok"
}
