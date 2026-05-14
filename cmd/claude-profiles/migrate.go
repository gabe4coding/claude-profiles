package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// migrateLegacyLayout ports state written by older versions of the CLI to the
// new ~/.claude-profiles/ root + unified profile format. Idempotent: running
// it on an already-migrated layout is a no-op. Runs once at startup before
// anything else touches disk.
//
// Migration map:
//   ~/.claude/mcp-profiles/                     → ~/.claude-profiles/profiles/
//     <name>.json                                 → profiles/<name>/profile.json
//     <name>/{profile.json,settings.json}         → profiles/<name>/profile.json (inlined)
//   ~/.claude/mcp-profiles-tokens/              → ~/.claude-profiles/tokens/
//   ~/.claude/mcp-profiles-clients/             → ~/.claude-profiles/clients/
//   ~/.claude/mcp-profiles-repos.json           → ~/.claude-profiles/repos.json
//   ~/.claude/mcp-profiles-repos/               → ~/.claude-profiles/repos/
//   ~/.claude/claude-profiles-sessions.json     → ~/.claude-profiles/sessions.json
//   ~/.claude/claude-profiles-recent.json       → ~/.claude-profiles/recent.json
//   ~/.claude/claude-profiles-asks.json         → ~/.claude-profiles/asks.json
//   ~/.claude/claude-profiles-run/              → ~/.claude-profiles/run/
//   ~/.claude/claude-profiles-run-settings.json → ~/.claude-profiles/run-settings.json
//   ~/.claude/.claude-profiles-next             → ~/.claude-profiles/next-marker
func migrateLegacyLayout() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	root := profilesRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return
	}

	moves := []struct{ src, dst string }{
		{filepath.Join(home, ".claude", "mcp-profiles-tokens"), tokensDirPath()},
		{filepath.Join(home, ".claude", "mcp-profiles-clients"), clientsDirPath()},
		{filepath.Join(home, ".claude", "mcp-profiles-repos.json"), reposConfigPathFn()},
		{filepath.Join(home, ".claude", "mcp-profiles-repos"), reposCacheDirPath()},
		{filepath.Join(home, ".claude", "claude-profiles-sessions.json"), sessionsLedgerPath()},
		{filepath.Join(home, ".claude", "claude-profiles-recent.json"), recentPath()},
		{filepath.Join(home, ".claude", "claude-profiles-asks.json"), asksPath()},
		{filepath.Join(home, ".claude", "claude-profiles-run"), runDirPath()},
		{filepath.Join(home, ".claude", "claude-profiles-run-settings.json"), runSettingsPath()},
		{filepath.Join(home, ".claude", ".claude-profiles-next"), nextMarkerPath()},
	}
	for _, m := range moves {
		moveIfPresent(m.src, m.dst)
	}

	migrateProfiles(filepath.Join(home, ".claude", "mcp-profiles"), profilesDirPath())

	// Rename the wrapper-owned plugin from its earlier "wrapper-plugin" name to
	// "claude-profiles" so the slash command's namespace matches the CLI name.
	moveIfPresent(filepath.Join(root, "wrapper-plugin"), wrapperPluginPath())
}

// moveIfPresent moves src→dst only if src exists and dst does not (so a
// migration partially completed by a previous run never clobbers later state).
func moveIfPresent(src, dst string) {
	if _, err := os.Stat(src); err != nil {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	if err := os.Rename(src, dst); err != nil {
		// Cross-device or permission — best-effort, leave src in place.
		return
	}
}

// migrateProfiles converts the old ~/.claude/mcp-profiles directory into the
// new unified layout under ~/.claude-profiles/profiles/. Handles both flat
// (<name>.json) and split (<name>/{profile.json,settings.json}) sources,
// emitting a single <name>/profile.json with _settings inline.
func migrateProfiles(srcDir, dstDir string) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return
	}

	migrated := 0
	for _, e := range entries {
		name := e.Name()
		var data []byte
		var settings []byte

		switch {
		case e.IsDir():
			// Folder format: <name>/profile.json (+ optional settings.json).
			pj := filepath.Join(srcDir, name, "profile.json")
			b, err := os.ReadFile(pj)
			if err != nil {
				continue
			}
			data = b
			if sb, err := os.ReadFile(filepath.Join(srcDir, name, "settings.json")); err == nil {
				settings = sb
			}
		case strings.HasSuffix(name, ".json"):
			// Flat format: <name>.json
			b, err := os.ReadFile(filepath.Join(srcDir, name))
			if err != nil {
				continue
			}
			data = b
			name = strings.TrimSuffix(name, ".json")
		default:
			continue
		}

		dstProfile := filepath.Join(dstDir, name, "profile.json")
		if _, err := os.Stat(dstProfile); err == nil {
			continue // already migrated for this name; skip
		}

		// Inline settings if we found any. Re-marshal so the file ends up
		// indented and round-trips through our struct.
		var p Profile
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if len(settings) > 0 {
			p.Settings = settings
		}
		if p.McpServers == nil {
			p.McpServers = map[string]ServerConfig{}
		}
		out, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstProfile), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(dstProfile, append(out, '\n'), 0o644); err != nil {
			continue
		}
		migrated++
	}
	if migrated > 0 {
		fmt.Fprintf(os.Stderr, "[claude-profiles] migrated %d profile(s) from %s to %s\n",
			migrated, srcDir, dstDir)
	}
}
