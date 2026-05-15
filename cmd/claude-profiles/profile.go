package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ServerConfig struct {
	Type    string   `json:"type,omitempty"`
	URL     string   `json:"url,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// ProfilePrompt is a named, ready-to-send message stored in a profile. When a
// profile defines prompts the wrapper shows a picker before launching so the
// user can start the session with a pre-filled message instead of typing it.
type ProfilePrompt struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type Profile struct {
	// Description explains why this profile exists. Shown in the hub list and
	// in `claude-profiles list`. Optional — empty means "no rationale recorded".
	Description string                  `json:"_description,omitempty"`
	McpServers  map[string]ServerConfig `json:"mcpServers"`
	DeniedTools []string                `json:"_deniedTools,omitempty"`
	// Settings is the Claude Code settings for this profile. Inlined into
	// profile.json as `_settings`.
	Settings json.RawMessage `json:"_settings,omitempty"`
	// Isolated (when true) tells the wrapper to pass `--setting-sources=` so
	// claude loads NO user/project/local settings.json — only the profile's
	// inline _settings (plus our SessionStart hook) are in effect. Plugins,
	// slash commands, agents, and CLAUDE.md from the host are NOT affected by
	// this flag — those require --bare, which would break /switch. Default
	// is false (profile blends with the user's root configuration as before).
	Isolated bool `json:"_isolated,omitempty"`
	// Prompts is an optional list of named messages offered to the user before
	// the session starts. Selecting one sends it as the initial message;
	// skipping starts an interactive session with no pre-filled text.
	Prompts []ProfilePrompt `json:"_prompts,omitempty"`
}

// ── Settings (JSON map) helpers ──────────────────────────────────────────────
//
// Settings on disk are stored as a JSON object whose schema is Claude Code's
// settings.json. We manipulate them as map[string]any so unknown keys round-trip.

func parseSettings(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) > 0 {
		json.Unmarshal(raw, &out)
		if out == nil {
			out = map[string]any{}
		}
	}
	return out
}

func marshalSettings(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

func getModel(s map[string]any) string {
	v, _ := s["model"].(string)
	return v
}

func setModel(s map[string]any, model string) {
	if model == "" {
		delete(s, "model")
		return
	}
	s["model"] = model
}

func getPermissionMode(s map[string]any) string {
	p, ok := s["permissions"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := p["defaultMode"].(string)
	return v
}

func setPermissionMode(s map[string]any, mode string) {
	p, _ := s["permissions"].(map[string]any)
	if mode == "" {
		if p != nil {
			delete(p, "defaultMode")
			if len(p) == 0 {
				delete(s, "permissions")
			}
		}
		return
	}
	if p == nil {
		p = map[string]any{}
	}
	p["defaultMode"] = mode
	s["permissions"] = p
}

// profilesDir is the local-profiles directory. Defined as a function so
// CLAUDE_PROFILES_ROOT changes during tests take effect immediately.
func profilesDir() string { return profilesDirPath() }

// profilePath returns the JSON path for a local profile. Every profile is a
// folder containing profile.json — this gives us a natural place to drop
// future per-profile artifacts (CLAUDE.md, hooks, etc.) without changing the
// loader.
func profilePath(name string) string {
	return filepath.Join(profilesDir(), name, "profile.json")
}

func listProfiles() ([]string, error) {
	entries, err := os.ReadDir(profilesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(profilesDir(), e.Name(), "profile.json")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func loadProfile(name string) (*Profile, error) {
	return loadProfileAt(profilePath(name))
}

// saveProfileAt writes a profile in split format to dir:
//   - profile.json  — metadata only (_description, _isolated)
//   - .mcp.json     — MCP server configs
//   - settings.json — Claude Code settings; DeniedTools is authoritative for
//                     permissions.deny (overwrites any existing deny list)
func saveProfileAt(dir string, p *Profile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// profile.json — metadata only
	type profileMeta struct {
		Description string `json:"_description,omitempty"`
		Isolated    bool   `json:"_isolated,omitempty"`
	}
	metaData, err := json.MarshalIndent(profileMeta{Description: p.Description, Isolated: p.Isolated}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), append(metaData, '\n'), 0o644); err != nil {
		return err
	}

	// .mcp.json — MCP server configs
	servers := p.McpServers
	if servers == nil {
		servers = map[string]ServerConfig{}
	}
	mcpData, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), append(mcpData, '\n'), 0o644); err != nil {
		return err
	}

	// settings.json — settings with DeniedTools as authoritative permissions.deny
	s := parseSettings(p.Settings)
	perms, _ := s["permissions"].(map[string]any)
	if len(p.DeniedTools) > 0 {
		if perms == nil {
			perms = map[string]any{}
		}
		perms["deny"] = p.DeniedTools
		s["permissions"] = perms
	} else if perms != nil {
		delete(perms, "deny")
		if len(perms) == 0 {
			delete(s, "permissions")
		}
	}
	settingsPath := filepath.Join(dir, "settings.json")
	if len(s) > 0 {
		settingsData, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(settingsPath, append(settingsData, '\n'), 0o644); err != nil {
			return err
		}
	} else {
		_ = os.Remove(settingsPath)
	}
	return nil
}

// saveProjectProfile writes a profile to .claude-profiles/<name>/ in (or
// above) the current working directory. Returns the absolute path of
// profile.json. Creates the directory if absent.
func saveProjectProfile(name string, p *Profile) (string, error) {
	root := findCwdProfilesDir()
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = filepath.Join(cwd, reposProfileDir)
	}
	dir := filepath.Join(root, name)
	if err := saveProfileAt(dir, p); err != nil {
		return "", err
	}
	return filepath.Join(dir, "profile.json"), nil
}

// saveProfile writes a local profile to <profilesDir>/<name>/ in split format.
func saveProfile(name string, p *Profile) error {
	return saveProfileAt(filepath.Join(profilesDir(), name), p)
}

func profileExists(name string) bool {
	_, err := os.Stat(filepath.Join(profilesDir(), name, "profile.json"))
	return err == nil
}

// pluginSubdirs lists the folder names claude's --plugin-dir auto-discovers.
// If any of these live inside the profile folder, we load the profile folder
// as a plugin via --plugin-dir at launch.
var pluginSubdirs = []string{"commands", "skills", "agents", "hooks"}

// pluginDirFor returns the absolute folder path to pass to `--plugin-dir`
// for this profile, or "" if the profile bundles no plugin content. The
// folder is profilePath(name)'s parent directory (i.e. the profile root).
func pluginDirFor(loc ProfileLocation) string {
	root := filepath.Dir(loc.JSONPath)
	for _, sub := range pluginSubdirs {
		if info, err := os.Stat(filepath.Join(root, sub)); err == nil && info.IsDir() {
			return root
		}
	}
	return ""
}

// profilePluginKinds returns the subset of plugin-content kinds the profile
// folder actually has on disk. Used for status/tagging in list + hub views.
func profilePluginKinds(loc ProfileLocation) []string {
	root := filepath.Dir(loc.JSONPath)
	out := make([]string, 0, len(pluginSubdirs))
	for _, sub := range pluginSubdirs {
		if info, err := os.Stat(filepath.Join(root, sub)); err == nil && info.IsDir() {
			out = append(out, sub)
		}
	}
	return out
}

// loadProfileAt reads a profile from path (must point at profile.json). Supports
// both the split format (profile.json + .mcp.json + settings.json) and the old
// combined format (everything inline in profile.json). Falls back to combined
// format when .mcp.json is absent — covers repo profiles, project profiles, and
// un-migrated local profiles.
func loadProfileAt(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err == nil {
		// Split format: .mcp.json holds servers; settings.json holds settings.
		var mcpFile struct {
			McpServers map[string]ServerConfig `json:"mcpServers"`
		}
		if raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json")); err == nil {
			_ = json.Unmarshal(raw, &mcpFile)
		}
		p.McpServers = mcpFile.McpServers

		if raw, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
			p.Settings = raw
			var s map[string]any
			if json.Unmarshal(raw, &s) == nil {
				if perms, ok := s["permissions"].(map[string]any); ok {
					if deny, ok := perms["deny"].([]any); ok {
						p.DeniedTools = make([]string, 0, len(deny))
						for _, d := range deny {
							if str, ok := d.(string); ok {
								p.DeniedTools = append(p.DeniedTools, str)
							}
						}
					}
				}
			}
		}
	} else if len(p.DeniedTools) > 0 {
		// Old combined format: synthesize permissions.deny into Settings so
		// claudeFlags doesn't need --disallowedTools.
		s := parseSettings(p.Settings)
		if perms, _ := s["permissions"].(map[string]any); perms == nil || perms["deny"] == nil {
			if perms == nil {
				perms = map[string]any{}
			}
			perms["deny"] = p.DeniedTools
			s["permissions"] = perms
			p.Settings = marshalSettings(s)
		}
	}

	if p.McpServers == nil {
		p.McpServers = map[string]ServerConfig{}
	}
	return &p, nil
}

// claudeFlags returns CLI flags derived from the profile.
//   - --settings         augmented JSON written by SessionStart hook, or
//                        inline Settings JSON from the profile. Denied tools
//                        live in settings.json as permissions.deny.
//   - --setting-sources= when Isolated, so claude loads NO user/project/local
//                        settings — only the explicit --settings file applies.
func claudeFlags(p *Profile, settingsPath string) []string {
	var args []string
	switch {
	case settingsPath != "":
		args = append(args, "--settings", settingsPath)
	case len(p.Settings) > 0:
		args = append(args, "--settings", string(p.Settings))
	}
	if p.Isolated {
		args = append(args, "--setting-sources=")
	}
	return args
}

// applyWhitelist denies all tools not in keep, merging with existing denials.
func applyWhitelist(p *Profile, allTools []ToolInfo, keep []string) {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	denied := make(map[string]bool)
	for _, t := range p.DeniedTools {
		denied[t] = true
	}
	for _, t := range allTools {
		if !keepSet[t.Name] {
			denied[t.Name] = true
		}
	}
	p.DeniedTools = sortedKeys(denied)
}

// applyDeny adds specific tools to the denial list.
func applyDeny(p *Profile, tools []string) {
	denied := make(map[string]bool)
	for _, t := range p.DeniedTools {
		denied[t] = true
	}
	for _, t := range tools {
		denied[t] = true
	}
	p.DeniedTools = sortedKeys(denied)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func printProfileList(names []string) {
	for i, name := range names {
		p, err := loadProfile(name)
		var servers, filter string
		if err == nil {
			snames := make([]string, 0, len(p.McpServers))
			for k := range p.McpServers {
				snames = append(snames, k)
			}
			sort.Strings(snames)
			servers = "[" + strings.Join(snames, ", ") + "]"
			if len(p.DeniedTools) > 0 {
				filter = fmt.Sprintf(" [deny:%d]", len(p.DeniedTools))
			}
		}
		fmt.Fprintf(os.Stderr, "  %2d) %-24s %s%s\n", i+1, name, servers, filter)
	}
}
