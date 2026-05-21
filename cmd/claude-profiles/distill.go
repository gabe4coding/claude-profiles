package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
//   - background_tasks non-empty: bg delegates still in flight; their commits
//     aren't landed yet, distilling now would advance the bookmark past work
//     that wasn't captured. Skip without writing the bookmark so the next
//     real commit retriggers us.
//   - session_crons non-empty: cron-triggered turn, non-interactive work,
//     distillation adds noise. Same no-bookmark-advance reasoning.
//   - no wrapper context available (running outside `claude-profiles run`).
//   - working tree shows only .claude/* changes: read-only / hook-tweak
//     session, nothing useful to distill.
func cmdHookStop() {
	if os.Getenv("DISTILL_ON_STOP") == "0" {
		return
	}
	// bg delegates are dispatched with CLAUDE_PROFILES_DELEGATE=1 in their env.
	// Distilling on their behalf is wrong: they're headless sessions whose
	// Stop is either the watcher calling `claude stop` or the user manually
	// attaching. Bail out so we don't advance the parent profile's bookmark
	// or emit a spurious decision:block inside the delegate session.
	//
	// Limitation: if a user /resume's a completed delegate from inside a
	// claude-profiles run context, the resumed process won't carry
	// CLAUDE_PROFILES_DELEGATE=1 (it was only set in the original subprocess
	// env). The wrapperContextForHook / profileID-empty guard below handles
	// most resumed cases, but if the wrapper PID is still live the parent's
	// profile could be resolved. The result.md contract is explicitly not
	// extended to resumed delegate sessions — treat /resume'd delegates as
	// read-only inspection, not as a new managed delegation.
	if os.Getenv("CLAUDE_PROFILES_DELEGATE") == "1" {
		return
	}

	var input struct {
		SessionID       string            `json:"session_id"`
		StopHookActive  bool              `json:"stop_hook_active"`
		BackgroundTasks []json.RawMessage `json:"background_tasks"`
		SessionCrons    []json.RawMessage `json:"session_crons"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return
	}
	if input.StopHookActive {
		return
	}
	if len(input.BackgroundTasks) > 0 {
		return
	}
	if len(input.SessionCrons) > 0 {
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

	if !sessionHasCommittedNonClaudeWork(startedAt) {
		return
	}

	headSHA := currentHeadSHA()
	if !commitsBetweenHaveNonClaude(lastDistillSHA(profileID), headSHA) {
		return
	}

	if err := ensureDistillProcedureFile(); err != nil {
		return
	}

	// Record the bookmark *before* emitting the block: if Claude crashes or
	// skips the distillation, we still advance past these commits next time
	// rather than re-prompting on the same work. Trading a rare missed
	// distillation for guaranteed no double-prompts is the right tradeoff.
	saveLastDistillBookmark(profileID, headSHA)

	out := map[string]any{
		"decision": "block",
		"reason":   fmt.Sprintf("Run session distillation per %s.", distillProcedurePath()),
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

// sessionHasCommittedNonClaudeWork reports whether the session produced at
// least one commit reachable from HEAD (since the wrapper started) that
// touched a file outside `.claude/`. Uncommitted-only work is deliberately
// excluded: WIP that may get reverted before commit isn't substantive enough
// to justify a distillation prompt, and once the agent does commit we'll
// catch it on the next Stop. Fail-closed on git error (returns false): no
// signal means no prompt rather than fail-open noise.
//
// sinceUnix=0 means the wrapper context wasn't available; we skip the check
// entirely (return false) — without a lower time bound we'd scan the entire
// repo history.
func sessionHasCommittedNonClaudeWork(sinceUnix int64) bool {
	if sinceUnix <= 0 {
		return false
	}
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

// currentHeadSHA returns the current `HEAD` commit SHA, or "" if unavailable
// (e.g. not in a git repo, no commits yet). "" propagates as "no bookmark
// progress" through the dedup check.
func currentHeadSHA() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// commitsBetweenHaveNonClaude reports whether any commit in the range
// `lastSHA..headSHA` (exclusive..inclusive) touched a file outside `.claude/`.
//
// Cases:
//   - lastSHA == "" → no prior bookmark; allow (return true) so first-time
//     distillation isn't blocked.
//   - lastSHA == headSHA → nothing new since last bookmark; skip.
//   - `git log` errors (lastSHA missing from history after rebase/squash, or
//     bookmark from a different worktree) → fail-open (return true) so the
//     next block emission resets the bookmark to a live SHA. Fail-closed
//     here would silently strand an orphaned bookmark and suppress every
//     future distillation until the file is deleted by hand.
func commitsBetweenHaveNonClaude(lastSHA, headSHA string) bool {
	if lastSHA == "" {
		return true
	}
	if lastSHA == headSHA {
		return false
	}
	rng := lastSHA + ".." + headSHA
	out, err := exec.Command("git", "log", rng, "--name-only", "--pretty=").Output()
	if err != nil {
		return true
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

// distillBookmark is the on-disk state recording the last HEAD SHA we
// emitted a distill-block prompt for, per profile. Read at hook entry to
// short-circuit when no new work has accumulated since.
type distillBookmark struct {
	HeadSHA  string `json:"head_sha"`
	StampUTC int64  `json:"stamp_utc"`
}

// lastDistillBookmarkPath returns the absolute path of the bookmark file for
// a given profileID. The profileID may contain `/` (alias/name form), so we
// path-encode separators to keep a flat per-profile namespace under
// ~/.claude-profiles/last-distill/.
func lastDistillBookmarkPath(profileID string) string {
	safe := strings.ReplaceAll(profileID, "/", "__")
	safe = strings.ReplaceAll(safe, string(os.PathSeparator), "__")
	return filepath.Join(profilesRoot(), "last-distill", safe+".json")
}

// lastDistillSHA returns the HEAD SHA recorded at the last distill-block
// emission for this profile, or "" if no bookmark exists or the file is
// unreadable.
func lastDistillSHA(profileID string) string {
	data, err := os.ReadFile(lastDistillBookmarkPath(profileID))
	if err != nil {
		return ""
	}
	var b distillBookmark
	if json.Unmarshal(data, &b) != nil {
		return ""
	}
	return b.HeadSHA
}

// saveLastDistillBookmark records the current HEAD SHA as the bookmark for
// this profile. Errors are swallowed — bookmark corruption only degrades the
// dedup filter to "always prompt", which is the previous behaviour.
func saveLastDistillBookmark(profileID, headSHA string) {
	if headSHA == "" {
		return
	}
	path := lastDistillBookmarkPath(profileID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(distillBookmark{HeadSHA: headSHA, StampUTC: time.Now().Unix()})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
