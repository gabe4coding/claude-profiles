package main

import (
	"bytes"
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
	// Description explains why this profile exists. Shown in the hub list and
	// in `claude-profiles list`. Optional — empty means "no rationale recorded".
	Description string                  `json:"_description,omitempty"`
	McpServers  map[string]ServerConfig `json:"mcpServers"`
	DeniedTools []string                `json:"_deniedTools,omitempty"`
	// Settings is the Claude Code settings for this profile. Written as a
	// sibling settings.json in folder format; only used inline for flat
	// profiles that have no other reason to use folder format.
	Settings json.RawMessage `json:"_settings,omitempty"`
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

// saveProfile writes a local profile.
//   - flat  ~/.claude/mcp-profiles/<name>.json          if Settings is empty
//   - folder ~/.claude/mcp-profiles/<name>/profile.json + settings.json otherwise
//
// Settings is never written inline into profile.json — always sidecar. Any
// pre-existing format for this profile is cleaned up so we don't leave both
// flat and folder copies behind.
func saveProfile(name string, p *Profile) error {
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		return err
	}
	flatPath := filepath.Join(profilesDir, name+".json")
	folderDir := filepath.Join(profilesDir, name)
	folderJSON := filepath.Join(folderDir, "profile.json")
	folderSettings := filepath.Join(folderDir, "settings.json")

	// Marshal profile.json without _settings (it lives in the sibling file)
	out := struct {
		Description string                  `json:"_description,omitempty"`
		McpServers  map[string]ServerConfig `json:"mcpServers"`
		DeniedTools []string                `json:"_deniedTools,omitempty"`
	}{p.Description, p.McpServers, p.DeniedTools}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}

	if len(p.Settings) > 0 {
		// Folder format. Remove any stale flat copy.
		_ = os.Remove(flatPath)
		if err := os.MkdirAll(folderDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(folderJSON, append(data, '\n'), 0o644); err != nil {
			return err
		}
		// Re-format settings.json so it's human-friendly on disk
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, p.Settings, "", "  ")
		if pretty.Len() == 0 {
			pretty.Write(p.Settings)
		}
		pretty.WriteByte('\n')
		return os.WriteFile(folderSettings, pretty.Bytes(), 0o644)
	}

	// Flat format. Remove any stale folder copy.
	_ = os.RemoveAll(folderDir)
	return os.WriteFile(flatPath, append(data, '\n'), 0o644)
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

// loadProfileAt reads a profile JSON from any absolute path. If a sibling
// settings.json lives next to it (folder format), the file content overrides
// any inline _settings in profile.json.
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
	sibling := filepath.Join(filepath.Dir(path), "settings.json")
	if sdata, err := os.ReadFile(sibling); err == nil {
		p.Settings = sdata
	}
	return &p, nil
}

// claudeFlags returns CLI flags derived from the profile.
//   - --disallowedTools  from DeniedTools
//   - --settings         from sidecar file (if folder format) or inline JSON
//
// model and permission mode live inside Settings now, so no separate flags.
func claudeFlags(p *Profile, settingsPath string) []string {
	var args []string
	if len(p.DeniedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(p.DeniedTools, ","))
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
