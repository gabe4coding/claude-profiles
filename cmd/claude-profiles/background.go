package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Backgrounded-session awareness: the supervisor daemon (Claude Code 2.1.139+)
// stores every /bg'd session in ~/.claude/daemon/roster.json. We read it
// directly, map each entry back to a claude-profiles profile by parsing its
// --mcp-config flag arg, and surface the result in the hub.

type rosterEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // ms since epoch
	PID       int    `json:"pid"`
	Dispatch  struct {
		Launch struct {
			FlagArgs []string `json:"flagArgs"`
		} `json:"launch"`
	} `json:"dispatch"`
}

type rosterFile struct {
	Workers map[string]rosterEntry `json:"workers"`
}

type BackgroundedSession struct {
	SessionID string
	Cwd       string
	StartedAt time.Time
	PID       int
	Profile   string // qualified id, empty if not launched via claude-profiles
}

func rosterPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "daemon", "roster.json")
}

func loadBackgroundedSessions() []BackgroundedSession {
	data, err := os.ReadFile(rosterPath())
	if err != nil {
		return nil
	}
	var rf rosterFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil
	}
	out := make([]BackgroundedSession, 0, len(rf.Workers))
	locs, _ := listAllLocations()
	ledger := loadSessionProfiles()
	for _, e := range rf.Workers {
		profile := ledger[e.SessionID] // authoritative
		if profile == "" {
			profile = profileFromFlagArgs(e.Dispatch.Launch.FlagArgs, locs)
		}
		if profile == "" {
			// Last-ditch: scan the session's .jsonl for the custom-title
			// event our --name flag wrote. Backfill the ledger so the next
			// lookup is O(1).
			profile = profileFromSessionJSONL(e.SessionID, e.Cwd, locs)
			if profile != "" {
				recordSessionProfile(e.SessionID, profile)
			}
		}
		out = append(out, BackgroundedSession{
			SessionID: e.SessionID,
			Cwd:       e.Cwd,
			StartedAt: time.UnixMilli(e.StartedAt),
			PID:       e.PID,
			Profile:   profile,
		})
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// profileFromSessionJSONL reads up to the first 50 lines of the session's
// .jsonl file looking for a {"type":"custom-title", ...} event — that's where
// the --name <qualified-id> we set on launch lands. Matches the customTitle
// against known profile ids and returns the qualified id, or "" if no match.
func profileFromSessionJSONL(sessionID, cwd string, locs []ProfileLocation) string {
	path := filepath.Join(encodedSessionsDir(cwd), sessionID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<24)
	for i := 0; i < 50 && scanner.Scan(); i++ {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte("custom-title")) {
			continue
		}
		var ev struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type != "custom-title" {
			continue
		}
		for _, loc := range locs {
			if loc.QualifiedID == ev.CustomTitle {
				return ev.CustomTitle
			}
		}
	}
	return ""
}

// rosterSessionIDs returns the set of session ids currently in the supervisor
// roster — used by doctor to prune stale session→profile ledger entries.
func rosterSessionIDs() map[string]bool {
	data, err := os.ReadFile(rosterPath())
	if err != nil {
		return nil
	}
	var rf rosterFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range rf.Workers {
		out[e.SessionID] = true
	}
	return out
}

// profileFromFlagArgs scans launch args for --mcp-config <path> and matches
// the path against known profile locations. Returns the qualified id when
// the session was launched via claude-profiles, "" otherwise.
func profileFromFlagArgs(args []string, locs []ProfileLocation) string {
	for i, a := range args {
		if a != "--mcp-config" || i+1 >= len(args) {
			continue
		}
		path := args[i+1]
		for _, loc := range locs {
			if loc.JSONPath == path {
				return loc.QualifiedID
			}
		}
	}
	return ""
}

// backgroundedByProfile groups backgrounded sessions by their claude-profiles
// qualified id. Sessions without a recognised profile are skipped.
func backgroundedByProfile() map[string][]BackgroundedSession {
	out := map[string][]BackgroundedSession{}
	for _, bs := range loadBackgroundedSessions() {
		if bs.Profile == "" {
			continue
		}
		out[bs.Profile] = append(out[bs.Profile], bs)
	}
	return out
}
