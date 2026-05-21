package main

import "testing"

// TestCanonicalProfileDir fixes the path-rewriting contract that
// canonicalProfileDir / mainRepoRoot rely on. Both functions strip a
// `/.claude/worktrees/<name>/` segment, but they diverge on the no-tail
// case (worktree root itself):
//
//   - canonicalProfileDir returns dir unchanged when there's no tail after
//     the worktree name (it's used to canonicalise prefs paths; the
//     mapping is undefined for the bare worktree dir).
//   - mainRepoRoot returns the repo root (i.e. the prefix before
//     `/.claude/worktrees/`) — OwnerRepo canonicalisation needs this even
//     when dir IS the worktree root.
//
// Refactored to strings.Cut in PR #25; this table locks the behaviour so
// the next refactor doesn't silently regress either branch.
func TestCanonicalProfileDir(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{
			name: "no marker — return unchanged",
			dir:  "/Users/x/repo/profiles/foo",
			want: "/Users/x/repo/profiles/foo",
		},
		{
			name: "empty input",
			dir:  "",
			want: "",
		},
		{
			name: "worktree root only (no tail) — return unchanged",
			dir:  "/Users/x/repo/.claude/worktrees/wt1",
			want: "/Users/x/repo/.claude/worktrees/wt1",
		},
		{
			name: "worktree root with trailing slash — empty tail still counts as tail",
			dir:  "/Users/x/repo/.claude/worktrees/wt1/",
			want: "/Users/x/repo/",
		},
		{
			name: "deep path under worktree — remap to main tree",
			dir:  "/Users/x/repo/.claude/worktrees/wt1/sub/dir",
			want: "/Users/x/repo/sub/dir",
		},
		{
			name: "single-segment tail",
			dir:  "/Users/x/repo/.claude/worktrees/wt1/profiles",
			want: "/Users/x/repo/profiles",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalProfileDir(tc.dir)
			if got != tc.want {
				t.Errorf("canonicalProfileDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}

func TestMainRepoRoot(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{
			name: "no marker — return unchanged",
			dir:  "/Users/x/repo",
			want: "/Users/x/repo",
		},
		{
			name: "empty input",
			dir:  "",
			want: "",
		},
		{
			name: "worktree root only (no tail) — return main-tree root",
			dir:  "/Users/x/repo/.claude/worktrees/wt1",
			want: "/Users/x/repo",
		},
		{
			name: "worktree root with trailing slash — main-tree root + slash",
			dir:  "/Users/x/repo/.claude/worktrees/wt1/",
			want: "/Users/x/repo/",
		},
		{
			name: "deep path under worktree — remap to main tree",
			dir:  "/Users/x/repo/.claude/worktrees/wt1/sub/dir",
			want: "/Users/x/repo/sub/dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mainRepoRoot(tc.dir)
			if got != tc.want {
				t.Errorf("mainRepoRoot(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}
