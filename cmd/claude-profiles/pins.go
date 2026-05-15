package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Persistent ordered list of "starred" profiles for quick-access from the hub.
// Pinned profiles float to the top of the hub list (preserving pin order among
// themselves) and can optionally carry a pre-selected prompt name so the
// session starts without a picker.

type PinEntry struct {
	ProfileID  string `json:"profileID"`
	PromptName string `json:"promptName,omitempty"` // named prompt to auto-select; "" = show picker
}

type pinsFile struct {
	Pins []PinEntry `json:"pins"`
}

func loadPins() []PinEntry {
	data, err := os.ReadFile(pinsPath())
	if err != nil {
		return nil
	}
	var f pinsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return f.Pins
}

func savePins(pins []PinEntry) error {
	path := pinsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(pinsFile{Pins: pins}, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func pinIdx(pins []PinEntry, profileID string) int {
	for i, p := range pins {
		if p.ProfileID == profileID {
			return i
		}
	}
	return -1
}

func findPin(pins []PinEntry, profileID string) (PinEntry, bool) {
	i := pinIdx(pins, profileID)
	if i < 0 {
		return PinEntry{}, false
	}
	return pins[i], true
}

func addPin(pins []PinEntry, entry PinEntry) []PinEntry {
	if i := pinIdx(pins, entry.ProfileID); i >= 0 {
		pins[i] = entry
		return pins
	}
	return append(pins, entry)
}

func removePin(pins []PinEntry, profileID string) []PinEntry {
	i := pinIdx(pins, profileID)
	if i < 0 {
		return pins
	}
	out := make([]PinEntry, 0, len(pins)-1)
	out = append(out, pins[:i]...)
	return append(out, pins[i+1:]...)
}
