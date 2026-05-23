package main

import "testing"

// TestParseClaudeVersion covers the major formats `claude --version` emits.
func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		input         string
		wantMaj       int
		wantMin       int
		wantPat       int
		wantOK        bool
	}{
		{"claude 2.1.147", 2, 1, 147, true},
		{"2.1.147", 2, 1, 147, true},
		{"Claude Code 2.1.148", 2, 1, 148, true},
		{"2.1.146", 2, 1, 146, true},
		{"2.1.0", 2, 1, 0, true},
		{"3.0.0", 3, 0, 0, true},
		// Unparseable inputs.
		{"", 0, 0, 0, false},
		{"no-version-here", 0, 0, 0, false},
		{"1.2", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, min, pat, ok := parseClaudeVersion(c.input)
		if ok != c.wantOK || maj != c.wantMaj || min != c.wantMin || pat != c.wantPat {
			t.Errorf("parseClaudeVersion(%q) = (%d, %d, %d, %v); want (%d, %d, %d, %v)",
				c.input, maj, min, pat, ok,
				c.wantMaj, c.wantMin, c.wantPat, c.wantOK)
		}
	}
}

// TestVersionAtLeast checks ordering semantics.
func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		maj, min, pat             int
		needMaj, needMin, needPat int
		want                      bool
	}{
		{2, 1, 148, 2, 1, 146, true},
		{2, 1, 146, 2, 1, 146, true},
		{2, 1, 145, 2, 1, 146, false},
		{2, 1, 147, 2, 1, 148, false},
		{3, 0, 0, 2, 1, 147, true},
		{2, 2, 0, 2, 1, 200, true},
		{2, 0, 200, 2, 1, 0, false},
	}
	for _, c := range cases {
		got := versionAtLeast(c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat)
		if got != c.want {
			t.Errorf("versionAtLeast(%d,%d,%d, %d,%d,%d) = %v; want %v",
				c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat, got, c.want)
		}
	}
}

// TestCheckClaudeBinaryVersionLogic exercises the version-comparison
// branches of checkClaudeBinary by calling the helpers directly (we can't
// run a real `claude` binary in CI). The cases cover known-bad, below-min,
// and ok ranges.
func TestCheckClaudeBinaryVersionLogic(t *testing.T) {
	type versionCase struct {
		label       string
		maj, min, pat int
		wantStatus  string
	}
	cases := []versionCase{
		// Known-bad: v2.1.147 has the Bash tool regression.
		{"v2.1.147 known-bad", 2, 1, 147, "fail"},
		// Below minimum delegate version.
		{"v2.1.145 below-min", 2, 1, 145, "warn"},
		{"v2.1.144 below-min", 2, 1, 144, "warn"},
		// Minimum reliable version.
		{"v2.1.146 min-ok", 2, 1, 146, "ok"},
		// Above min, not in bad list.
		{"v2.1.148 ok", 2, 1, 148, "ok"},
		{"v2.2.0 ok", 2, 2, 0, "ok"},
	}

	// Replicate the version-check logic from checkClaudeBinary without the
	// actual exec call so this test runs in any environment.
	checkVersion := func(maj, min, pat int) string {
		for _, bad := range knownBadVersions {
			inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
				!versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
			if inRange {
				return "fail"
			}
		}
		if !versionAtLeast(maj, min, pat, minDelegateMaj, minDelegateMin, minDelegatePat) {
			return "warn"
		}
		return "ok"
	}

	for _, c := range cases {
		got := checkVersion(c.maj, c.min, c.pat)
		if got != c.wantStatus {
			t.Errorf("%s: checkVersion(%d,%d,%d) = %q; want %q",
				c.label, c.maj, c.min, c.pat, got, c.wantStatus)
		}
	}
}
