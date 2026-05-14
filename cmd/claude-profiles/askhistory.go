package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Persistent log of past "ask" prompts. Used by the hub textinput to let
// users recall previous prompts via shell-style ↑/↓ cycling.

type AskHistoryEntry struct {
	Ts      int64  `json:"ts"`
	Text    string `json:"text"`
	Profile string `json:"profile"` // qualified profile id, or "" for "none"
}

type askHistoryFile struct {
	Prompts []AskHistoryEntry `json:"prompts"`
}

const askHistoryMax = 50

func askHistoryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "claude-profiles-asks.json")
}

func loadAskHistory() []AskHistoryEntry {
	data, err := os.ReadFile(askHistoryPath())
	if err != nil {
		return nil
	}
	var f askHistoryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return f.Prompts
}

// appendAskHistory prepends a new entry, de-duplicating prior occurrences of
// the same text (so the newest copy bubbles to the top), and caps the file at
// askHistoryMax entries. File mode 0600 because prompts can be sensitive.
func appendAskHistory(text, profile string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	entries := loadAskHistory()
	out := make([]AskHistoryEntry, 0, len(entries)+1)
	out = append(out, AskHistoryEntry{
		Ts:      time.Now().Unix(),
		Text:    text,
		Profile: profile,
	})
	for _, e := range entries {
		if e.Text == text {
			continue
		}
		out = append(out, e)
	}
	if len(out) > askHistoryMax {
		out = out[:askHistoryMax]
	}
	path := askHistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(askHistoryFile{Prompts: out}, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
