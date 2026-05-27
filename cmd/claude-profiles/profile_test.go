package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestLoadProfileAt_PluginOnlySettings locks the regression fix for the
// case where a profile ships agents/skills/monitors/hooks + settings.json
// but no .mcp.json (plugin-only profile, e.g. kb-curator). The previous
// implementation gated settings.json loading on .mcp.json existence,
// silently dropping model / agent / permissions / statusLine from
// every plugin-only profile.
func TestLoadProfileAt_PluginOnlySettings(t *testing.T) {
	dir := t.TempDir()

	// Plugin-only layout: settings.json + a plugin-content subdir, no
	// profile.json and no .mcp.json.
	settingsJSON := `{
  "model": "haiku",
  "agent": "kb-curator",
  "permissions": {"defaultMode": "auto"}
}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}

	p, err := loadProfileAt(filepath.Join(dir, "profile.json"))
	if err != nil {
		t.Fatalf("loadProfileAt: %v", err)
	}
	if len(p.Settings) == 0 {
		t.Fatal("expected settings.json content in p.Settings, got empty")
	}
	s := string(p.Settings)
	if !strings.Contains(s, `"model": "haiku"`) {
		t.Errorf("model not preserved in p.Settings: %s", s)
	}
	if !strings.Contains(s, `"agent": "kb-curator"`) {
		t.Errorf("agent not preserved in p.Settings: %s", s)
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
