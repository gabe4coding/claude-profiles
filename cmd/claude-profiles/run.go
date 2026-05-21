package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ── In-session profile switching via wrapper loop ─────────────────────────────
//
// `claude-profiles run <profile>` launches claude with the profile, waits for
// exit, then checks for a "next profile" marker file. If the marker is set,
// it relaunches claude with the new profile and --resume on the same session,
// so the conversation continues seamlessly.
//
// The marker is set by a `/handoff <name>` slash command, which the loop
// installs into the wrapper plugin's commands dir on first run. The command also
// kills the running claude (SIGTERM to its parent), so the user doesn't need
// to type /exit themselves.

const handoffSlashCommand = `---
description: Hand off to another claude-profiles profile. Pass a profile id or describe an intent in plain English; the agent will pick the best-fit profile and ask whether to start with fresh context or resume this conversation.
argument-hint: [profile-id | intent] [--fresh | --keep]
allowed-tools: AskUserQuestion, Bash
---
The user typed ` + "`/handoff $ARGUMENTS`" + `.

# Step 1 — pick the target profile id

  - empty $ARGUMENTS (or only mode flags --fresh/--keep) → target is the empty string (wrapper will open its picker)
  - $ARGUMENTS exactly matches an "Available profiles" id in your session context → that's the target
  - free-form intent (e.g. "now I need to analyze revenue loss") → classify against the profile list and pick the closest fit
  - if no profile list is in your context, /handoff was invoked outside the claude-profiles run wrapper — TELL THE USER and do not run any command

# Step 2 — decide context mode

  - args contain ` + "`--fresh`" + ` → mode=fresh
  - args contain ` + "`--keep`" + ` → mode=keep
  - otherwise → use the AskUserQuestion tool to ask the user:

      Question: "Hand off to <target>. Start with fresh context or resume this conversation?"
      Header:   "Context"
      Options:
        - label: "Fresh context",      description: "Start a new session. I'll write a brief so the next agent picks up without friction."
        - label: "Resume conversation", description: "Continue this exact conversation under the new profile."

    Map the answer: "Fresh context" → mode=fresh, "Resume conversation" → mode=keep.

# Step 3 — if mode=fresh, build a handoff brief

Write a tight handoff brief covering, in 5–10 bullets:

  - What was being worked on (one line)
  - Key decisions or findings reached so far
  - Open questions / next actions
  - Files, links, tickets, identifiers the next session will need
  - Anything the next agent should NOT re-do

No preamble. No filler. Make every bullet load-bearing. Keep it under ~250 words.

# Step 4 — write the marker and exit

Use the Bash tool to run EXACTLY this script (substitute <TARGET>, <MODE>, and <BRIEF>; <BRIEF> is the empty string for mode=keep). The env guard refuses to kill the session if /handoff is invoked outside the wrapper:

if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/handoff only works inside 'claude-profiles run' wrapper" >&2; exit 1; fi
mkdir -p "$HOME/.claude-profiles"
cat > "$HOME/.claude-profiles/next-marker" <<'HANDOFF_MARKER_EOF'
{"profile": "<TARGET>", "mode": "<MODE>", "brief": "<BRIEF>"}
HANDOFF_MARKER_EOF
kill $PPID

Use a single-quoted heredoc so the brief survives shell interpolation; escape ` + "`\"`" + ` inside <BRIEF> as ` + "`\\\"`" + ` and replace literal newlines with the two characters ` + "`\\n`" + `.

# Step 5 — acknowledge

Briefly tell the user: which profile is queued, whether mode is fresh or keep, and (for fresh) that you wrote a handoff brief.
`

func cmdRun(args []string) {
	// Drop any leftover marker from a prior crashed wrapper. Markers only
	// have meaning while a single wrapper loop is alive — finding one at
	// startup means a previous run died without consuming it.
	cleanStaleMarker()

	// If we're not already inside tmux, transparently re-exec ourselves
	// under `tmux new-session` so /delegate has a place to drop its windows.
	// Opt out with CLAUDE_PROFILES_NO_TMUX=1 (for users without tmux or who
	// want plain-terminal behaviour). This call replaces the process when it
	// fires; on the user's POV `claude-profiles run …` always lands inside
	// tmux on the first invocation.
	bootstrapTmuxIfNeeded(tmuxSessionName(args), append([]string{"run"}, args...))

	// Pre-extract --resume <id> so a caller (hub bg attach, or CLI) can ask
	// the wrapper to enter the loop with an existing session id ready to
	// resume. Everything else is positional: <profile> [extra claude args…].
	initialResumeID := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--resume" && i+1 < len(args) {
			initialResumeID = args[i+1]
			i++
			continue
		}
		positional = append(positional, args[i])
	}
	args = positional

	var profile string
	if len(args) > 0 {
		profile = args[0]
		args = args[1:]
	}
	if profile == "" {
		picked, err := pickProfile()
		if err != nil {
			fatal(err)
		}
		profile = picked
	}
	passThrough := args

	if err := ensureHandoffSlashCommand(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install /handoff slash command: %v\n", err)
	}
	if err := ensureWrapperPluginHooks(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install wrapper-plugin hooks: %v\n", err)
	}

	binary, err := exec.LookPath("claude")
	if err != nil {
		fatal(fmt.Errorf("claude not found in PATH"))
	}

	// Pidfile lifecycle: write at startup, update each iteration, remove on
	// clean exit. A SIGINT/SIGTERM handler removes it before os.Exit so a
	// Ctrl+C doesn't leave a ghost entry behind.
	cwd, _ := os.Getwd()
	wrapper := &RunningWrapper{
		PID:       os.Getpid(),
		Profile:   profile,
		Cwd:       cwd,
		StartedAt: time.Now().Unix(),
	}
	_ = writeRunningPidfile(wrapper)
	defer removeRunningPidfile(wrapper.PID)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		removeRunningPidfile(wrapper.PID)
		os.Exit(130)
	}()

	resumeID := initialResumeID
	// On a wrapper-attach (CLI/hub supplied --resume), the first iteration
	// should just open claude --resume <id> interactively, not auto-continue
	// the conversation as we do after /handoff. Flip false after the first
	// iteration so subsequent /handoff relaunches still auto-continue.
	skipAutoContinue := initialResumeID != ""
	firstIteration := true
	for {
		loc, err := resolveProfileLocation(profile)
		if err != nil {
			fatal(err)
		}
		if loadProfilePrefs(filepath.Dir(loc.JSONPath)).Disabled {
			fatal(fmt.Errorf("profile %q is disabled — enable it in the hub (Tab → palette → h)", profile))
		}
		p, err := loadProfileAt(loc.JSONPath)
		if err != nil {
			fatal(err)
		}

		// If the current directory is already a linked git worktree (e.g. the
		// user resumed an existing worktree session), skip creating another one.
		// Setting p.Worktree = false here affects only this loop iteration —
		// the profile is reloaded fresh each time, and isLinkedWorktree() will
		// still return true on subsequent iterations, so the flag stays suppressed.
		if p.Worktree && isLinkedWorktree() {
			p.Worktree = false
		}

		// On first launch: if the profile wants a worktree and we're already
		// inside a tmux session, open a new tmux window so the worktree session
		// gets its own pane while the full wrapper lifecycle stays intact inside
		// the new window. The guard env var prevents the new window from
		// recursing back into this branch.
		if firstIteration && p.Worktree && tmuxAvailable() &&
			os.Getenv("TMUX") != "" && os.Getenv("CLAUDE_PROFILES_WORKTREE_WINDOW") == "" {
			if openWorktreeWindow(profile, passThrough, initialResumeID) {
				return
			}
			// fall through: window creation failed, run in current pane
		}

		// On the first launch only: if the profile defines prompts and the user
		// has not already supplied a message, show a picker. Skipped when
		// resuming (the conversation already has context) or when passThrough
		// is already set (explicit CLI argument takes precedence).
		if firstIteration && len(p.Prompts) > 0 && len(passThrough) == 0 && initialResumeID == "" {
			if chosen, err := pickPrompt(p.Prompts); err == nil && chosen != "" {
				passThrough = []string{chosen}
			}
		}
		firstIteration = false

		settingsPath := ""
		if loc.Builtin == "" {
			if augmented, err := runSettingsWithHook(p, ""); err == nil {
				settingsPath = augmented
			}
		}

		// For real profiles: pin claude to the profile's MCP config (split
		// format prefers .mcp.json; combined format falls back to profile.json).
		// For built-ins: skip both --strict-mcp-config and --mcp-config so
		// claude uses its native MCP discovery — that's the whole point of the
		// built-in profiles.
		claudeArgs := []string{"claude"}
		if loc.Builtin == "" {
			mcpConfigPath := filepath.Join(filepath.Dir(loc.JSONPath), ".mcp.json")
			if _, err := os.Stat(mcpConfigPath); err != nil {
				mcpConfigPath = loc.JSONPath
			}
			claudeArgs = append(claudeArgs, "--strict-mcp-config", "--mcp-config", mcpConfigPath)
		}
		claudeArgs = append(claudeArgs, claudeFlags(p, settingsPath)...)
		// Always load the wrapper-plugin so /handoff survives --setting-sources=
		// in isolated mode. The plugin is a tiny dir containing commands/
		// handoff.md — claude's --plugin-dir auto-discovery loads it without
		// touching user/project settings.
		claudeArgs = append(claudeArgs, "--plugin-dir", wrapperPluginPath())
		// If the profile folder bundles commands/skills/agents/hooks, load it
		// too. Multiple --plugin-dir flags stack; both plugins coexist.
		if dir := pluginDirFor(*loc); dir != "" {
			claudeArgs = append(claudeArgs, "--plugin-dir", dir)
		}
		// Surface the profile name in the prompt box, terminal title, and
		// /resume picker so the user always knows which profile is active.
		claudeArgs = append(claudeArgs, "--name", loc.QualifiedID)
		if resumeID != "" {
			claudeArgs = append(claudeArgs, "--resume", resumeID)
			// On wrapper-attach to a /bg'd session, claude refuses plain
			// --resume because the supervisor still owns the session id. Fork
			// off a copy. We only do this on the first iteration of attach;
			// subsequent /handoff relaunches target our own forked id, which
			// plain --resume handles fine.
			if skipAutoContinue {
				claudeArgs = append(claudeArgs, "--fork-session")
			}
		}
		// First launch: pass through caller-supplied trailing args (e.g. an
		// initial prompt). On relaunches after a /handoff we instead inject a
		// short "continue" message — claude treats trailing args after --resume
		// as the next user message, so the agent picks up the task itself
		// without the user having to type anything.
		switch {
		case len(passThrough) > 0:
			claudeArgs = append(claudeArgs, "--")
			claudeArgs = append(claudeArgs, passThrough...)
			passThrough = nil
		case resumeID != "" && !skipAutoContinue:
			claudeArgs = append(claudeArgs, "--", fmt.Sprintf(
				"[claude-profiles] Profile switched to %s — continue from where you left off. Any new tools and MCP servers from this profile are now available; use them if helpful.",
				loc.QualifiedID))
		}
		skipAutoContinue = false

		mode := "fresh"
		switch {
		case resumeID != "" && skipAutoContinue:
			mode = "fork from " + shortSession(resumeID)
		case resumeID != "":
			mode = "resume " + shortSession(resumeID) + ", auto-continue"
		}
		info("→ %s (%s)", loc.QualifiedID, mode)
		recordRecentLaunch(loc.QualifiedID)

		// Refresh the pidfile so the hub can read the current profile + session.
		wrapper.Profile = loc.QualifiedID
		wrapper.SessionID = resumeID
		_ = writeRunningPidfile(wrapper)
		// Persist session→profile mapping so bg'd sessions remain mappable
		// in the hub long after this wrapper exits.
		recordSessionProfile(resumeID, loc.QualifiedID)

		// Snapshot existing session files so we can detect which one this
		// invocation creates (used for --resume on the next iteration).
		before := snapshotSessionFiles("")

		cmd := exec.Command(binary, claudeArgs[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Marker env vars: CLAUDE_PROFILES_RUN guards /handoff and /delegate
		// from firing outside the wrapper; CLAUDE_PROFILES_WRAPPER_PID lets
		// slash commands locate the wrapper's pidfile (which carries the live
		// session id) without walking the process tree — $PPID inside the
		// slash command's Bash is claude itself, two hops below the wrapper.
		cmd.Env = append(os.Environ(),
			"CLAUDE_PROFILES_RUN=1",
			fmt.Sprintf("CLAUDE_PROFILES_WRAPPER_PID=%d", os.Getpid()),
		)

		// Poll the project sessions dir while claude is running so the hub's
		// "attach" prompt can offer a session id even during the first
		// iteration — without polling, SessionID stays empty until claude
		// exits, which means no attach is offered for a freshly-started
		// wrapper.
		pollDone := make(chan struct{})
		go pollForSessionID(before, wrapper, pollDone)

		_ = cmd.Run() // claude's exit code isn't meaningful for our purpose
		close(pollDone)

		marker, hasMarker := consumeNextProfileMarker()
		if !hasMarker {
			return // user /exit'd or claude crashed without a queued handoff
		}

		next := marker.Profile
		if next == "" {
			fmt.Fprintln(os.Stderr)
			info("Pick the next profile (Esc to keep current):")
			picked, perr := pickProfile()
			if perr != nil || picked == "" {
				info("Keeping current profile: %s", profile)
				next = profile
			} else {
				next = picked
			}
		}

		if _, err := resolveProfileLocation(next); err != nil {
			fmt.Fprintf(os.Stderr, "queued profile %q does not resolve: %v\n", next, err)
			return
		}

		// Mode dispatch: "fresh" → discard the resumeID and queue the handoff
		// brief as the next launch's initial user message. Default ("keep" or
		// legacy plain-text marker) → set resumeID to the just-finished
		// session and let the auto-continue prompt fire on the next iteration.
		switch marker.Mode {
		case "fresh":
			resumeID = ""
			skipAutoContinue = true // no "continue from where you left off" — the brief replaces it
			body := marker.Brief
			if body == "" {
				body = fmt.Sprintf("[claude-profiles] Profile switched to %s. The previous session declined to write a handoff brief; pick up from a clean slate.", next)
			} else {
				body = fmt.Sprintf("[claude-profiles handoff from previous profile]\n\n%s", body)
			}
			passThrough = []string{body}
		default: // "keep" or unset (legacy plain-text marker)
			after := snapshotSessionFiles("")
			if id := findNewOrUpdatedSession(before, after); id != "" {
				resumeID = id
			}
		}
		profile = next
		fmt.Fprintln(os.Stderr)
	}
}

// snapshotSessionFiles records mtimes of every .jsonl session file across all
// session dirs for the given root — including worktree subdirs. Pass "" to use
// the current process cwd. Keys are ABSOLUTE paths so callers can distinguish
// files with the same basename across dirs and resolve the actual file path
// directly without re-deriving it from cwd (which may not match the dir the
// file lives in once --worktree is involved).
func snapshotSessionFiles(root string) map[string]int64 {
	out := map[string]int64{}
	for _, dir := range sessionDirsToWatch(root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, _ := e.Info()
			out[filepath.Join(dir, e.Name())] = info.ModTime().UnixNano()
		}
	}
	return out
}

// sessionDirsToWatch returns all ~/.claude/projects/ subdirs that could receive
// session files for the given root directory. Pass "" to use the current
// process cwd. It always includes the main encoded dir, plus any worktree dirs
// (pattern: <main-encoded>--claude-worktrees-<name>) that already exist on
// disk. The before-snapshot won't include a worktree dir that hasn't been
// created yet — that's fine: new files in it will naturally appear as "new" in
// the after-snapshot.
func sessionDirsToWatch(root string) []string {
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil
		}
	}
	mainDir := encodedSessionsDir(root)
	mainEncoded := filepath.Base(mainDir)
	projectsRoot := filepath.Dir(mainDir)

	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return []string{mainDir}
	}

	// Worktree dirs encode as "<main>--claude-worktrees-<name>" because
	// /.claude/worktrees/ → "--claude-worktrees-" after / and . replacement.
	worktreePrefix := mainEncoded + "--claude-worktrees-"
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == mainEncoded || strings.HasPrefix(e.Name(), worktreePrefix) {
			dirs = append(dirs, filepath.Join(projectsRoot, e.Name()))
		}
	}
	if len(dirs) == 0 {
		return []string{mainDir}
	}
	return dirs
}

// findNewOrUpdatedSession returns the session ID of the most-recently-modified
// .jsonl that is either new or has a newer mtime than the "before" snapshot.
// Returns "" if nothing matches. Snapshot keys are absolute paths; the session
// ID is the basename minus ".jsonl".
func findNewOrUpdatedSession(before, after map[string]int64) string {
	var newestPath string
	var newestMtime int64
	for path, mtime := range after {
		prior, existed := before[path]
		if existed && mtime <= prior {
			continue
		}
		if mtime > newestMtime {
			newestMtime = mtime
			newestPath = path
		}
	}
	if newestPath == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(newestPath), ".jsonl")
}

// projectSessionsDir returns the sessions dir for the current cwd.
func projectSessionsDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return encodedSessionsDir(cwd)
}

// encodedSessionsDir returns ~/.claude/projects/<encoded-cwd> where
// <encoded-cwd> is the absolute cwd with BOTH "/" and "." replaced by "-" —
// matching Claude Code's on-disk encoding. The dot replacement is the
// non-obvious bit: ".claude" inside a path becomes "-claude", which produces
// the characteristic "--claude" double-dash for "/.claude".
func encodedSessionsDir(cwd string) string {
	encoded := strings.ReplaceAll(cwd, "/", "-")
	encoded = strings.ReplaceAll(encoded, ".", "-")
	return filepath.Join(claudeRootDirPath(), "projects", encoded)
}

// cleanStaleMarker removes any pre-existing marker file at wrapper startup.
// The marker is supposed to live for milliseconds inside one loop iteration;
// finding one at startup is always a stale leftover from a prior crash.
func cleanStaleMarker() {
	p := nextMarkerPath()
	info, err := os.Stat(p)
	if err != nil {
		return
	}
	warn("Removing stale profile-switch marker from %s (last modified %s)",
		p, info.ModTime().Format("15:04:05"))
	_ = os.Remove(p)
}

// HandoffMarker is the JSON payload the /handoff slash command writes into
// ~/.claude-profiles/next-marker. The wrapper reads it after the child
// claude process exits and uses it to decide how to relaunch.
type HandoffMarker struct {
	Profile string `json:"profile"`         // target profile id; empty → open picker
	Mode    string `json:"mode,omitempty"`  // "keep" (default) or "fresh"
	Brief   string `json:"brief,omitempty"` // handoff text for mode=fresh
}

// consumeNextProfileMarker reads and removes the /handoff marker. The second
// return value is true when the file was present (even if empty content).
// Accepts both the new JSON format and the legacy plain-text format (just a
// profile id) for backward compatibility.
func consumeNextProfileMarker() (HandoffMarker, bool) {
	p := nextMarkerPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return HandoffMarker{}, false
	}
	_ = os.Remove(p)
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return HandoffMarker{}, true
	}
	if strings.HasPrefix(raw, "{") {
		var m HandoffMarker
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			return m, true
		}
		// Fall through to plain-text interpretation if JSON parse failed.
	}
	return HandoffMarker{Profile: raw}, true
}

// ensureHandoffSlashCommand installs handoff.md into the wrapper-plugin dir at
// ~/.claude-profiles/claude-profiles/commands/handoff.md. The wrapper passes
// --plugin-dir <wrapper-plugin> on every launch, so /handoff is available
// regardless of isolated mode (which strips user/project settings.json and
// — empirically — the slash commands they normally pull in).
//
// Idempotent — only rewrites when the embedded text drifts. Also cleans up
// the old switch.md / ~/.claude/commands/switch.md leftovers so /help only
// shows one entry.
func ensureHandoffSlashCommand() error {
	dir := filepath.Join(wrapperPluginPath(), "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	commands := []struct{ name, body string }{
		{"handoff.md", handoffSlashCommand},
		{"generate.md", generateSlashCommand},
		// /delegate is installed unconditionally. Atto III made bg the only
		// dispatch path — no tmux dependency, no live-watcher subprocess.
		// The launch script rejects the now-removed --bg / --legacy-tmux
		// flags loudly so older invocations surface as errors instead of
		// silent surprises.
		{"delegate.md", delegateSlashCommand},
	}
	for _, sc := range commands {
		path := filepath.Join(dir, sc.name)
		if cur, err := os.ReadFile(path); err != nil || string(cur) != sc.body {
			if err := os.WriteFile(path, []byte(sc.body), 0o644); err != nil {
				return err
			}
		}
	}
	// Bundled helper script the slash command invokes via ${CLAUDE_PLUGIN_ROOT}.
	// chmod 0o755 so it's executable. Atto III dropped delegate-watch.sh
	// alongside the tmux runner — the bg watcher is a Go subprocess
	// (_delegate-bg-watcher), not a bash one.
	scriptDir := filepath.Join(wrapperPluginPath(), "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return err
	}
	for _, s := range []struct{ name, body string }{
		{"delegate-launch.sh", delegateLaunchScript},
	} {
		path := filepath.Join(scriptDir, s.name)
		if cur, err := os.ReadFile(path); err != nil || string(cur) != s.body {
			if err := os.WriteFile(path, []byte(s.body), 0o755); err != nil {
				return err
			}
		}
	}
	// One-shot cleanup of stale slash-command + script files from earlier versions.
	_ = os.Remove(filepath.Join(claudeRootDirPath(), "commands", "switch.md"))
	_ = os.Remove(filepath.Join(dir, "switch.md"))
	_ = os.Remove(filepath.Join(scriptDir, "delegate-watch.sh"))
	return nil
}

// pollForSessionID watches the project sessions dir for the new .jsonl that
// claude creates after launch, then writes the session id into the wrapper's
// pidfile so the hub's attach prompt has something to offer. Stops when the
// done channel closes (claude has exited) or after 60s (claude is slow to
// commit its first .jsonl on some setups, but past a minute it's clearly not
// our session that's writing).
func pollForSessionID(before map[string]int64, w *RunningWrapper, done <-chan struct{}) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(60 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-timeout:
			return
		case <-ticker.C:
			id := findNewOrUpdatedSession(before, snapshotSessionFiles(""))
			if id == "" || id == w.SessionID {
				continue
			}
			w.SessionID = id
			_ = writeRunningPidfile(w)
			recordSessionProfile(id, w.Profile)
			return
		}
	}
}

// openWorktreeWindow opens a new tmux window in the current session running
// this profile's wrapper with CLAUDE_PROFILES_WORKTREE_WINDOW=1, which prevents
// the new window from recursing into this function. The current window exits
// (tmux auto-focuses the new one), keeping the full wrapper lifecycle intact
// inside the new window. Returns true when the window was opened successfully.
func openWorktreeWindow(profile string, passThrough []string, resumeID string) bool {
	self, err := os.Executable()
	if err != nil {
		warn("cannot locate own binary for worktree window: %v", err)
		return false
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return false
	}

	// Build the inner shell command: CLAUDE_PROFILES_WORKTREE_WINDOW=1 <self> run <profile> [--resume <id>] [-- args]
	parts := []string{shellQuote(self), "run", shellQuote(profile)}
	if resumeID != "" {
		parts = append(parts, "--resume", shellQuote(resumeID))
	}
	if len(passThrough) > 0 {
		parts = append(parts, "--")
		for _, pt := range passThrough {
			parts = append(parts, shellQuote(pt))
		}
	}
	innerCmd := "CLAUDE_PROFILES_WORKTREE_WINDOW=1 " + strings.Join(parts, " ")

	// Window name mirrors the tmux session naming convention.
	windowName := strings.NewReplacer("/", "-", ".", "-", ":", "-").Replace(profile)

	if err := exec.Command(tmuxBin, "new-window", "-n", windowName, innerCmd).Run(); err != nil {
		warn("Failed to open new tmux window for worktree: %v", err)
		return false
	}
	info("↳ worktree session opened in new tmux window")
	return true
}

func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// findTmuxBin returns the path to the tmux binary.
// Falls back to well-known installation directories when tmux is not on PATH
// (e.g. Homebrew on macOS when the process inherits a stripped PATH).
func findTmuxBin() (string, error) {
	if bin, err := exec.LookPath("tmux"); err == nil {
		return bin, nil
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
		"/bin/tmux",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("tmux not found")
}

// tmuxAvailable reports whether tmux is findable and the user hasn't opted out.
func tmuxAvailable() bool {
	if os.Getenv("CLAUDE_PROFILES_NO_TMUX") != "" {
		return false
	}
	_, err := findTmuxBin()
	return err == nil
}

// offerTmuxInstall interactively proposes installing tmux when it is not found.
// Returns true if tmux was successfully installed (caller can retry bootstrap).
func offerTmuxInstall() bool {
	installCmd := suggestTmuxInstallCmd()
	info("tmux is not installed — /delegate needs it to open sub-sessions in new windows.")
	if installCmd == "" {
		info("Install tmux for your platform, then re-run claude-profiles.")
		info("(Pass --no-tmux or set CLAUDE_PROFILES_NO_TMUX=1 to skip this prompt.)")
		return false
	}
	info("Suggested: %s", installCmd)
	if !confirm("Install tmux now?") {
		info("Continuing without tmux. Pass --no-tmux to suppress this prompt.")
		return false
	}
	parts := strings.Fields(installCmd)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		warn("Install failed: %v", err)
		return false
	}
	if _, err := findTmuxBin(); err != nil {
		warn("tmux still not found after install.")
		return false
	}
	success("tmux installed.")
	return true
}

// suggestTmuxInstallCmd returns a best-guess install command for the current
// platform, or "" when no package manager is detected.
// findBin looks for a binary on PATH and then in known fallback directories.
func findBin(name string, fallbacks []string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	for _, f := range fallbacks {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

func suggestTmuxInstallCmd() string {
	switch runtime.GOOS {
	case "darwin":
		if findBin("brew", []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"}) {
			return "brew install tmux"
		}
		if findBin("port", []string{"/opt/local/bin/port"}) {
			return "sudo port install tmux"
		}
	case "linux":
		for _, pm := range []struct{ bin, cmd string }{
			{"apt-get", "sudo apt-get install -y tmux"},
			{"apt", "sudo apt install -y tmux"},
			{"dnf", "sudo dnf install -y tmux"},
			{"yum", "sudo yum install -y tmux"},
			{"pacman", "sudo pacman -S --noconfirm tmux"},
			{"zypper", "sudo zypper install -y tmux"},
		} {
			if _, err := exec.LookPath(pm.bin); err == nil {
				return pm.cmd
			}
		}
	}
	return ""
}

// bootstrapTmuxIfNeeded re-execs the current process under `tmux new-session`
// when stdin is a TTY and we're not already inside tmux. Refuses (silently
// returns to normal flow) when:
//   - we're already in tmux ($TMUX set)
//   - the user opted out (CLAUDE_PROFILES_NO_TMUX set)
//   - stdin isn't a TTY (e.g. piped invocation)
//   - the tmux binary isn't on PATH (a warning is printed)
//
// On success this never returns — syscall.Exec replaces the process. Inside
// the new tmux session, $TMUX is set, so the inner re-invocation hits this
// same function and falls through to the normal flow. innerArgs is the
// argv (excluding the binary itself) the inner instance should be launched
// with — pass `nil` for the bare interactive hub.
func bootstrapTmuxIfNeeded(sessionName string, innerArgs []string) {
	if os.Getenv("TMUX") != "" {
		return
	}
	if os.Getenv("CLAUDE_PROFILES_NO_TMUX") != "" {
		return
	}
	if !isTTY() {
		return
	}
	tmuxBin, err := findTmuxBin()
	if err != nil {
		if offerTmuxInstall() {
			bootstrapTmuxIfNeeded(sessionName, innerArgs)
		}
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		warn("cannot locate own binary; skipping tmux bootstrap.")
		return
	}

	innerParts := []string{shellQuote(self)}
	for _, a := range innerArgs {
		innerParts = append(innerParts, shellQuote(a))
	}
	inner := strings.Join(innerParts, " ")

	// Name the session per profile so re-running `claude-profiles run X`
	// attaches to an existing X session instead of spinning up a parallel
	// one. -A: attach if the named session exists, create otherwise.
	//
	// When the host terminal speaks tmux control mode (iTerm2, Warp), launch
	// with -CC so windows render as native tabs in the host UI. Plain
	// terminals get the standard tmux UI. Opt out with
	// CLAUDE_PROFILES_NO_TMUX_CC=1.
	tmuxArgs := []string{"tmux"}
	if supportsTmuxControlMode() && os.Getenv("CLAUDE_PROFILES_NO_TMUX_CC") == "" {
		tmuxArgs = append(tmuxArgs, "-CC")
	}
	tmuxArgs = append(tmuxArgs, "new-session", "-A", "-s", sessionName, inner)
	if err := syscall.Exec(tmuxBin, tmuxArgs, os.Environ()); err != nil {
		warn("tmux bootstrap failed (%v); continuing without tmux.", err)
	}
}

// supportsTmuxControlMode reports whether the host terminal speaks tmux's
// control-mode protocol (-CC). Only iTerm2 implements it natively; Warp
// advertises tmux integration but does NOT consume -CC output and instead
// dumps the raw control-mode escapes to the screen (verified empirically).
// Other terminals would do the same.
func supportsTmuxControlMode() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app"
}

// tmuxSessionName picks a deterministic session name from the first positional
// arg (the profile id) plus a short hash of the current working directory.
// Slashes (repo profiles like "alias/name") become dashes — tmux session names
// disallow "/" and "." in some versions, and ":" is reserved as a separator.
// Without the cwd suffix, launching from a different repo would attach to the
// session created from the first one, dragging the user back to that cwd.
func tmuxSessionName(args []string) string {
	suffix := cwdSessionSuffix()
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		safe := a
		for _, ch := range []string{"/", ".", ":"} {
			safe = strings.ReplaceAll(safe, ch, "-")
		}
		if safe != "" {
			return "cp-" + safe + "-" + suffix
		}
	}
	return "claude-profiles-" + suffix
}

// cwdSessionSuffix returns a short, stable hash of the current working
// directory so each cwd gets its own tmux session. Symlinks are resolved so
// symlinked checkouts collapse to the same session.
func cwdSessionSuffix() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "nocwd"
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != "" {
		cwd = resolved
	}
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:])[:8]
}

// shellQuote single-quotes s for safe interpolation into a tmux shell-command
// argument. Embedded single quotes are escaped via the standard POSIX dance.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ── SessionStart hook ────────────────────────────────────────────────────────
//
// Claude Code's SessionStart hook can inject text into a fresh session's
// context via `hookSpecificOutput.additionalContext`. We use that to brief
// the agent on which profiles exist (so /handoff can take free-form intent
// and the agent can classify) and on the exact bash command to write the
// marker file when the user requests a handoff.

const sessionStartHookHeader = `# claude-profiles: in-session profile handoff

You are running inside the ` + "`claude-profiles run`" + ` wrapper. The user can hand off mid-session via the ` + "`/handoff`" + ` slash command. When they do, you write a JSON marker to ~/.claude-profiles/next-marker and end this session; the wrapper will relaunch claude under the chosen profile, either resuming the conversation or starting fresh with a brief.

Marker shape (JSON):
  {"profile": "<id>", "mode": "keep"|"fresh", "brief": "<text or empty>"}

Protocol when ` + "`/handoff <argument>`" + ` is invoked:
1. Pick the target profile id from <argument>:
   - empty → empty target (wrapper opens its picker)
   - exact match to an Available-profiles id → that id
   - free-form intent → classify against the list, pick the closest fit
2. Decide the mode:
   - args contain ` + "`--fresh`" + ` → mode=fresh
   - args contain ` + "`--keep`" + ` → mode=keep
   - otherwise → use the AskUserQuestion tool to ask the user whether to start fresh or resume this conversation. Map "Fresh context" → fresh, "Resume conversation" → keep.
3. If mode=fresh, write a tight handoff brief (5–10 bullets, <250 words) covering what was being worked on, key decisions, open questions, and references the next session will need. Escape ` + "`\"`" + ` as ` + "`\\\"`" + ` and newlines as ` + "`\\n`" + ` so the JSON survives.
4. Use the Bash tool to write the marker and kill the session (the env guard refuses to kill if /handoff is invoked outside the wrapper):
   if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/handoff only works inside 'claude-profiles run' wrapper" >&2; exit 1; fi
   mkdir -p "$HOME/.claude-profiles"
   cat > "$HOME/.claude-profiles/next-marker" <<'HANDOFF_MARKER_EOF'
   {"profile": "<TARGET>", "mode": "<MODE>", "brief": "<BRIEF>"}
   HANDOFF_MARKER_EOF
   kill $PPID

Acknowledge the handoff briefly to the user (target, mode) before running the Bash command.

Available profiles:
`

func cmdHookSessionStart() {
	var sb strings.Builder
	sb.WriteString(sessionStartHookHeader)

	locs, err := listAllLocations()
	if err == nil {
		// Supplement with project profiles from other repos (prefs-store discovery),
		// so sessions launched from a different CWD still see all configured profiles.
		seen := map[string]bool{}
		for _, loc := range locs {
			seen[loc.QualifiedID] = true
		}
		for _, loc := range listKnownProjectLocations() {
			if !seen[loc.QualifiedID] {
				seen[loc.QualifiedID] = true
				locs = append(locs, loc)
			}
		}

		// Strip disabled profiles — they should not be offered to the agent.
		prefsStore := loadPrefsStore()
		filtered := locs[:0]
		for _, loc := range locs {
			if !prefsStore[canonicalProfileDir(filepath.Dir(loc.JSONPath))].Disabled {
				filtered = append(filtered, loc)
			}
		}
		locs = filtered

		// Workspace mode: when CWD is NOT inside any repo with .claude-profiles/,
		// group profiles by owning repo so the orchestrator knows which are
		// usable anywhere vs which require --dir <owning repo>. Inside a single
		// repo, keep the flat list (current behavior).
		if findCwdProfilesDir() == "" {
			writeGroupedProfileList(&sb, locs)
		} else {
			for _, loc := range locs {
				p, _ := loadProfileAt(loc.JSONPath)
				desc := ""
				if p != nil && strings.TrimSpace(p.Description) != "" {
					desc = " — " + strings.TrimSpace(p.Description)
				}
				sb.WriteString(fmt.Sprintf("- %s%s\n", loc.QualifiedID, desc))
			}
		}
	}

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": sb.String(),
		},
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

// writeGroupedProfileList renders the profile list grouped by owning repo for
// workspace-mode SessionStart hooks. Profiles with empty OwnerRepo are listed
// under "Common (usable on any --dir)". Project-local profiles are grouped by
// the basename of their OwnerRepo; profiles with _worktree are annotated so
// the orchestrator knows --dir is required.
func writeGroupedProfileList(sb *strings.Builder, locs []ProfileLocation) {
	type entry struct {
		id       string
		desc     string
		needsDir bool
	}
	common := []entry{}
	perRepo := map[string][]entry{} // owner abspath → entries
	for _, loc := range locs {
		p, _ := loadProfileAt(loc.JSONPath)
		desc := ""
		needsDir := false
		if p != nil {
			if strings.TrimSpace(p.Description) != "" {
				desc = " — " + strings.TrimSpace(p.Description)
			}
			needsDir = p.Worktree
		}
		e := entry{id: loc.QualifiedID, desc: desc, needsDir: needsDir}
		if loc.OwnerRepo == "" {
			common = append(common, e)
		} else {
			perRepo[loc.OwnerRepo] = append(perRepo[loc.OwnerRepo], e)
		}
	}

	if len(common) > 0 {
		sb.WriteString("\nCommon (usable on any --dir):\n")
		for _, e := range common {
			suffix := ""
			if e.needsDir {
				suffix = " (requires --dir)"
			}
			sb.WriteString(fmt.Sprintf("- %s%s%s\n", e.id, suffix, e.desc))
		}
	}

	owners := make([]string, 0, len(perRepo))
	for k := range perRepo {
		owners = append(owners, k)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		sb.WriteString(fmt.Sprintf("\nFrom %s (use --dir %s):\n", filepath.Base(owner), owner))
		for _, e := range perRepo[owner] {
			suffix := ""
			if e.needsDir {
				suffix = " (requires --dir)"
			}
			sb.WriteString(fmt.Sprintf("- %s%s%s\n", e.id, suffix, e.desc))
		}
	}
}

// worktreeCacheDirs is the list of well-known dependency-cache directories that
// are safe to share across git worktrees via a symlink to the main working tree.
// Omits dirs that store per-branch build artifacts (target, dist, .next, .venv)
// or that embed absolute paths (Python virtualenvs).
var worktreeCacheDirs = []string{
	"node_modules",
	"vendor",
	".yarn/cache",
	".pnpm-store",
	"bower_components",
	"Pods",
}

// cmdHookWorktreeCaches is the _hook-worktree-caches SessionStart handler.
// When the current directory is a linked git worktree it symlinks any
// worktreeCacheDirs that exist in the main working tree but are absent here,
// so installs don't need to be repeated for every worktree.
func cmdHookWorktreeCaches() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	gitDir, err := gitOutput("rev-parse", "--absolute-git-dir")
	if err != nil {
		return
	}

	// git-common-dir may be relative — resolve it to absolute.
	rawCommon, err := gitOutput("rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	commonDir := rawCommon
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	commonDir = filepath.Clean(commonDir)

	// In the main working tree both paths are the same; linked worktrees have
	// a per-worktree gitDir under <common>/.git/worktrees/<name>.
	if filepath.Clean(gitDir) == commonDir {
		return
	}

	mainRoot := filepath.Dir(commonDir) // parent of <main>/.git

	topLevel, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return
	}

	for _, rel := range worktreeCacheDirs {
		src := filepath.Join(mainRoot, rel)
		dst := filepath.Join(topLevel, rel)

		if _, err := os.Stat(src); err != nil {
			continue // not present in main repo
		}
		if _, err := os.Lstat(dst); err == nil {
			continue // already exists in worktree
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		_ = os.Symlink(src, dst)
	}
}

// cmdHookGuardWorktreeWrites is the _hook-guard-worktree-writes PreToolUse
// handler. When the current session is inside a linked git worktree it blocks:
//   - Write/Edit operations targeting absolute paths inside the main repo but
//     outside this worktree.
//   - Bash commands that switch the working branch to main/master (which would
//     collapse the worktree's isolation).
//
// It is a no-op outside worktrees so it adds negligible overhead to normal sessions.
func cmdHookGuardWorktreeWrites() {
	if !isLinkedWorktree() {
		return // allow
	}

	topLevel, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return // allow on error
	}
	rawCommon, err := gitOutput("rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	cwd, _ := os.Getwd()
	commonDir := rawCommon
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	mainRoot := filepath.Dir(filepath.Clean(commonDir))

	var input struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			FilePath string `json:"file_path"`
			Command  string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return // allow if input can't be parsed
	}

	sep := string(filepath.Separator)
	switch input.ToolName {
	case "Write", "Edit":
		path := input.ToolInput.FilePath
		if path == "" || !filepath.IsAbs(path) {
			return // relative paths stay inside the worktree cwd — allow
		}
		// Block writes inside the main repo that are NOT inside this worktree
		if strings.HasPrefix(path, mainRoot+sep) && !strings.HasPrefix(path, topLevel+sep) {
			fmt.Fprintf(os.Stderr,
				"Blocked: %s is outside this worktree (%s). Modify files inside the worktree only.\n",
				path, topLevel)
			os.Exit(2)
		}

	case "Bash":
		if bashChangesToMainBranch(input.ToolInput.Command) {
			fmt.Fprintf(os.Stderr,
				"Blocked: switching to main/master is not allowed inside a worktree. "+
					"You are on a dedicated branch. Commit your changes here instead of switching branches.\n")
			os.Exit(2)
		}
	}
}

// bashChangesToMainBranch returns true when the shell command contains a git
// checkout or switch that would change the current HEAD to main or master.
func bashChangesToMainBranch(cmd string) bool {
	for _, line := range strings.Split(cmd, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for i, f := range fields {
			if f != "git" || i+2 >= len(fields) {
				continue
			}
			subCmd := fields[i+1]
			if subCmd != "checkout" && subCmd != "switch" {
				continue
			}
			// Walk past flag arguments to find the branch name
			for j := i + 2; j < len(fields); j++ {
				if strings.HasPrefix(fields[j], "-") {
					continue
				}
				target := strings.TrimPrefix(fields[j], "origin/")
				if target == "main" || target == "master" {
					return true
				}
				break // first non-flag arg is the target, stop
			}
		}
	}
	return false
}

// cmdHookWorktreeBranch is the _hook-worktree-branch SessionStart handler.
// When the current directory is a linked git worktree it injects
// additionalContext telling the agent the actual current branch, since
// Claude Code's gitStatus context is computed from the main working tree and
// may show "main" even inside a worktree.
func cmdHookWorktreeBranch() {
	gitDir, err := gitOutput("rev-parse", "--absolute-git-dir")
	if err != nil {
		return
	}
	rawCommon, err := gitOutput("rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	commonDir := rawCommon
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	if filepath.Clean(gitDir) == filepath.Clean(commonDir) {
		return // main working tree, not a linked worktree
	}

	branch, err := gitOutput("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return
	}

	context := fmt.Sprintf(
		"You are working in a git worktree. Current branch: %s\n"+
			"IMPORTANT: The gitStatus shown in the system context may report 'main' as the current branch — that is wrong. "+
			"You are on branch '%s', not 'main'. Do NOT commit to main. Commit only to '%s'.\n",
		branch, branch, branch,
	)
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": context,
		},
	}
	enc, _ := json.Marshal(out)
	fmt.Println(string(enc))
}

// isLinkedWorktree reports whether the current directory is inside a linked
// git worktree (as opposed to the main working tree).
func isLinkedWorktree() bool {
	gitDir, err := gitOutput("rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	rawCommon, err := gitOutput("rev-parse", "--git-common-dir")
	if err != nil {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	commonDir := rawCommon
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	return filepath.Clean(gitDir) != filepath.Clean(commonDir)
}

// WorktreeInfo describes one of the git worktrees created by claude --worktree
// and found under <repo>/.claude/worktrees/.
type WorktreeInfo struct {
	Name            string    // slug from .claude/worktrees/<name>
	Path            string    // absolute path to the worktree directory
	Branch          string    // current git branch (empty if detached)
	LastSessionID   string    // most-recently-modified session ID in this worktree, or ""
	LastSessionTime time.Time // mtime of the session file, zero if none
	Hint            string    // first user message from the last session, for display
}

// listExistingWorktrees returns all claude-managed worktrees for the current
// git repo (those located under <repo-root>/.claude/worktrees/), enriched
// with the last session for each. Returns nil when not in a git repo or when
// no such worktrees exist.
func listExistingWorktrees() []WorktreeInfo {
	repoRoot, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	worktreeBase := filepath.Join(repoRoot, ".claude", "worktrees")

	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}

	var worktrees []WorktreeInfo
	var cur struct{ path, branch string }
	flush := func() {
		if cur.path == "" {
			return
		}
		// Only keep worktrees under <repo>/.claude/worktrees/
		if !strings.HasPrefix(cur.path, worktreeBase+"/") {
			cur = struct{ path, branch string }{}
			return
		}
		name := cur.path[len(worktreeBase)+1:]
		// Skip the main working tree (shouldn't appear here, but be defensive)
		if name == "" || strings.Contains(name, "/") {
			cur = struct{ path, branch string }{}
			return
		}
		sid, st := lastSessionForWorktreeDir(cur.path)
		worktrees = append(worktrees, WorktreeInfo{
			Name:            name,
			Path:            cur.path,
			Branch:          cur.branch,
			LastSessionID:   sid,
			LastSessionTime: st,
			Hint:            hintFromSessionJSONL(sid, cur.path),
		})
		cur = struct{ path, branch string }{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			cur.branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	return worktrees
}

// lastSessionForWorktreeDir finds the most-recently-modified session JSONL
// file in the Claude Code projects directory that corresponds to worktreePath.
// Returns the session ID and modification time, or ("", zero) if none found.
func lastSessionForWorktreeDir(worktreePath string) (string, time.Time) {
	dir := encodedSessionsDir(worktreePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}
	var newestFile string
	var newestTime time.Time
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, _ := e.Info()
		if mt := info.ModTime(); mt.After(newestTime) {
			newestTime = mt
			newestFile = e.Name()
		}
	}
	if newestFile == "" {
		return "", time.Time{}
	}
	return strings.TrimSuffix(newestFile, ".jsonl"), newestTime
}

// gitOutput runs a git command and returns its trimmed stdout.
func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runSettingsWithHook writes the profile's settings JSON to a temp path and
// returns the path. Hooks are NOT injected here — they live in the wrapper
// plugin's hooks/hooks.json (see ensureWrapperPluginHooks) because Claude
// Code 2.1.x reliably loads plugin hooks via --plugin-dir but does not always
// register Stop / UserPromptSubmit hooks declared in a --settings file.
//
// The originalPath parameter is kept for callers that need to start from an
// existing settings file on disk rather than the profile's inline Settings.
func runSettingsWithHook(p *Profile, originalPath string) (string, error) {
	settings := map[string]any{}
	switch {
	case originalPath != "":
		if data, err := os.ReadFile(originalPath); err == nil {
			_ = json.Unmarshal(data, &settings)
		}
	case len(p.Settings) > 0:
		_ = json.Unmarshal(p.Settings, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}

	out := runSettingsPath()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// wrapperPluginHooksJSON is the canonical hooks file installed into the
// wrapper plugin so the wrapper's per-event handlers (SessionStart,
// UserPromptSubmit, Stop) fire on every claude-profiles-launched session.
// The wrapper plugin is loaded via --plugin-dir, which is the only path
// where Claude Code reliably registers Stop hooks. The handlers self-skip
// when they have no work (e.g. _hook-worktree-caches outside a linked
// worktree, _hook-stop when the active profile has _distill: "off").
const wrapperPluginHooksJSON = `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-session-start"}
        ]
      },
      {
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-worktree-caches"}
        ]
      },
      {
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-worktree-branch"}
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-prompt-submit"}
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-stop"}
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Write|Edit|Bash",
        "hooks": [
          {"type": "command", "command": "claude-profiles _hook-guard-worktree-writes"}
        ]
      }
    ]
  }
}
`

// ensureWrapperPluginHooks writes hooks/hooks.json into the wrapper plugin
// directory. Called from cmdRun startup so the hooks file is always current
// with the binary. Idempotent — only rewrites when content drifts.
func ensureWrapperPluginHooks() error {
	dir := filepath.Join(wrapperPluginPath(), "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "hooks.json")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == wrapperPluginHooksJSON {
		return nil
	}
	return os.WriteFile(path, []byte(wrapperPluginHooksJSON), 0o644)
}
