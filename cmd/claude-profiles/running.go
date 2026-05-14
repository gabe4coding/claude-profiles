package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Per-wrapper pidfile tracking — written at cmdRun startup, updated on each
// loop iteration with the active profile + most-recently-known session id,
// removed on clean exit. The hub reads these to indicate "running" profiles
// and to offer "attach to existing session" instead of starting a duplicate.

type RunningWrapper struct {
	PID       int    `json:"pid"`
	Profile   string `json:"profile"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"session_id,omitempty"`
	StartedAt int64  `json:"started_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func runningDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "claude-profiles-run")
}

func pidfilePath(pid int) string {
	return filepath.Join(runningDir(), fmt.Sprintf("%d.json", pid))
}

func writeRunningPidfile(w *RunningWrapper) error {
	if err := os.MkdirAll(runningDir(), 0o755); err != nil {
		return err
	}
	w.UpdatedAt = time.Now().Unix()
	data, _ := json.MarshalIndent(w, "", "  ")
	return os.WriteFile(pidfilePath(w.PID), append(data, '\n'), 0o600)
}

func removeRunningPidfile(pid int) {
	_ = os.Remove(pidfilePath(pid))
}

// loadRunningWrappers returns every live wrapper. Pidfiles for processes that
// no longer exist are deleted as a side effect so the directory self-cleans.
func loadRunningWrappers() []RunningWrapper {
	entries, err := os.ReadDir(runningDir())
	if err != nil {
		return nil
	}
	var out []RunningWrapper
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(runningDir(), e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var w RunningWrapper
		if err := json.Unmarshal(data, &w); err != nil {
			continue
		}
		if !processAlive(w.PID) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, w)
	}
	return out
}

func runningByProfile() map[string][]RunningWrapper {
	out := map[string][]RunningWrapper{}
	for _, w := range loadRunningWrappers() {
		out[w.Profile] = append(out[w.Profile], w)
	}
	return out
}

// processAlive checks whether `pid` corresponds to a process that exists and
// is reachable by signal 0 (POSIX standard liveness probe).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// countStalePidfiles is used by doctor — counts files whose pid is dead. Does
// NOT delete (delete is a side effect of loadRunningWrappers, which runs as
// part of every hub render).
func countStalePidfiles() int {
	entries, err := os.ReadDir(runningDir())
	if err != nil {
		return 0
	}
	stale := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(runningDir(), e.Name()))
		if err != nil {
			continue
		}
		var w RunningWrapper
		if err := json.Unmarshal(data, &w); err != nil {
			continue
		}
		if !processAlive(w.PID) {
			stale++
		}
	}
	return stale
}
