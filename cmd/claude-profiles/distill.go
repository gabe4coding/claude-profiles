package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// distillProcedureDefault is the canonical procedure text shipped with this
// binary. Lazy-written to distillProcedurePath() on first hook fire so users
// can edit their copy without us clobbering it on upgrade.
//
//go:embed distill.md
var distillProcedureDefault string

// distillProcedurePath is the absolute path of the on-disk distillation
// procedure file. Lives under profilesRoot() so it travels with the rest of
// our state. Absolute on purpose: Claude Code's Read tool requires absolute
// paths, and the hook's `reason` field renders this path verbatim.
func distillProcedurePath() string {
	return filepath.Join(profilesRoot(), "distill.md")
}

// ensureDistillProcedureFile writes the embedded default procedure to
// distillProcedurePath() ONLY when the file is absent. Idempotent —
// user edits survive `claude-profiles upgrade` without merge conflicts.
func ensureDistillProcedureFile() error {
	path := distillProcedurePath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(distillProcedureDefault), 0o644)
}

// cmdHookStop is the Stop-hook handler invoked as `claude-profiles _hook-stop`
// by the wrapper's injected settings. Reads Claude Code's hook JSON from
// stdin and emits either nothing (silent skip) or a `{decision:"block",
// reason:...}` JSON that hands Claude an instruction to run the distillation
// procedure before stopping.
//
// On/off precedence (highest first):
//  1. DISTILL_ON_STOP=0 env var — ad-hoc force off
//  2. ProfilePrefs.Distill — user-side override of the profile setting
//  3. Profile._distill — profile-level default
//
// Other silent early-exits:
//   - stop_hook_active=true: Claude has already done the work; let it stop.
//   - session_id not yet recorded in sessions.json: race window at first
//     turn, fail safe (no double-prompts later).
//   - working tree shows only .claude/* changes: read-only / hook-tweak
//     session, nothing useful to distill.
func cmdHookStop() {
	if os.Getenv("DISTILL_ON_STOP") == "0" {
		return
	}

	var input struct {
		SessionID      string `json:"session_id"`
		StopHookActive bool   `json:"stop_hook_active"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return
	}
	if input.StopHookActive {
		return
	}

	profileID := loadSessionProfiles()[input.SessionID]
	if profileID == "" {
		return
	}
	loc, err := resolveProfileLocation(profileID)
	if err != nil {
		return
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		return
	}
	// loadProfileAt has already merged ProfilePrefs.Distill over Profile._distill,
	// so p.Distill is the effective value.
	if strings.ToLower(p.Distill) != "on" {
		return
	}

	if !workingTreeHasNonClaudeChanges() {
		return
	}

	if err := ensureDistillProcedureFile(); err != nil {
		return
	}

	out := map[string]any{
		"decision": "block",
		"reason":   fmt.Sprintf("Run session distillation per %s.", distillProcedurePath()),
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

// workingTreeHasNonClaudeChanges reports whether `git status --porcelain` shows
// any modified/untracked path outside `.claude/`. Fail-open: when git is
// unavailable or the cwd isn't a repo, return true so we don't silently lose
// distillation in environments where the filter has no signal.
func workingTreeHasNonClaudeChanges() bool {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		// Rename: "R  old -> new" — check the destination.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if !strings.HasPrefix(path, ".claude/") {
			return true
		}
	}
	return false
}
