package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Authoritative session_id → profile_id ledger. The wrapper writes here as
// soon as it detects the session id for a launched claude. Survives wrapper
// exit so backgrounded sessions remain mappable in the hub long after their
// originating wrapper is gone.

func sessionProfilesPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "claude-profiles-sessions.json")
}

func loadSessionProfiles() map[string]string {
	data, err := os.ReadFile(sessionProfilesPath())
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]string{}
	}
	return m
}

func recordSessionProfile(sessionID, profileID string) {
	if sessionID == "" || profileID == "" {
		return
	}
	m := loadSessionProfiles()
	if m[sessionID] == profileID {
		return // no-op
	}
	m[sessionID] = profileID
	path := sessionProfilesPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
}

// pruneSessionProfiles removes entries whose session id is not in `keep`. Used
// by doctor (or any other caller) to garbage-collect old session→profile
// mappings against the supervisor's current roster.
func pruneSessionProfiles(keep map[string]bool) int {
	m := loadSessionProfiles()
	removed := 0
	for k := range m {
		if !keep[k] {
			delete(m, k)
			removed++
		}
	}
	if removed == 0 {
		return 0
	}
	path := sessionProfilesPath()
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
	return removed
}
