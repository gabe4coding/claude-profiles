package main

import (
	"os"
	"path/filepath"
)

// All persistent state owned by claude-profiles lives under one root so users
// can blow it away with `rm -rf ~/.claude-profiles` without taking Claude
// Code's own state (commands/, projects/, daemon/) with it.
//
// Layout:
//   ~/.claude-profiles/
//     profiles/<name>/profile.json     local profile (settings always inline)
//     repos.json                       repo registry
//     repos/<url-hash>/                clone cache for each remote repo
//     tokens/<url-hash>.json           OAuth bearer tokens
//     clients/<endpoint-hash>.json     dynamic OAuth client registrations
//     sessions.json                    session id → profile id ledger
//     recent.json                      most-recently-launched profile ids
//     asks.json                        ask-prompt history
//     run/                             pidfiles of running foreground wrappers
//     run-settings.json                last SessionStart-hook augmented settings
//     next-marker                      transient /switch handoff token
//
// Overridable via CLAUDE_PROFILES_ROOT (rarely needed; mostly for tests).

// reposProfileDir is the per-repo subdirectory we expect remote repos to
// publish their profiles in (e.g. <repo-root>/.claude-profiles/<name>/profile.json).
const reposProfileDir = ".claude-profiles"

func profilesRoot() string {
	if d := os.Getenv("CLAUDE_PROFILES_ROOT"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-profiles")
}

// Sub-paths. Each is a function (not a var) so CLAUDE_PROFILES_ROOT changes
// during tests take effect without re-importing.

func profilesDirPath() string  { return filepath.Join(profilesRoot(), "profiles") }
func tokensDirPath() string    { return filepath.Join(profilesRoot(), "tokens") }
func clientsDirPath() string   { return filepath.Join(profilesRoot(), "clients") }
func reposCacheDirPath() string{ return filepath.Join(profilesRoot(), "repos") }
func reposConfigPathFn() string{ return filepath.Join(profilesRoot(), "repos.json") }
func runDirPath() string       { return filepath.Join(profilesRoot(), "run") }
func runSettingsPath() string  { return filepath.Join(profilesRoot(), "run-settings.json") }
// wrapperPluginPath is the on-disk path of the wrapper-owned plugin dir.
// Folder name is "claude-profiles" so that the slash command shows up as
// `/switch` (or namespaced `/claude-profiles:switch` if the user prefers).
func wrapperPluginPath() string { return filepath.Join(profilesRoot(), "claude-profiles") }
func delegatesDir() string      { return filepath.Join(profilesRoot(), "delegates") }
func sessionsLedgerPath() string { return filepath.Join(profilesRoot(), "sessions.json") }
func recentPath() string       { return filepath.Join(profilesRoot(), "recent.json") }
func asksPath() string         { return filepath.Join(profilesRoot(), "asks.json") }
func nextMarkerPath() string   { return filepath.Join(profilesRoot(), "next-marker") }
func pinsPath() string         { return filepath.Join(profilesRoot(), "pins.json") }
func profilePrefsPath() string { return filepath.Join(profilesRoot(), "profile-prefs.json") }

// claudeRootDirPath stays under ~/.claude — it's Claude Code's own filesystem
// (commands/, projects/, daemon/), not ours.
func claudeRootDirPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}
