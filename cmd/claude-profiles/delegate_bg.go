package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// /delegate path (Atto III): dispatch via `claude --bg` and rely on Claude
// Code's Agent View supervisor to host the session. A small watcher
// subprocess polls the session's state file under ~/.claude/jobs/<id>/state.json,
// and when the first turn completes it calls `claude stop <bg-id>` to free
// the slot. The parent's UserPromptSubmit hook reads state.json directly
// (no result.md round-trip) — the hook records a delivered.txt marker after
// injection so subsequent prompts don't re-fire the same reply.
//
// Lifecycle:
//   1. Slash command runs delegate-launch.sh, which writes request.json
//      and invokes `claude-profiles _delegate-bg-dispatch <id>`.
//   2. _delegate-bg-dispatch reads the request, builds the `claude --bg`
//      command from the profile flags, runs it, captures the short bg
//      session id from `backgrounded · <id>`, writes bg-session-id.txt,
//      then spawns `_delegate-bg-watcher <id>` as a detached process and
//      exits. Failures before bg-session-id.txt is written leave
//      dispatch-error.md behind so the parent hook can deliver the error.
//   3. _delegate-bg-watcher polls ~/.claude/jobs/<bg-id>/state.json.
//      When state reaches a "first turn done" condition it calls
//      `claude stop <bg-id>` to free the Agent View slot — that's its
//      only job; the parent hook handles result delivery. On timeout it
//      stops the session and writes dispatch-error.md.
//   4. On the parent's next UserPromptSubmit, cmdHookPromptSubmit walks
//      the delegates dir, reads each bg session's state.json directly,
//      extracts the assistant reply from linkScanPath, and injects it
//      as additionalContext. A delivered.txt marker is written so the
//      same reply isn't re-injected on subsequent prompts.

const (
	bgWatcherPollInterval = 2 * time.Second
	bgWatcherAbandonAfter = 30 * time.Minute
)

// bgJobState mirrors the subset of ~/.claude/jobs/<id>/state.json this
// codebase reads. Field names match Claude Code's serialization; extra
// fields are ignored.
type bgJobState struct {
	State        string `json:"state"`
	LinkScanPath string `json:"linkScanPath"`
	SessionID    string `json:"sessionId"`
	DaemonShort  string `json:"daemonShort"`
	Name         string `json:"name"`
	Intent       string `json:"intent"`
	Detail       string `json:"detail"`
}

// claudeJobsDir returns ~/.claude/jobs (or the supervisor's equivalent
// under CLAUDE_CONFIG_DIR when set). Mirrors the path documented at
// https://code.claude.com/docs/en/agent-view#where-state-is-stored.
func claudeJobsDir() string {
	if root := os.Getenv("CLAUDE_CONFIG_DIR"); root != "" {
		return filepath.Join(root, "jobs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "jobs")
}

// writeDispatchError records a dispatch-time failure for the parent hook to
// pick up. Used when the dispatcher can't even reach `claude --bg` (profile
// missing, disabled, binary not on PATH, etc.) or when the watcher gives up
// (timeout). The parent's UserPromptSubmit hook reads this file and renames
// it to delivered-error.md after injection so the same error isn't repeated
// on every subsequent prompt.
//
// Plain-text body, no trailer: the Atto II "delegate cleanup reminder" is
// no longer applicable (there is no tmux window to close and no Monitor
// watcher to release).
func writeDispatchError(dir, body string) {
	_ = os.WriteFile(filepath.Join(dir, "dispatch-error.md"), []byte(body), 0o644)
}

// cmdDelegateBgDispatch is invoked by delegate-launch.sh in --bg mode:
// `claude-profiles _delegate-bg-dispatch <delegate-id>`. Synchronous:
// reads request.json, builds the claude --bg command, runs it, captures
// the short id, writes bg-session-id.txt, spawns the watcher, and prints
// DELEGATE_BG_ID + DELEGATE_BG_NAME to stdout so the launch script can
// pass them up to the slash command.
func cmdDelegateBgDispatch(args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: claude-profiles _delegate-bg-dispatch <delegate-id>"))
	}
	delegateID := args[0]

	dir, req := findDelegateRequest(delegateID)
	if dir == "" {
		fatal(fmt.Errorf("delegate %s: no request.json found under %s", delegateID, delegatesDir()))
	}

	loc, err := resolveProfileLocation(req.Profile)
	if err != nil {
		writeDispatchError(dir, fmt.Sprintf("(delegate %s failed: profile %q does not resolve: %v)", delegateID, req.Profile, err))
		fatal(err)
	}
	prefs := loadProfilePrefs(filepath.Dir(loc.JSONPath))
	if prefs.Disabled {
		msg := fmt.Sprintf("(delegate %s failed: profile %q is disabled)", delegateID, req.Profile)
		writeDispatchError(dir, msg)
		fatal(fmt.Errorf("profile %q is disabled", req.Profile))
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		writeDispatchError(dir, fmt.Sprintf("(delegate %s failed to load profile: %v)", delegateID, err))
		fatal(err)
	}

	// Atto III: `OwnerRepo` is no longer enforced at dispatch time — it's
	// kept as a hub-filter hint only (see listAllLocations). Dispatching a
	// worktree-bound profile against the "wrong" repo now silently runs
	// the delegate there; the hub still groups profiles correctly so the
	// UX hint survives. Tradeoff: we trade a guard against a foot-gun for
	// a simpler dispatcher and lower coupling. If this regresses for real
	// users the binding can come back as a runtime check on req.Dir
	// without re-introducing the tmux path.

	// Settings: write the profile's inline _settings beside the request,
	// so --settings takes the same path-style argument as the wrapper. The
	// file lives in the delegate dir (not os.TempDir) so it gets cleaned
	// up alongside request.json / bg-session-id.txt when the user reaps
	// the delegate, instead of leaking into /tmp on every dispatch. Stop
	// hooks (e.g. distill) propagate through this file into the bg
	// session — failing to write means the delegate would silently lose
	// its hooks, so we fail dispatch loudly instead.
	settingsPath := ""
	if len(p.Settings) > 0 {
		settingsPath = filepath.Join(dir, "settings.json")
		if err := os.WriteFile(settingsPath, p.Settings, 0o644); err != nil {
			body := fmt.Sprintf("(delegate %s failed to write settings.json: %v)", delegateID, err)
			writeDispatchError(dir, body)
			fatal(fmt.Errorf("write %s: %v", settingsPath, err))
		}
	}

	binary, err := exec.LookPath("claude")
	if err != nil {
		writeDispatchError(dir, fmt.Sprintf("(delegate %s failed: claude binary not on PATH)", delegateID))
		fatal(err)
	}

	// Build the claude --bg invocation. Mirrors cmdDelegateRunner's
	// composition, with these differences:
	//   - prepend --bg
	//   - never pass --worktree: bg sessions auto-create their own worktree
	//     under .claude/worktrees/, with cleanup tied to `claude rm`
	//   - --name uses "<profile>:<truncated-task>" so Agent View rows are
	//     identifiable across profiles instead of all-named "delegate-xxx"
	//   - --add-dir grants file access to the parent's cwd, so the bg
	//     session (which starts in its own auto-worktree) can still
	//     reference the orchestrator's directory if needed
	claudeArgs := []string{"--bg"}
	if loc.Builtin == "" {
		if mcp := resolveMCPConfigPath(*loc); mcp != "" {
			claudeArgs = append(claudeArgs, "--strict-mcp-config", "--mcp-config", mcp)
		}
	}

	// Suppress the auto --worktree from claudeFlags (bg handles worktrees
	// itself) and strip inline settings since we passed settingsPath.
	pcopy := *p
	pcopy.Settings = nil
	pcopy.Worktree = false
	claudeArgs = append(claudeArgs, claudeFlags(&pcopy, settingsPath)...)

	claudeArgs = append(claudeArgs, "--plugin-dir", wrapperPluginPath())
	if pdir := pluginDirFor(*loc); pdir != "" {
		claudeArgs = append(claudeArgs, "--plugin-dir", pdir)
	}

	if req.Dir != "" {
		claudeArgs = append(claudeArgs, "--add-dir", req.Dir)
	}

	// Same permission-mode rule as the tmux path: Haiku gets
	// --dangerously-skip-permissions (no auto classifier), everything else
	// gets --permission-mode auto. The bg supervisor enforces the same
	// one-time interactive acceptance for bypassPermissions; if it hasn't
	// been done, dispatch fails fast with a clear error.
	if strings.Contains(strings.ToLower(getModel(parseSettings(p.Settings))), "haiku") {
		claudeArgs = append(claudeArgs, "--dangerously-skip-permissions")
	} else {
		claudeArgs = append(claudeArgs, "--permission-mode", "auto")
	}

	if err := validateGoalName(req.Goal); err != nil {
		body := fmt.Sprintf("(delegate %s rejected: %v)", delegateID, err)
		writeDispatchError(dir, body)
		fatal(err)
	}
	displayName := bgDisplayName(req.Profile, req.Task, req.Goal)
	claudeArgs = append(claudeArgs, "--name", displayName)

	// Minimum Claude Code version for --bg dispatch: v2.1.139 (--bg introduced).
	// Slash-command / skill tasks (req.Task starting with "/") require v2.1.146+:
	// earlier versions refused to launch a bg session whose only input was a
	// skill invocation and returned a launch error that looked like a generic
	// dispatch failure. Profile Tasks starting with "/" are valid since v2.1.146.
	if req.Task != "" {
		claudeArgs = append(claudeArgs, req.Task)
	}

	cmd := exec.Command(binary, claudeArgs...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	cmd.Env = append(os.Environ(), "CLAUDE_PROFILES_DELEGATE=1")
	// SubagentModel (issue #18) pins the model used by any subagents the bg
	// delegate itself spawns. Read from the resolved Profile (which already
	// merges profile.json + prefs via loadProfileAt) so the value can be set
	// either in profile.json _subagent_model OR in prefs — the edit TUI writes
	// through prefs but power users can pin it directly in the profile file.
	// CLAUDE_CODE_SUBAGENT_MODEL reaches direct child processes since v2.1.146;
	// it extends to agent-team teammate processes since v2.1.147. On older
	// versions this is a no-op. Append AFTER
	// CLAUDE_PROFILES_DELEGATE so the distill guard ordering invariant in
	// CLAUDE.md is preserved.
	if p.SubagentModel != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_SUBAGENT_MODEL="+p.SubagentModel)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		body := fmt.Sprintf("(delegate %s failed to dispatch via `claude --bg`: %v\n\nOutput:\n%s)", delegateID, err, string(out))
		writeDispatchError(dir, body)
		fatal(fmt.Errorf("claude --bg dispatch failed: %v", err))
	}

	bgID := parseBackgroundedID(string(out))
	if bgID == "" {
		body := fmt.Sprintf("(delegate %s failed: could not parse bg session id from claude --bg output:\n\n%s)", delegateID, string(out))
		writeDispatchError(dir, body)
		fatal(fmt.Errorf("could not parse bg id from: %s", out))
	}

	if err := os.WriteFile(filepath.Join(dir, "bg-session-id.txt"), []byte(bgID), 0o644); err != nil {
		fatal(fmt.Errorf("write bg-session-id.txt: %v", err))
	}
	// Resumability note (v2.1.144+): Claude Code's /resume picker now
	// surfaces bg sessions alongside interactive ones. If a user /resume's
	// this delegate after it completes, the resumed session is entirely
	// unobserved by this codebase: the watcher has exited, the parent has
	// already marked the delegate as delivered (delivered.txt), and the
	// hook won't re-inject. /resume on a completed delegate is therefore
	// unsupported — treat it as manual inspection only, with no
	// expectation of result delivery to the parent.

	if err := spawnBgWatcher(delegateID); err != nil {
		// Watcher failed to start — record a dispatch error so the parent
		// at least knows the dispatch happened but lifecycle is broken.
		body := fmt.Sprintf("(delegate %s dispatched as bg session %s but the watcher failed to spawn: %v\n\nRun `claude attach %s` to interact with the delegate manually.)",
			delegateID, bgID, err, bgID)
		writeDispatchError(dir, body)
		fatal(err)
	}

	fmt.Printf("DELEGATE_BG_ID=%s\n", bgID)
	fmt.Printf("DELEGATE_BG_NAME=%s\n", displayName)
}

// cmdDelegateBgWatcher polls ~/.claude/jobs/<bg-id>/state.json until the
// bg session reaches a terminal state, then calls `claude stop <bg-id>` to
// free the Agent View slot. Atto III: the watcher no longer writes any
// result file — the parent UserPromptSubmit hook reads state.json directly
// (via the bg-session-id.txt the dispatcher already wrote). On timeout we
// stop the session and leave dispatch-error.md behind so the parent gets a
// message instead of an indefinite stall. Designed to be run detached from
// the launching process.
func cmdDelegateBgWatcher(args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: claude-profiles _delegate-bg-watcher <delegate-id>"))
	}
	delegateID := args[0]

	dir, _ := findDelegateRequest(delegateID)
	if dir == "" {
		fatal(fmt.Errorf("delegate %s: no request.json found", delegateID))
	}

	logFile, _ := os.OpenFile(filepath.Join(dir, "bg-watcher.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logFile != nil {
		defer logFile.Close()
	}
	logf := func(format string, args ...any) {
		if logFile == nil {
			return
		}
		fmt.Fprintf(logFile, "[%s] ", time.Now().Format(time.RFC3339))
		fmt.Fprintf(logFile, format, args...)
		fmt.Fprintln(logFile)
	}

	idBytes, err := os.ReadFile(filepath.Join(dir, "bg-session-id.txt"))
	if err != nil {
		logf("read bg-session-id.txt: %v", err)
		writeDispatchError(dir, fmt.Sprintf("(delegate %s: bg watcher could not read bg-session-id.txt: %v)", delegateID, err))
		fatal(err)
	}
	bgID := strings.TrimSpace(string(idBytes))
	if bgID == "" {
		logf("empty bg session id")
		writeDispatchError(dir, fmt.Sprintf("(delegate %s: bg watcher found empty bg-session-id.txt)", delegateID))
		return
	}

	statePath := filepath.Join(claudeJobsDir(), bgID, "state.json")
	deadline := time.Now().Add(bgWatcherAbandonAfter)

	logf("watching state %s for delegate %s (bg %s)", statePath, delegateID, bgID)

	for time.Now().Before(deadline) {
		// If the parent hook has already delivered, exit early — no point
		// keeping the slot warm. Both markers (success / error) signal a
		// terminal hook interaction. Skip `claude stop` when the supervisor
		// already knows the session is stopped: state was the hook's signal
		// to deliver, and if it reads "stopped" we'd just be spawning a
		// subprocess for a no-op.
		hookDeliveredSuccess := false
		hookDeliveredError := false
		if _, err := os.Stat(filepath.Join(dir, "delivered.txt")); err == nil {
			hookDeliveredSuccess = true
		} else if _, err := os.Stat(filepath.Join(dir, "delivered-error.md")); err == nil {
			hookDeliveredError = true
		}
		if hookDeliveredSuccess || hookDeliveredError {
			marker := "delivered.txt"
			if hookDeliveredError {
				marker = "delivered-error.md"
			}
			if cur, err := readBgJobState(statePath); err == nil && cur.State == "stopped" {
				logf("%s exists and session already stopped, exiting", marker)
				return
			}
			logf("%s exists, stopping and exiting", marker)
			stopBgSession(bgID, logf)
			return
		}

		state, err := readBgJobState(statePath)
		if err != nil {
			logf("read state: %v", err)
			time.Sleep(bgWatcherPollInterval)
			continue
		}

		if isBgFirstTurnDone(state) {
			logf("first turn done (state=%s, linkScanPath=%s) — stopping bg session; parent hook will pick up the result", state.State, state.LinkScanPath)
			stopBgSession(bgID, logf)
			return
		}

		time.Sleep(bgWatcherPollInterval)
	}

	// Timeout. If we got here, the hook never delivered (otherwise the
	// early-exit branch at the top of the loop would have caught
	// delivered.txt / delivered-error.md and bailed). Stop the session
	// and record a dispatch error so the parent gets *something* on its
	// next prompt instead of an indefinite stall.
	logf("timeout after %s — stopping bg session %s", bgWatcherAbandonAfter, bgID)
	stopBgSession(bgID, logf)
	writeDispatchError(dir, fmt.Sprintf("(delegate %s abandoned by bg watcher after %s — session %s stopped; attach with `claude attach %s` if it might still be useful)",
		delegateID, bgWatcherAbandonAfter, bgID, bgID))
}

// cmdDelegateLinkScanPath prints the absolute path of the running bg
// delegate's session JSONL (state.linkScanPath) so callers can compose
// `tail -F | jq --unbuffered | Monitor` pipelines without reaching into the
// supervisor's state file themselves. Pure read: never writes inside the
// delegate dir.
//
// Two waits in series:
//
//  1. `bg-session-id.txt` may not exist yet (dispatcher writes it after
//     `claude --bg` returns the bg id).
//  2. `state.linkScanPath` may be empty even once the file exists (supervisor
//     materialises the JSONL ~5s after dispatch).
//
// Total budget is bgLinkScanPathBudget (30s). When the dispatcher already
// wrote dispatch-error.md (no bg-session-id.txt to point at) we exit
// non-zero immediately so callers can distinguish "still warming up" from
// "dispatch failed". This subcommand is documented in the slash-command
// markdown under "Live progress (advanced)" — most delegates don't need it,
// the parent hook still delivers results without any caller involvement.
func cmdDelegateLinkScanPath(args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: claude-profiles _delegate-jsonl <delegate-id>"))
	}
	delegateID := args[0]

	dir, _ := findDelegateRequest(delegateID)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "delegate %s: no request.json found under %s\n", delegateID, delegatesDir())
		os.Exit(1)
	}

	deadline := time.Now().Add(bgLinkScanPathBudget)
	bgIDPath := filepath.Join(dir, "bg-session-id.txt")

	for time.Now().Before(deadline) {
		// Fail fast on a real dispatch failure: the dispatcher writes
		// dispatch-error.md when it bails before bg-session-id.txt, so its
		// presence is a definitive "no bg session will ever exist for this
		// delegate" — polling further is wasted budget.
		if _, err := os.Stat(filepath.Join(dir, "dispatch-error.md")); err == nil {
			fmt.Fprintf(os.Stderr, "delegate %s: dispatch failed (see %s/dispatch-error.md)\n",
				delegateID, dir)
			os.Exit(1)
		}

		idBytes, err := os.ReadFile(bgIDPath)
		if err != nil {
			time.Sleep(bgLinkScanPathPoll)
			continue
		}
		bgID := strings.TrimSpace(string(idBytes))
		if bgID == "" {
			time.Sleep(bgLinkScanPathPoll)
			continue
		}

		statePath := filepath.Join(claudeJobsDir(), bgID, "state.json")
		state, err := readBgJobState(statePath)
		if err == nil && state.LinkScanPath != "" {
			fmt.Println(state.LinkScanPath)
			return
		}
		time.Sleep(bgLinkScanPathPoll)
	}

	fmt.Fprintf(os.Stderr, "delegate %s: timed out after %s waiting for linkScanPath in state.json\n",
		delegateID, bgLinkScanPathBudget)
	os.Exit(1)
}

// Polling cadence for cmdDelegateLinkScanPath. The total budget covers two
// races: the dispatcher writing bg-session-id.txt, and the supervisor
// materialising the JSONL (state.linkScanPath becoming non-empty). 30s is
// roomy for both even on a cold supervisor — the worst real-world latency
// measured in Atto III testing was ~6s end-to-end. Polling at 250ms keeps
// the cache-miss cost low for callers that hit the helper early.
const (
	bgLinkScanPathBudget = 30 * time.Second
	bgLinkScanPathPoll   = 250 * time.Millisecond
)

// readBgJobState reads and parses the supervisor's state file for a bg
// session. Returns a zero value (not an error) when the file does not
// exist yet — the supervisor may take a beat after `claude --bg` returns
// before it materialises the directory.
func readBgJobState(path string) (bgJobState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bgJobState{}, nil
		}
		return bgJobState{}, err
	}
	var s bgJobState
	if err := json.Unmarshal(data, &s); err != nil {
		return bgJobState{}, err
	}
	return s, nil
}

// isBgFirstTurnDone returns true when the bg session has produced enough
// for us to treat the delegate's reply as ready. The supervisor's state
// machine transitions to "blocked" after the first turn ends (waiting for
// the next user prompt — which a delegate doesn't get), to "completed"
// when /stop runs, to "failed" on error, to "stopped" when
// `claude stop <id>` runs, and to "done" on Claude Code 2.1.145+ when
// a bg job reaches a terminal state with a successful result. Any of
// these means we can extract.
func isBgFirstTurnDone(s bgJobState) bool {
	switch s.State {
	case "blocked", "completed", "failed", "stopped", "done":
		return s.LinkScanPath != "" || s.State != "blocked"
	}
	return false
}

// stopBgSession runs `claude stop <id>` best-effort. Errors are logged but
// not fatal — the watcher's job is freeing the Agent View slot, not
// enforcing session lifecycle.
func stopBgSession(bgID string, logf func(string, ...any)) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		logf("claude not on PATH for stop: %v", err)
		return
	}
	out, err := exec.Command(bin, "stop", bgID).CombinedOutput()
	if err != nil {
		logf("claude stop %s failed: %v\n%s", bgID, err, string(out))
		return
	}
	logf("claude stop %s ok", bgID)
}

// parseBackgroundedID extracts the short id from `claude --bg` output.
// The CLI prints `backgrounded · <id>` as the first line; we tolerate
// whitespace and surrounding lines.
func parseBackgroundedID(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "backgrounded"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		// Drop the middle-dot separator (U+00B7) the CLI prints between
		// "backgrounded" and the short id, plus its ASCII fallbacks.
		rest = strings.TrimLeft(rest, "·- \t")
		// First whitespace token is the id.
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// Display-name grammar for bg sessions. The writer (bgDisplayName) and the
// parser (parseGoalFromName, used by `claude-profiles goal`) must agree on
// this. The format is a roundtrip contract; do not change without updating
// both sides plus the unit test in delegate_bg_test.go.
//
//	<no goal>:   "<profile>: <task>"
//	<with goal>: "goal:<name> | <profile>: <task>"
//
// goalDelim is the literal separator between the goal segment and the rest.
// goalPrefix marks the goal segment so a profile or task literally containing
// " | " is not mistaken for a goal.
const (
	goalPrefix = "goal:"
	goalDelim  = " | "
)

// bgDisplayName produces a stable, human-readable name for the bg
// session. Agent View rows are sorted by this; keeping the profile
// first makes cross-profile dispatches easy to read at a glance.
//
// When goal is non-empty, the row is prefixed with "goal:<name> | " so
// `claude-profiles goal list` can group sessions by goal.
//
// Truncation is rune-aware (not byte-based): tasks in non-ASCII text
// must not be split mid-rune, otherwise Agent View renders replacement
// glyphs in the row name.
func bgDisplayName(profile, task, goal string) string {
	const maxTaskRunes = 50
	t := strings.TrimSpace(task)
	// Strip newlines so the name renders as a single row entry.
	t = strings.ReplaceAll(t, "\n", " ")
	t = strings.ReplaceAll(t, "\r", " ")
	if runes := []rune(t); len(runes) > maxTaskRunes {
		t = string(runes[:maxTaskRunes]) + "…"
	}
	base := profile + ": " + t
	if g := strings.TrimSpace(goal); g != "" {
		return goalPrefix + g + goalDelim + base
	}
	return base
}

// parseGoalFromName extracts the goal name from a bg session's display name.
// Returns "" when the name has no goal prefix or the prefix is malformed.
// Mirror of bgDisplayName's prefix emission — see the format contract above.
//
// Defence in depth: the parsed candidate must itself satisfy validateGoalName.
// Profile names are not constrained to exclude "goal:" or " | ", so without
// this check a profile literally named "goal:foo" combined with a task that
// contains " | " (e.g. "goal:foo: oh | wow") would register as a false-positive
// goal-tagged session. Since validateGoalName rejects ':', '|', and whitespace,
// any candidate containing them cannot have been produced by bgDisplayName.
func parseGoalFromName(name string) string {
	if !strings.HasPrefix(name, goalPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, goalPrefix)
	idx := strings.Index(rest, goalDelim)
	if idx <= 0 {
		return ""
	}
	goal := rest[:idx]
	if validateGoalName(goal) != nil {
		return ""
	}
	return goal
}

// validateGoalName enforces the constraints bgDisplayName / parseGoalFromName
// rely on: a goal name cannot contain the delimiter, the prefix-colon, or any
// whitespace at its edges. Returns nil for the empty string (callers treat
// empty as "no goal") and an error otherwise. Called at flag-parse time so
// users see the failure before any dispatch happens.
func validateGoalName(name string) error {
	if name == "" {
		return nil
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("goal name has leading or trailing whitespace: %q", name)
	}
	if strings.ContainsAny(name, "|: \t\r\n") {
		return fmt.Errorf("goal name contains a reserved character ('|', ':', or whitespace): %q", name)
	}
	return nil
}

// spawnBgWatcher starts `claude-profiles _delegate-bg-watcher <id>` as a
// detached process. The launching process exits without waiting, leaving
// the watcher to outlive it (init adopts it). All stdio is closed so the
// watcher doesn't hold the launch script's terminal open.
func spawnBgWatcher(delegateID string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %v", err)
	}
	cmd := exec.Command(self, "_delegate-bg-watcher", delegateID)
	// Replace stdio with /dev/null so the watcher detaches cleanly from
	// the launch script's terminal. The watcher writes its progress to
	// <delegate-dir>/bg-watcher.log instead of stdout/stderr.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %v", err)
	}
	defer devnull.Close()
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watcher: %v", err)
	}
	// Don't Wait — let the watcher run until the bg session reaches a
	// terminal state (and `claude stop` runs) or the timeout fires. The
	// process is reparented to init when we exit.
	return nil
}

// cmdOpenAgentView is wired to the hub's "open Agent View" palette action.
// It launches `claude agents --cwd <mainRepoRoot>` so the user sees every
// bg delegate dispatched against this repo. Deliberately passes no
// --settings / --plugin-dir / --model so the view is profile-agnostic and
// each delegate appears with its own per-dispatch flags.
func cmdOpenAgentView(repoRoot string) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		info("claude binary not on PATH — cannot open Agent View. Install Claude Code (v2.1.139+).")
		return
	}
	args := []string{"agents"}
	if repoRoot != "" {
		args = append(args, "--cwd", repoRoot)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
