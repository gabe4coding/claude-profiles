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
//   - no wrapper context available (running outside `claude-profiles run`).
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

	profileID, startedAt := wrapperContextForHook(input.SessionID)
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

	if !sessionHasNonClaudeWork(startedAt) {
		return
	}

	procedure := distillProcedureDefault
	if custom, err := os.ReadFile(distillProcedurePath()); err == nil && len(custom) > 0 {
		procedure = string(custom)
	}

	out := map[string]any{
		"decision": "block",
		"reason":   procedure,
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

// wrapperContextForHook resolves the active profile id and wrapper start time
// for a hook firing inside `claude-profiles run`. The wrapper exports
// CLAUDE_PROFILES_WRAPPER_PID so its pidfile is the authoritative live source
// — more reliable than the session→profile ledger, which depends on
// pollForSessionID having seen the jsonl in time. Falls back to the ledger
// for hand-tested or detached cases (no started_at available then).
// Backfills the ledger when a mapping is recovered from the pidfile so the hub
// and analytics keep working after the wrapper exits.
func wrapperContextForHook(sessionID string) (profileID string, startedAt int64) {
	if pid := os.Getenv("CLAUDE_PROFILES_WRAPPER_PID"); pid != "" {
		if data, err := os.ReadFile(filepath.Join(runDirPath(), pid+".json")); err == nil {
			var w RunningWrapper
			if json.Unmarshal(data, &w) == nil && w.Profile != "" {
				if sessionID != "" {
					recordSessionProfile(sessionID, w.Profile)
				}
				return w.Profile, w.StartedAt
			}
		}
	}
	return loadSessionProfiles()[sessionID], 0
}

// sessionHasNonClaudeWork reports whether the session has touched any file
// outside `.claude/` — checking both uncommitted (`git status --porcelain`)
// and committed (`git log --since=<wrapper-start>`) changes. The committed
// path is required because once the agent commits, status goes clean and the
// uncommitted-only filter would silently skip distillation for substantive
// sessions. sinceUnix=0 means the wrapper context wasn't available — the log
// check is skipped and we fall back to the uncommitted view only.
func sessionHasNonClaudeWork(sinceUnix int64) bool {
	if uncommittedHasNonClaude() {
		return true
	}
	if sinceUnix > 0 && committedHasNonClaudeSince(sinceUnix) {
		return true
	}
	return false
}

// uncommittedHasNonClaude reports whether `git status --porcelain` shows any
// modified/untracked path outside `.claude/`. Fail-open: when git is
// unavailable or the cwd isn't a repo, return true so distillation isn't
// silently lost in environments where the filter has no signal.
func uncommittedHasNonClaude() bool {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		// Rename: "R  old -> new" — only the destination matters.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if !strings.HasPrefix(path, ".claude/") {
			return true
		}
	}
	return false
}

// committedHasNonClaudeSince reports whether any commit reachable from HEAD
// with a commit date at or after sinceUnix touched a file outside `.claude/`.
// Fail-closed on git error (returns false) — the uncommitted check is the
// primary signal and has its own fail-open behaviour.
func committedHasNonClaudeSince(sinceUnix int64) bool {
	out, err := exec.Command("git", "log",
		fmt.Sprintf("--since=@%d", sinceUnix),
		"--name-only", "--pretty=", "HEAD").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ".claude/") {
			return true
		}
	}
	return false
}
