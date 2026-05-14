package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Persistent map of "when was each profile last launched". Used by the hub to
// sort the list by recency so the most recently used profiles surface first.
// Population is on every cmdRun launch; lookup is in runHub().

type recentsFile struct {
	Profiles map[string]int64 `json:"profiles"`
}

func recentsPath() string { return recentPath() }

func loadRecents() map[string]int64 {
	data, err := os.ReadFile(recentsPath())
	if err != nil {
		return map[string]int64{}
	}
	var f recentsFile
	if err := json.Unmarshal(data, &f); err != nil || f.Profiles == nil {
		return map[string]int64{}
	}
	return f.Profiles
}

func recordRecentLaunch(profileID string) {
	if profileID == "" {
		return
	}
	r := loadRecents()
	r[profileID] = time.Now().Unix()
	path := recentsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(recentsFile{Profiles: r}, "", "  ")
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
}
