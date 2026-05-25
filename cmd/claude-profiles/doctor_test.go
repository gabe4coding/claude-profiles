package main

import "testing"

// Tests for the version-check helpers added in issues #27 and #28.

func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		input string
		maj   int
		min   int
		pat   int
		ok    bool
	}{
		{"claude 2.1.147", 2, 1, 147, true},
		{"2.1.148", 2, 1, 148, true},
		{"Claude Code 2.1.146 (build abc123)", 2, 1, 146, true},
		{"v2.1.145", 0, 0, 0, false}, // leading 'v' prevents Sscanf from matching — claude --version doesn't emit this format
		{"unknown", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"2.1", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, min, pat, ok := parseClaudeVersion(c.input)
		if ok != c.ok {
			t.Errorf("parseClaudeVersion(%q): got ok=%v want ok=%v", c.input, ok, c.ok)
			continue
		}
		if ok && (maj != c.maj || min != c.min || pat != c.pat) {
			t.Errorf("parseClaudeVersion(%q): got %d.%d.%d want %d.%d.%d",
				c.input, maj, min, pat, c.maj, c.min, c.pat)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		maj, min, pat             int
		needMaj, needMin, needPat int
		want                      bool
	}{
		{2, 1, 148, 2, 1, 146, true},
		{2, 1, 146, 2, 1, 146, true},
		{2, 1, 145, 2, 1, 146, false},
		{2, 2, 0, 2, 1, 146, true},
		{3, 0, 0, 2, 1, 146, true},
		{1, 9, 999, 2, 1, 146, false},
	}
	for _, c := range cases {
		got := versionAtLeast(c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat)
		if got != c.want {
			t.Errorf("versionAtLeast(%d.%d.%d >= %d.%d.%d): got %v want %v",
				c.maj, c.min, c.pat, c.needMaj, c.needMin, c.needPat, got, c.want)
		}
	}
}

func TestCheckClaudeBinaryVersionStatus(t *testing.T) {
	// Unit-test the version-comparison logic in isolation, without invoking
	// the real `claude` binary. We exercise the logic by calling the helpers
	// directly; checkClaudeBinary() itself is integration-level (requires PATH).

	// v2.1.147 must be flagged as known-bad.
	maj, min, pat, ok := parseClaudeVersion("2.1.147")
	if !ok {
		t.Fatal("parseClaudeVersion('2.1.147') returned ok=false")
	}
	foundBad := false
	for _, bad := range knownBadVersions {
		inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
			!versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
		if inRange {
			foundBad = true
			break
		}
	}
	if !foundBad {
		t.Error("v2.1.147 was not flagged as a known-bad release")
	}

	// v2.1.148 must NOT be in the known-bad table.
	maj, min, pat, ok = parseClaudeVersion("2.1.148")
	if !ok {
		t.Fatal("parseClaudeVersion('2.1.148') returned ok=false")
	}
	for _, bad := range knownBadVersions {
		inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
			!versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
		if inRange {
			t.Errorf("v2.1.148 was incorrectly flagged as known-bad: %s", bad.description)
		}
	}

	// v2.1.145 must be below the minimum delegate baseline.
	maj, min, pat, ok = parseClaudeVersion("2.1.145")
	if !ok {
		t.Fatal("parseClaudeVersion('2.1.145') returned ok=false")
	}
	if versionAtLeast(maj, min, pat, minDelegateMaj, minDelegateMin, minDelegatePat) {
		t.Error("v2.1.145 incorrectly passes the minimum-delegate-version check")
	}

	// v2.1.146 must meet the minimum delegate baseline.
	maj, min, pat, ok = parseClaudeVersion("2.1.146")
	if !ok {
		t.Fatal("parseClaudeVersion('2.1.146') returned ok=false")
	}
	if !versionAtLeast(maj, min, pat, minDelegateMaj, minDelegateMin, minDelegatePat) {
		t.Error("v2.1.146 does not pass the minimum-delegate-version check")
	}
}
