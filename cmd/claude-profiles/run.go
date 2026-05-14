package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
// The marker is set by a `/switch <name>` slash command, which the loop
// installs into ~/.claude/commands/switch.md on first run. The command also
// kills the running claude (SIGTERM to its parent), so the user doesn't need
// to type /exit themselves.

const switchSlashCommand = `---
description: Switch the active claude-profiles profile. Pass a profile id directly, or describe an intent in plain English and the agent will pick the best-fit profile from the list.
argument-hint: <profile-id | intent>
allowed-tools: Bash
---
The user typed ` + "`/switch $ARGUMENTS`" + `.

Decide the target profile id:
  - empty $ARGUMENTS → target is the empty string (wrapper will open its picker)
  - $ARGUMENTS exactly matches an "Available profiles" id in your session context → that's the target
  - free-form intent (e.g. "now I need to analyze revenue loss") → classify against the profile list and pick the closest fit
  - if no profile list is in your context, /switch was invoked outside the claude-profiles run wrapper — TELL THE USER and do not run any command

Then use Bash to run EXACTLY this one-liner (substitute the chosen profile id into <TARGET>; can be empty). The env guard at the front refuses to kill the session if /switch is invoked outside the wrapper:

if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/switch only works inside 'claude-profiles run' wrapper" >&2; exit 1; fi; mkdir -p "$HOME/.claude-profiles" && printf "%s" "<TARGET>" > "$HOME/.claude-profiles/next-marker" && kill $PPID

Briefly tell the user which profile is queued (or that the picker will open).
`

func cmdRun(args []string) {
	// Drop any leftover marker from a prior crashed wrapper. Markers only
	// have meaning while a single wrapper loop is alive — finding one at
	// startup means a previous run died without consuming it.
	cleanStaleMarker()

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

	if err := ensureSwitchSlashCommand(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install /switch slash command: %v\n", err)
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
	// the conversation as we do after /switch. Flip false after the first
	// iteration so subsequent /switch relaunches still auto-continue.
	skipAutoContinue := initialResumeID != ""
	for {
		loc, err := resolveProfileLocation(profile)
		if err != nil {
			fatal(err)
		}
		p, err := loadProfileAt(loc.JSONPath)
		if err != nil {
			fatal(err)
		}
		// Settings always live inline in profile.Settings now. Augment with
		// the SessionStart hook (which briefs the agent on /switch + the
		// available-profile list) and pass it to claude via a temp file.
		settingsPath := ""
		if augmented, err := runSettingsWithHook(p, ""); err == nil {
			settingsPath = augmented
			// Clear inline _settings on the in-memory profile so claudeFlags
			// picks up the file path and doesn't double-emit --settings.
			p.Settings = nil
		}

		claudeArgs := []string{"claude", "--strict-mcp-config", "--mcp-config", loc.JSONPath}
		claudeArgs = append(claudeArgs, claudeFlags(p, settingsPath)...)
		// Surface the profile name in the prompt box, terminal title, and
		// /resume picker so the user always knows which profile is active.
		claudeArgs = append(claudeArgs, "--name", loc.QualifiedID)
		if resumeID != "" {
			claudeArgs = append(claudeArgs, "--resume", resumeID)
			// On wrapper-attach to a /bg'd session, claude refuses plain
			// --resume because the supervisor still owns the session id. Fork
			// off a copy. We only do this on the first iteration of attach;
			// subsequent /switch relaunches target our own forked id, which
			// plain --resume handles fine.
			if skipAutoContinue {
				claudeArgs = append(claudeArgs, "--fork-session")
			}
		}
		// First launch: pass through caller-supplied trailing args (e.g. an
		// initial prompt). On relaunches after a /switch we instead inject a
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
		before := snapshotSessionFiles()

		cmd := exec.Command(binary, claudeArgs[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Marker env var so the /switch slash command can refuse to kill claude
		// when invoked outside our wrapper (plain claude sessions inherit the
		// command file from ~/.claude/commands/ but not this env var).
		cmd.Env = append(os.Environ(), "CLAUDE_PROFILES_RUN=1")

		// Poll the project sessions dir while claude is running so the hub's
		// "attach" prompt can offer a session id even during the first
		// iteration — without polling, SessionID stays empty until claude
		// exits, which means no attach is offered for a freshly-started
		// wrapper.
		pollDone := make(chan struct{})
		go pollForSessionID(before, wrapper, pollDone)

		_ = cmd.Run() // claude's exit code isn't meaningful for our purpose
		close(pollDone)

		raw, hasMarker := consumeNextProfileMarker()
		if !hasMarker {
			return // user /exit'd or claude crashed without a queued switch
		}

		next := raw
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

		after := snapshotSessionFiles()
		if id := findNewOrUpdatedSession(before, after); id != "" {
			resumeID = id
		}
		profile = next
		fmt.Fprintln(os.Stderr)
	}
}

// snapshotSessionFiles records mtimes of every .jsonl session file in the
// project dir for the current cwd. Used as a "before" snapshot so we can find
// the session this claude invocation creates.
func snapshotSessionFiles() map[string]int64 {
	dir := projectSessionsDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, _ := e.Info()
		out[e.Name()] = info.ModTime().UnixNano()
	}
	return out
}

// findNewOrUpdatedSession returns the session ID of the most-recently-modified
// .jsonl that is either new or has a newer mtime than the "before" snapshot.
// Returns "" if nothing matches.
func findNewOrUpdatedSession(before, after map[string]int64) string {
	var newestName string
	var newestMtime int64
	for name, mtime := range after {
		prior, existed := before[name]
		if existed && mtime <= prior {
			continue
		}
		if mtime > newestMtime {
			newestMtime = mtime
			newestName = name
		}
	}
	return strings.TrimSuffix(newestName, ".jsonl")
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

// consumeNextProfileMarker reads and removes the /switch handoff marker.
// Returns (raw, exists). "exists" is true when the marker file was present —
// even if its content was empty (empty == "open the picker"). When false the
// loop exits.
func consumeNextProfileMarker() (string, bool) {
	p := nextMarkerPath()
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	_ = os.Remove(p)
	return strings.TrimSpace(string(data)), true
}

// ensureSwitchSlashCommand installs ~/.claude/commands/switch.md and keeps it
// up to date. The commands directory belongs to Claude Code (not us), so we
// don't move it into ~/.claude-profiles/. We rewrite only when the file
// content has actually changed.
func ensureSwitchSlashCommand() error {
	dir := filepath.Join(claudeRootDirPath(), "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "switch.md")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == switchSlashCommand {
		return nil
	}
	return os.WriteFile(path, []byte(switchSlashCommand), 0o644)
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
			id := findNewOrUpdatedSession(before, snapshotSessionFiles())
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

func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ── SessionStart hook ────────────────────────────────────────────────────────
//
// Claude Code's SessionStart hook can inject text into a fresh session's
// context via `hookSpecificOutput.additionalContext`. We use that to brief
// the agent on which profiles exist (so /switch can take free-form intent
// and the agent can classify) and on the exact bash command to write the
// marker file when the user requests a switch.

const sessionStartHookHeader = `# claude-profiles: in-session profile switching

You are running inside the ` + "`claude-profiles run`" + ` wrapper. The user can switch profiles mid-session via the ` + "`/switch`" + ` slash command. When they do, write the chosen profile id to ~/.claude-profiles/next-marker and end this session; the wrapper will relaunch claude with that profile and resume the conversation.

Protocol when ` + "`/switch <argument>`" + ` is invoked:
1. If <argument> is empty, write an empty marker and exit — the wrapper will show its picker.
2. If <argument> exactly matches one of the profile ids listed below, write that id.
3. Otherwise treat <argument> as free-form intent and pick the profile from the list whose description best matches. If two are roughly tied or none is a clear fit, ask the user ONE short clarifying question before deciding.
4. To execute, use the Bash tool to run EXACTLY (the env guard refuses to kill the session if /switch is invoked outside the wrapper):
   if [ -z "$CLAUDE_PROFILES_RUN" ]; then echo "/switch only works inside 'claude-profiles run' wrapper" >&2; exit 1; fi; mkdir -p "$HOME/.claude-profiles" && printf "%s" "<chosen-profile-id>" > "$HOME/.claude-profiles/next-marker" && kill $PPID

Acknowledge the switch briefly to the user (which profile and why) before running the command.

Available profiles:
`

func cmdHookSessionStart() {
	var sb strings.Builder
	sb.WriteString(sessionStartHookHeader)

	locs, err := listAllLocations()
	if err == nil {
		for _, loc := range locs {
			p, _ := loadProfileAt(loc.JSONPath)
			desc := ""
			if p != nil && strings.TrimSpace(p.Description) != "" {
				desc = " — " + strings.TrimSpace(p.Description)
			}
			sb.WriteString(fmt.Sprintf("- %s%s\n", loc.QualifiedID, desc))
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

// runSettingsWithHook builds a settings.json file that combines the profile's
// own settings with our SessionStart hook entry, and returns the path. The
// merge appends our hook to any existing SessionStart hooks rather than
// replacing them.
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

	// Resolve "claude-profiles" via PATH at hook-fire time rather than baking
	// in an absolute path — keeps the hook working across reinstalls / moves.
	hookCmd := "claude-profiles _hook-session-start"

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	existingRaw, _ := hooks["SessionStart"].([]any)
	hooks["SessionStart"] = append(existingRaw, map[string]any{
		"hooks": []map[string]any{
			{"type": "command", "command": hookCmd},
		},
	})
	settings["hooks"] = hooks

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
