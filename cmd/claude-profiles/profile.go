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

type Profile struct {
	McpServers     map[string]ServerConfig `json:"mcpServers"`
	DeniedTools    []string                `json:"_deniedTools,omitempty"`
	PermissionMode string                  `json:"_permissionMode,omitempty"`
	Model          string                  `json:"_model,omitempty"`
	Settings       json.RawMessage         `json:"_settings,omitempty"`
}

var profilesDir = func() string {
	if d := os.Getenv("CLAUDE_MCP_PROFILES_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "mcp-profiles")
}()

// profilePath returns the JSON path for a local profile. Folder-format profiles
// (created by `copy` from a repo with settings.json) live at
// <dir>/<name>/profile.json; flat-format ones at <dir>/<name>.json.
func profilePath(name string) string {
	folder := filepath.Join(profilesDir, name, "profile.json")
	if _, err := os.Stat(folder); err == nil {
		return folder
	}
	return filepath.Join(profilesDir, name+".json")
}

// localSettingsPath returns the sibling settings.json path if the profile is in
// folder format and the file exists, else "".
func localSettingsPath(name string) string {
	p := filepath.Join(profilesDir, name, "settings.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func listProfiles() ([]string, error) {
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, e := range entries {
		var name string
		if e.IsDir() {
			// Folder format: must contain profile.json
			if _, err := os.Stat(filepath.Join(profilesDir, e.Name(), "profile.json")); err != nil {
				continue
			}
			name = e.Name()
		} else if strings.HasSuffix(e.Name(), ".json") {
			name = strings.TrimSuffix(e.Name(), ".json")
		} else {
			continue
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	sort.Strings(names)
	return names, nil
}

func loadProfile(name string) (*Profile, error) {
	data, err := os.ReadFile(profilePath(name))
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.McpServers == nil {
		p.McpServers = map[string]ServerConfig{}
	}
	return &p, nil
}

func saveProfile(name string, p *Profile) error {
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(profilePath(name), append(data, '\n'), 0o644)
}

func profileExists(name string) bool {
	if _, err := os.Stat(filepath.Join(profilesDir, name+".json")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(profilesDir, name, "profile.json")); err == nil {
		return true
	}
	return false
}

// loadProfileAt reads a profile JSON from any absolute path (used for repo
// profiles whose path doesn't follow the local convention).
func loadProfileAt(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.McpServers == nil {
		p.McpServers = map[string]ServerConfig{}
	}
	return &p, nil
}

// claudeFlags returns CLI flags derived from profile settings.
// If settingsPath is non-empty (folder-format profile with a sibling
// settings.json), --settings receives the file path; otherwise the inline
// _settings JSON is used.
func claudeFlags(p *Profile, settingsPath string) []string {
	var args []string
	if len(p.DeniedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(p.DeniedTools, ","))
	}
	if p.PermissionMode != "" {
		args = append(args, "--permission-mode", p.PermissionMode)
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	switch {
	case settingsPath != "":
		args = append(args, "--settings", settingsPath)
	case len(p.Settings) > 0:
		args = append(args, "--settings", string(p.Settings))
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
