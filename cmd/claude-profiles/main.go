package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

// version is injected by -ldflags "-X main.version=v1.2.3" at release build
// time. When that is absent (e.g. go install), the init() below fills it from
// the module version embedded by the Go toolchain in the binary's build info.
var version = "dev"

func init() {
	if version != "dev" {
		return // already set by ldflags
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

func main() {
	args := os.Args[1:]
	// Strip --no-tmux globally: honour it via the env var that
	// bootstrapTmuxIfNeeded already checks so it works for every subcommand.
	var filtered []string
	for _, a := range args {
		if a == "--no-tmux" {
			os.Setenv("CLAUDE_PROFILES_NO_TMUX", "1")
		} else {
			filtered = append(filtered, a)
		}
	}
	args = filtered
	// Completion feed runs before migration / auto-sync: TAB-driven invocations
	// should stay cheap and side-effect-free.
	if len(args) > 0 && args[0] == "_complete" {
		cmdComplete(args[1:])
		return
	}
	// Port any state written by older versions of the CLI to the new
	// ~/.claude-profiles/ root + unified profile format. Idempotent.
	migrateLegacyLayout()
	// Fire background sync for every registered repo whose lastSync is stale.
	// Runs ahead of any command so repo profiles seen by this invocation come
	// from the freshest local cache. The sync itself completes async.
	kickAutoSync()
	// Check for a newer release and install it in the background. No-op for
	// dev builds or when CLAUDE_PROFILES_NO_UPDATE is set. The update lands
	// at the same binary path so the next invocation picks it up automatically.
	kickAutoUpdate()

	if len(args) == 0 {
		cmdInteractive()
		return
	}
	switch args[0] {
	case "launch":
		cmdRun(args[1:])
	case "list", "ls":
		cmdList()
	case "new", "create":
		cmdNew()
	case "ask":
		cmdAsk(strings.Join(args[1:], " "))
	case "run":
		cmdRun(args[1:])
	case "exec":
		cmdExec(args[1:])
	case "show":
		cmdShow(args[1:])
	case "_hook-session-start":
		cmdHookSessionStart()
	case "_hook-prompt-submit":
		cmdHookPromptSubmit()
	case "_hook-worktree-caches":
		cmdHookWorktreeCaches()
	case "_hook-worktree-branch":
		cmdHookWorktreeBranch()
	case "_hook-guard-worktree-writes":
		cmdHookGuardWorktreeWrites()
	case "_hook-stop":
		cmdHookStop()
	case "_delegate-runner":
		cmdDelegateRunner(args[1:])
	case "doctor":
		cmdDoctor()
	case "analytics", "stats":
		cmdAnalytics(args[1:])
	case "probe":
		cmdProbe(args[1:])
	case "edit":
		cmdEdit(args[1:])
	case "delete", "rm":
		cmdDelete(args[1:])
	case "export":
		cmdExport(args[1:])
	case "import":
		cmdImport(args[1:])
	case "repo":
		cmdRepo(args[1:])
	case "copy", "cp":
		cmdCopy(args[1:])
	case "help", "--help", "-h":
		usage()
	case "update":
		cmdUpdate()
	case "version", "--version":
		fmt.Println(version)
	case "completion":
		cmdCompletion(args[1:])
	default:
		// Treat as profile name shorthand (supports "alias/name" too)
		cmdRun(args)
	}
}

// ── interactive hub ───────────────────────────────────────────────────────────
//
// The hub displays the profile list as the primary view. Highlighted-profile
// actions are dispatched via single-key shortcuts (see hub.go).

func cmdInteractive() {
	// Bootstrap tmux around the hub so:
	//   - the hub picker has its own pane/tab (on iTerm2 -CC: a native tab)
	//   - launched profiles open in SEPARATE tmux windows (see actLaunch),
	//     so the hub tab survives across profile launches
	//   - /delegate has somewhere to drop windows
	bootstrapTmuxIfNeeded(tmuxSessionName(nil), nil)

	hubMode = true
	defer func() { hubMode = false }()

	for {
		r := runHub()
		if r.action == actQuit || r.action == "" {
			return
		}
		runHubAction(r)
	}
}

func runHubAction(r hubResult) {
	// Clear the normal screen each time the hub hands off to an action so that
	// info lines and sub-prompts from previous invocations don't accumulate
	// when the hub re-enters alt-screen and the user triggers the same action
	// again.
	if isTTY() {
		fmt.Fprint(os.Stderr, "\033[2J\033[H")
	}
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if rec != errHubBack {
			panic(rec)
		}
		if isTTY() {
			fmt.Fprint(os.Stderr, "\033[2J\033[H")
		}
	}()
	switch r.action {
	case actLaunch:
		// Indicators + workaround for the supervisor's /bg respawnFlags=[] bug:
		// instead of `claude attach` (which would respawn under empty flags
		// and lose the profile), we offer to `claude --resume <id>` via the
		// wrapper itself, reapplying every flag. The supervisor's /bg'd
		// worker is left orphaned but the conversation continues with full
		// profile intact.
		bgs := backgroundedByProfile()[r.profile]
		runs := runningByProfile()[r.profile]
		for _, w := range runs {
			info("· %s has a foreground wrapper (PID %d, cwd %s, started %s)",
				r.profile, w.PID, w.Cwd, time.Unix(w.StartedAt, 0).Format("15:04:05"))
		}

		// For worktree-enabled profiles, show a dedicated worktree picker that
		// lists existing worktrees (annotated with bg/live state) alongside a
		// "[+ new worktree]" option. This replaces the bg-session picker so the
		// user always resumes in the correct worktree context.
		var isWorktreeProfile bool
		if loc, err := resolveProfileLocation(r.profile); err == nil {
			if p, _ := loadProfileAt(loc.JSONPath); p != nil {
				isWorktreeProfile = p.Worktree
			}
		}
		if isWorktreeProfile {
			worktrees := listExistingWorktrees()
			if len(worktrees) > 0 {
				runningIDs := make(map[string]bool, len(runs))
				for _, w := range runs {
					if w.SessionID != "" {
						runningIDs[w.SessionID] = true
					}
				}
				choice := pickWorktreeOrNew(r.profile, worktrees, bgs, runningIDs)
				if choice == nil {
					return // user cancelled
				}
				if !choice.isNew {
					launchInExistingWorktree(r.profile, choice.worktree)
					return
				}
				// User chose "[+ new worktree]" — skip bg picker, launch fresh
				if len(runs) > 0 && !confirm("Start a second instance in this terminal?") {
					return
				}
				var extra []string
				if r.prompt != "" {
					extra = append(extra, r.prompt)
				}
				launchFromHub(r.profile, extra)
				return
			}
			// No existing worktrees — fall through to bg picker (handles orphaned
			// bg sessions whose worktrees were pruned) and then fresh launch.
		}

		if len(bgs) > 0 {
			var chosen *BackgroundedSession
			if len(bgs) == 1 {
				bs := bgs[0]
				hint := ""
				if bs.Hint != "" {
					hint = fmt.Sprintf(" — %q", bs.Hint)
				}
				info("· %s has a backgrounded session %s (cwd %s, started %s%s)",
					r.profile, shortSession(bs.SessionID), bs.Cwd, bs.StartedAt.Format("15:04:05"), hint)
				if confirm("Resume that conversation via the wrapper (full profile reapplied)?") {
					chosen = &bs
				}
			} else {
				info("· %s has %d backgrounded sessions — pick one to resume:", r.profile, len(bgs))
				chosen = pickBackgroundedSession(r.profile, bgs)
			}
			if chosen != nil {
				launchFromHub(r.profile, []string{"--resume", chosen.SessionID})
				return
			}
		}
		if len(runs) > 0 && !confirm("Start a second instance in this terminal?") {
			return
		}
		var extra []string
		if r.prompt != "" {
			extra = append(extra, r.prompt)
		}
		launchFromHub(r.profile, extra)
	case actPin:
		pins := loadPins()
		if _, ok := findPin(pins, r.profile); ok {
			pins = removePin(pins, r.profile)
			_ = savePins(pins)
			return
		}
		loc, err := resolveProfileLocation(r.profile)
		if err != nil {
			fatal(err)
		}
		p, _ := loadProfileAt(loc.JSONPath)
		promptName := ""
		if len(p.Prompts) > 0 {
			if name, err := pickPinPromptName(p.Prompts); err == nil {
				promptName = name
			}
		}
		pins = addPin(pins, PinEntry{ProfileID: r.profile, PromptName: promptName})
		_ = savePins(pins)
	case actAsk:
		cmdAsk(r.prompt) // may syscall.Exec
	case actNew:
		cmdNew()
	case actEdit:
		cmdEdit([]string{r.profile})
	case actDelete:
		cmdDelete([]string{r.profile})
	case actCopy:
		cmdCopy([]string{r.profile})
	case actExport:
		cmdExport([]string{r.profile})
	case actImport:
		cmdImport(nil)
	case actRepo:
		cmdRepoHub()
	case actAnalytics:
		cmdAnalytics(nil)
		fmt.Fprintln(os.Stderr, "Press Enter to return to the menu...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
	}
}

// ── list ──────────────────────────────────────────────────────────────────────

func cmdList() {
	locs, err := listAllLocations()
	if err != nil {
		fatal(err)
	}
	if len(locs) == 0 {
		fmt.Println("No profiles. Run: claude-profiles new")
		return
	}
	for _, loc := range locs {
		p, err := loadProfileAt(loc.JSONPath)
		var servers string
		var tags []string
		var description string
		if err == nil {
			description = p.Description
			var snames []string
			for k := range p.McpServers {
				snames = append(snames, k)
			}
			servers = strings.Join(snames, ", ")
			if len(p.DeniedTools) > 0 {
				tags = append(tags, fmt.Sprintf("deny:%d", len(p.DeniedTools)))
			}
			if p.Isolated {
				tags = append(tags, "isolated")
			}
			if p.Worktree {
				tags = append(tags, "worktree")
			}
			if strings.EqualFold(p.Distill, "on") {
				tags = append(tags, "distill")
			}
			if p.Cwd != "" {
				tags = append(tags, "cwd")
			}
			if kinds := profilePluginKinds(loc); len(kinds) > 0 {
				tags = append(tags, "+"+strings.Join(kinds, "/"))
			}
			// Surface the most useful settings inline so users don't need to open the file
			s := parseSettings(p.Settings)
			if m := getModel(s); m != "" {
				tags = append(tags, "model:"+m)
			}
			if pm := getPermissionMode(s); pm != "" {
				tags = append(tags, "mode:"+pm)
			}
			if len(s) > 0 {
				// Count keys other than model + permissions to indicate extra settings
				extra := 0
				for k := range s {
					if k != "model" && k != "permissions" {
						extra++
					}
				}
				if extra > 0 {
					tags = append(tags, fmt.Sprintf("+%d settings", extra))
				}
			}
		}
		source := "[local]"
		switch {
		case loc.RepoAlias == ".":
			source = "[project]"
		case loc.RepoAlias != "":
			source = "[repo:" + loc.RepoAlias + "]"
		}
		var extras string
		if len(tags) > 0 {
			extras = " [" + strings.Join(tags, " ") + "]"
		}
		fmt.Printf("%-30s %-10s %s%s\n", loc.QualifiedID, source, servers, extras)
		if description != "" {
			fmt.Printf("%-30s   — %s\n", "", description)
		}
	}
}

// ── new ───────────────────────────────────────────────────────────────────────

func cmdNew() {
	fmt.Fprintln(os.Stderr)
	title("=== New MCP Profile ===")
	fmt.Fprintln(os.Stderr)

	name := strings.ReplaceAll(prompt("Profile name"), " ", "-")
	if name == "" {
		fatal(fmt.Errorf("name required"))
	}

	scope := pickScope() // "user" or "project"

	if scope == "user" && profileExists(name) {
		fatal(fmt.Errorf("profile %q already exists", name))
	}

	description := prompt("Description (purpose of this profile, optional)")

	p := &Profile{
		Description: strings.TrimSpace(description),
		McpServers:  map[string]ServerConfig{},
	}

	for {
		fmt.Fprintln(os.Stderr)
		sname := prompt("Add server name (empty to finish)")
		if sname == "" {
			break
		}
		addServer(p, sname)
	}

	configureSettings(p)

	var savedPath string
	if scope == "project" {
		var err error
		savedPath, err = saveProjectProfile(name, p)
		if err != nil {
			fatal(err)
		}
	} else {
		if err := saveProfile(name, p); err != nil {
			fatal(err)
		}
		savedPath = profilePath(name)
	}
	fmt.Fprintln(os.Stderr)
	success("Profile %q saved → %s", name, savedPath)

	if confirm("Launch now?") {
		launchFromHub(name, nil)
	}
}

// launchInExistingWorktree opens an existing git worktree in a new tmux window
// (or in-process when tmux is unavailable), resuming its last session if one
// exists. The window cds into the worktree path and runs with
// CLAUDE_PROFILES_WORKTREE_WINDOW=1 so the wrapper skips opening yet another
// window. Because the cwd is a linked worktree, isLinkedWorktree() in cmdRun
// suppresses the --worktree flag — claude runs in-place rather than creating
// a new worktree.
func launchInExistingWorktree(profile string, wt *WorktreeInfo) {
	var extra []string
	if wt.LastSessionID != "" {
		extra = append(extra, "--resume", wt.LastSessionID)
	}

	self, err := os.Executable()
	if err != nil || self == "" {
		warn("cannot locate own binary; launching in-place")
		if err2 := os.Chdir(wt.Path); err2 == nil {
			cmdRun(append([]string{profile}, extra...))
		}
		return
	}

	parts := []string{shellQuote(self), "run", shellQuote(profile)}
	for _, a := range extra {
		parts = append(parts, shellQuote(a))
	}
	innerCmd := fmt.Sprintf("cd %s && CLAUDE_PROFILES_WORKTREE_WINDOW=1 %s",
		shellQuote(wt.Path), strings.Join(parts, " "))

	// Append worktree slug to window name to distinguish multiple worktrees
	// of the same profile from one another.
	windowName := strings.NewReplacer("/", "-", ".", "-", ":", "-").Replace(profile) + "-" + wt.Name

	if os.Getenv("TMUX") == "" {
		if err2 := os.Chdir(wt.Path); err2 == nil {
			cmdRun(append([]string{profile}, extra...))
		}
		return
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		if err2 := os.Chdir(wt.Path); err2 == nil {
			cmdRun(append([]string{profile}, extra...))
		}
		return
	}
	if err := exec.Command(tmuxBin, "new-window", "-n", windowName, innerCmd).Run(); err != nil {
		warn("Failed to open tmux window for worktree %s: %v — trying in-place", wt.Name, err)
		if err2 := os.Chdir(wt.Path); err2 == nil {
			cmdRun(append([]string{profile}, extra...))
		}
	}
}

// launchFromHub spawns `claude-profiles run <profile> [extra...]` in a fresh
// tmux window so the hub picker (running in its own window) stays alive.
// Outside tmux it falls back to in-process cmdRun, which then bootstraps its
// own tmux session.
func launchFromHub(profile string, extra []string) {
	if os.Getenv("TMUX") == "" {
		args := append([]string{profile}, extra...)
		cmdRun(args)
		return
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		args := append([]string{profile}, extra...)
		cmdRun(args)
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		warn("cannot locate own binary; launching in-place")
		args := append([]string{profile}, extra...)
		cmdRun(args)
		return
	}
	parts := []string{shellQuote(self), "run", shellQuote(profile)}
	for _, a := range extra {
		parts = append(parts, shellQuote(a))
	}
	windowName := strings.NewReplacer("/", "-", ".", "-", ":", "-").Replace(profile)
	if err := exec.Command(tmuxBin, "new-window", "-n", windowName, strings.Join(parts, " ")).Run(); err != nil {
		warn("Failed to open new tmux window for profile: %v — falling back to in-place launch", err)
		args := append([]string{profile}, extra...)
		cmdRun(args)
	}
}

func addServer(p *Profile, sname string) {
	t := pickServerType()
	var cfg ServerConfig

	if t == "stdio" {
		cmd := prompt("Command")
		argsStr := prompt("Args (space-separated, or empty)")
		var args []string
		if strings.TrimSpace(argsStr) != "" {
			args = strings.Fields(argsStr)
		}
		cfg = ServerConfig{Type: "stdio", Command: cmd, Args: args}
	} else {
		u := prompt("URL")
		cfg = ServerConfig{Type: "http", URL: u}
	}

	p.McpServers[sname] = cfg
	success("  + Added: %s", sname)

	probeAndFilter(p, sname, cfg)
}

func probeAndFilter(p *Profile, sname string, cfg ServerConfig) {
	fmt.Fprintf(os.Stderr, "  Fetching available tools from %q...\n", sname)

	tools, err := FetchTools(cfg, sname)
	if err != nil {
		if errors.Is(err, errNeedsAuth) {
			fmt.Fprintf(os.Stderr, "  (Could not authenticate — skipping filter setup)\n")
		} else {
			fmt.Fprintf(os.Stderr, "  (Could not reach server — skipping filter setup)\n")
		}
		return
	}

	fmt.Fprintf(os.Stderr, "  %d tool(s) found.\n", len(tools))
	selectToolFilter(p, sname, tools)
}

// ── edit ──────────────────────────────────────────────────────────────────────

func cmdEdit(args []string) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	if arg == "" {
		picked, err := pickProfile()
		if err != nil {
			fatal(err)
		}
		arg = picked
	}
	loc, err := resolveProfileLocation(arg)
	if err != nil {
		fatal(err)
	}
	// Non-TTY fallback: open the profile folder directly in $EDITOR.
	if !isTTY() {
		openInEditor(filepath.Dir(loc.JSONPath))
		return
	}
	runEditMenu(*loc)
}

func openInEditor(path string) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// editRepoProfileSettings opens a temp JSON file containing the profile's
// current user-settings override, lets the user edit it in $EDITOR, then
// reads it back and saves it via saveFn. Used for repo/project profiles whose
// source files must not be modified.
func editRepoProfileSettings(p *Profile, saveFn func(*Profile) error) {
	content := p.Settings
	if len(content) == 0 {
		content = json.RawMessage("{}\n")
	}
	f, err := os.CreateTemp("", "claude-profiles-settings-*.json")
	if err != nil {
		warn("Could not create temp file: %v", err)
		return
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	if _, err := f.Write(content); err != nil {
		f.Close()
		return
	}
	f.Close()
	openInEditor(tmpName)
	data, err := os.ReadFile(tmpName)
	if err != nil {
		warn("Could not read edited settings: %v", err)
		return
	}
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		warn("Settings not saved — invalid JSON: %v", err)
		return
	}
	if len(check) == 0 {
		p.Settings = nil
	} else {
		p.Settings = json.RawMessage(data)
	}
	if err := saveFn(p); err != nil {
		fatal(err)
	}
}

// runEditMenu drives the interactive edit flow: per-server allow/deny tooling,
// session settings, raw $EDITOR escape hatch. Loops until the user picks Done
// (or aborts with Esc). State is reloaded from disk each iteration so the
// $EDITOR branch picks up.
func runEditMenu(loc ProfileLocation) {
	dir := filepath.Dir(loc.JSONPath)
	// Project and repo profiles: only write user prefs (isolated, prompts,
	// settings) — don't touch the source files which are either git-tracked or
	// a sync cache.
	var saveFn func(*Profile) error
	if loc.RepoAlias != "" {
		saveFn = func(p *Profile) error {
			existingPrefs := loadProfilePrefs(dir)
			return saveProfilePrefs(dir, ProfilePrefs{
				Description: p.Description,
				Isolated:    p.Isolated,
				Disabled:    existingPrefs.Disabled,
				Worktree:    p.Worktree,
				Prompts:     p.Prompts,
				Cwd:         p.Cwd,
				Settings:    p.Settings,
				Distill:     p.Distill,
			})
		}
	} else {
		saveFn = func(p *Profile) error { return saveProfileAt(dir, p) }
	}
	for {
		p, err := loadProfileAt(loc.JSONPath)
		if err != nil {
			fatal(err)
		}
		action := pickEditAction(loc, p)
		switch action {
		case "tools":
			manageToolFilters(p, loc)
			if err := saveProfileAt(dir, p); err != nil {
				fatal(err)
			}
		case "settings":
			configureSettings(p)
			if loc.RepoAlias != "" {
				if err := saveFn(p); err != nil {
					fatal(err)
				}
			} else {
				if err := saveProfileAt(dir, p); err != nil {
					fatal(err)
				}
			}
		case "isolated":
			p.Isolated = !p.Isolated
			if err := saveFn(p); err != nil {
				fatal(err)
			}
			state := "off"
			if p.Isolated {
				state = "on"
			}
			info("Isolated mode is now %s.", state)
		case "worktree":
			p.Worktree = !p.Worktree
			if err := saveFn(p); err != nil {
				fatal(err)
			}
			state := "off"
			if p.Worktree {
				state = "on"
			}
			info("Worktree mode is now %s.", state)
		case "distill":
			if strings.EqualFold(p.Distill, "on") {
				p.Distill = "off"
			} else {
				p.Distill = "on"
			}
			if err := saveFn(p); err != nil {
				fatal(err)
			}
			info("Session distillation is now %s.", p.Distill)
		case "prompts":
			managePrompts(p, loc, saveFn)
		case "plugin":
			manageProfilePlugin(loc)
		case "editor":
			if loc.RepoAlias != "" {
				editRepoProfileSettings(p, saveFn)
			} else {
				openInEditor(dir)
			}
		case "done", "":
			return
		}
	}
}

// ── probe ─────────────────────────────────────────────────────────────────────

// cmdProbe runs FetchTools against one or all MCP servers in a profile and
// prints the raw error verbatim — useful when the edit menu collapses a
// failure to "Could not reach …".
func cmdProbe(args []string) {
	if len(args) == 0 {
		picked, err := pickProfile()
		if err != nil {
			fatal(err)
		}
		args = []string{picked}
	}
	loc, err := resolveProfileLocation(args[0])
	if err != nil {
		fatal(err)
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		fatal(err)
	}
	if len(p.McpServers) == 0 {
		info("Profile %s has no MCP servers.", loc.QualifiedID)
		return
	}

	targets := []string{}
	if len(args) > 1 {
		if _, ok := p.McpServers[args[1]]; !ok {
			known := make([]string, 0, len(p.McpServers))
			for k := range p.McpServers {
				known = append(known, k)
			}
			sort.Strings(known)
			fatal(fmt.Errorf("server %q not found in %s — servers: %s",
				args[1], loc.QualifiedID, strings.Join(known, ", ")))
		}
		targets = append(targets, args[1])
	} else {
		for k := range p.McpServers {
			targets = append(targets, k)
		}
		sort.Strings(targets)
	}

	for _, sname := range targets {
		probeOne(loc.QualifiedID, sname, p.McpServers[sname])
	}
}

func probeOne(profile, sname string, cfg ServerConfig) {
	fmt.Fprintln(os.Stderr)
	title("probe %s · %s", profile, sname)
	t := cfg.Type
	if t == "" {
		t = "http"
	}
	info("  type:    %s", t)
	switch t {
	case "stdio":
		info("  command: %s %s", cfg.Command, strings.Join(cfg.Args, " "))
	default:
		info("  url:     %s", cfg.URL)
		if loadToken(cfg.URL) != "" {
			info("  token:   cached (Bearer …)")
		} else {
			info("  token:   none cached — OAuth will trigger on 401")
		}
	}

	start := time.Now()
	tools, err := FetchTools(cfg, sname)
	elapsed := time.Since(start).Round(time.Millisecond)
	info("  elapsed: %s", elapsed)

	if err != nil {
		switch {
		case errors.Is(err, errNeedsAuth):
			warn("  result:  AUTH REQUIRED")
		default:
			warn("  result:  FAILED")
		}
		fmt.Fprintf(os.Stderr, "  error:   %v\n", err)
		return
	}
	success("  result:  OK — %d tool(s)", len(tools))
	for _, tt := range tools {
		marker := ""
		if tt.ReadOnlyHint {
			marker = " " + styleReadOnly.Render("[R]")
		}
		fmt.Fprintf(os.Stderr, "    · %s%s\n", shortToolName(tt.Name), marker)
	}
}

// ── delete ────────────────────────────────────────────────────────────────────

func cmdDelete(args []string) {
	var arg string
	if len(args) > 0 {
		arg = args[0]
	} else {
		var err error
		arg, err = pickProfile()
		if err != nil {
			fatal(err)
		}
	}
	loc, err := resolveProfileLocation(arg)
	if err != nil {
		fatal(err)
	}
	if loc.RepoAlias == "." {
		fatal(fmt.Errorf("project profiles can't be deleted from here — remove .claude-profiles/%s/ from your repo", loc.Name))
	}
	if loc.RepoAlias != "" {
		fatal(fmt.Errorf("repo profiles can't be deleted from here — manage them in the source repo"))
	}
	if !confirm(fmt.Sprintf("Delete %q?", loc.QualifiedID)) {
		return
	}
	dir := filepath.Dir(loc.JSONPath)
	os.RemoveAll(dir)
	_ = deleteProfilePrefs(dir)
	success("Deleted.")
}

// ── export ────────────────────────────────────────────────────────────────────

func cmdExport(args []string) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	name, err := resolveProfile(arg)
	if err != nil {
		fatal(err)
	}
	p, err := loadProfile(name)
	if err != nil {
		fatal(err)
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(data))
}

// ── import ────────────────────────────────────────────────────────────────────

func cmdImport(args []string) {
	var src string
	if len(args) > 0 {
		src = args[0]
	} else {
		src = prompt("File path to import")
		if src == "" {
			fatal(fmt.Errorf("file path required"))
		}
		src = expandPath(src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		fatal(err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		fatal(fmt.Errorf("invalid profile JSON: %v", err))
	}

	defaultName := strings.TrimSuffix(fileBase(src), ".json")
	name := strings.ReplaceAll(promptWithDefault("Profile name", defaultName), " ", "-")
	if name == "" {
		name = defaultName
	}

	if profileExists(name) {
		if !confirm(fmt.Sprintf("Profile %q already exists. Overwrite?", name)) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return
		}
	}

	if err := saveProfile(name, &p); err != nil {
		fatal(err)
	}
	success("Imported %q → %s", name, profilePath(name))
}

// ── repo subcommands ──────────────────────────────────────────────────────────

func cmdRepo(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo {add|list|remove|sync} ..."))
	}
	switch args[0] {
	case "add":
		cmdRepoAdd(args[1:])
	case "list", "ls":
		cmdRepoList()
	case "remove", "rm":
		cmdRepoRemove(args[1:])
	case "sync":
		cmdRepoSync(args[1:])
	default:
		fatal(fmt.Errorf("unknown repo subcommand: %s", args[0]))
	}
}

func addRepo(url, alias, branch string) error {
	if alias == "" {
		alias = defaultAlias(url)
	}
	cfg, err := loadReposConfig()
	if err != nil {
		return err
	}
	if findRepo(cfg, alias) != nil {
		return fmt.Errorf("alias already registered: %s", alias)
	}
	if findRepo(cfg, url) != nil {
		return fmt.Errorf("URL already registered: %s", url)
	}
	r := RepoConfig{URL: url, Alias: alias, Branch: branch}
	fmt.Fprintf(os.Stderr, "Cloning %s → %s\n", url, repoCachePath(url))
	if err := cloneRepo(r); err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}
	r.LastSync = time.Now().Unix()
	r.LastSyncOK = true
	cfg.Repos = append(cfg.Repos, r)
	if err := saveReposConfig(cfg); err != nil {
		return err
	}
	success("Registered repo %q (alias: %s)", url, alias)
	return nil
}

func cmdRepoAdd(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo add <git-url> [--branch X] [--alias Y]"))
	}
	url := args[0]
	branch := ""
	alias := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--branch":
			if i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		case "--alias":
			if i+1 < len(args) {
				alias = args[i+1]
				i++
			}
		}
	}
	if err := addRepo(url, alias, branch); err != nil {
		fatal(err)
	}
}

func cmdRepoAddInteractive() {
	var url, alias, branch string
	err := runForm(
		huh.NewInput().
			Title("Git URL").
			Placeholder("https://github.com/owner/repo").
			Value(&url).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("URL is required")
				}
				return nil
			}),
		huh.NewInput().
			Title("Alias (optional)").
			Description("Short name for this repo. Leave empty to use the default derived from the URL.").
			Value(&alias),
		huh.NewInput().
			Title("Branch (optional)").
			Description("Leave empty to use the default branch.").
			Value(&branch),
	)
	if errors.Is(err, huh.ErrUserAborted) {
		return
	}
	if err != nil {
		fatal(err)
	}
	url = strings.TrimSpace(url)
	alias = strings.TrimSpace(alias)
	branch = strings.TrimSpace(branch)
	if err := addRepo(url, alias, branch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "\nPress Enter to continue...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func cmdRepoHub() {
	for {
		var action string
		err := runFieldBack(huh.NewSelect[string]().
			Title("Manage repos").
			Options(
				huh.NewOption("Add repo", "add"),
				huh.NewOption("List repos", "list"),
			).
			Value(&action))
		if errors.Is(err, huh.ErrUserAborted) {
			return
		}
		switch action {
		case "add":
			cmdRepoAddInteractive()
		case "list":
			cmdRepoList()
			fmt.Fprintln(os.Stderr, "\nPress Enter to continue...")
			bufio.NewReader(os.Stdin).ReadBytes('\n')
		}
	}
}

func cmdRepoList() {
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	if len(cfg.Repos) == 0 {
		fmt.Println("No repos registered. Run: claude-profiles repo add <url>")
		return
	}
	for _, r := range cfg.Repos {
		branch := r.Branch
		if branch == "" {
			branch = "<default>"
		}
		fmt.Printf("%-16s %-50s branch=%s  %s\n", r.Alias, r.URL, branch, r.syncStatus())
	}
}

func cmdRepoRemove(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo remove <alias-or-url>"))
	}
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	target := findRepo(cfg, args[0])
	if target == nil {
		fatal(fmt.Errorf("repo not found: %s", args[0]))
	}
	if !confirm(fmt.Sprintf("Remove repo %q and delete its cache?", target.Alias)) {
		return
	}
	cachePath := repoCachePath(target.URL)
	var newRepos []RepoConfig
	for _, r := range cfg.Repos {
		if r.Alias != target.Alias {
			newRepos = append(newRepos, r)
		}
	}
	cfg.Repos = newRepos
	if err := saveReposConfig(cfg); err != nil {
		fatal(err)
	}
	if err := os.RemoveAll(cachePath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove cache: %v\n", err)
	}
	success("Removed repo %q", target.Alias)
}

func cmdRepoSync(args []string) {
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(os.Stderr, "No repos registered.")
		return
	}
	// Sync targets: all, or just the named one
	for i := range cfg.Repos {
		r := &cfg.Repos[i]
		if len(args) > 0 && r.Alias != args[0] && normaliseURL(r.URL) != normaliseURL(args[0]) {
			continue
		}
		fmt.Fprintf(os.Stderr, "Syncing %s...\n", r.Alias)
		if err := syncRepoForeground(r); err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
		} else {
			success("  + %s synced", r.Alias)
		}
	}
	saveReposConfig(cfg)
}

// ── copy ──────────────────────────────────────────────────────────────────────

func cmdCopy(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles copy <alias/profile> [<local-name>]"))
	}
	srcID := args[0]
	if !strings.Contains(srcID, "/") {
		fatal(fmt.Errorf("source must be a repo profile (alias/name); local profiles are already editable"))
	}
	loc, err := resolveProfileLocation(srcID)
	if err != nil {
		fatal(err)
	}

	dstName := loc.Name
	if len(args) >= 2 {
		dstName = args[1]
	} else {
		dstName = promptWithDefault("Local profile name", loc.Name)
	}
	dstName = strings.ReplaceAll(dstName, " ", "-")
	if profileExists(dstName) {
		if !confirm(fmt.Sprintf("Local profile %q exists. Overwrite?", dstName)) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return
		}
		os.RemoveAll(filepath.Join(profilesDir(), dstName))
	}

	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		fatal(err)
	}
	if err := saveProfile(dstName, p); err != nil {
		fatal(err)
	}
	success("Copied %s → %s (local)", srcID, dstName)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	if hubMode {
		fmt.Fprint(os.Stderr, "\nPress Enter to return to the menu...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		panic(errHubBack)
	}
	os.Exit(1)
}

func fileBase(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}

func usage() {
	fmt.Printf(`Usage: claude-profiles [command] [profile] [claude-args...]

Commands:
  (none)             Interactive hub: launch / new / edit / delete / export / import
  launch [name]      Launch claude with profile (local "name" or "repo-alias/name")
  list               List all profiles (local + repos)
  show [name]        Print a human-readable detail view of one profile:
                       source, MCP servers, deny-list, settings highlights
                       (model, permission mode, sandbox, env vars, plugins,
                       hooks, statusLine), flags, prompts, plugin content.
  new                Create a new profile interactively
  ask [prompt]       Classify a prompt to the best profile and launch it
  run <profile>      Launch claude in a wrapper loop. Inside the session,
                       /handoff <name> [keep|fresh] hands off to another
                       profile; bare /handoff opens the profile picker.
  exec <profile>     Replace this process with claude, applying the profile's
   [claude-args...]    flags. No wrapper loop, tmux, hooks, or pidfiles —
                       suited to CI and other non-interactive automation.
                       Args after the profile pass through to claude verbatim:
                         claude-profiles exec my-profile -p "do the thing"
  doctor             Run sanity checks (claude binary, hook resolution,
                       profile JSON, stale marker, session dir) and report.
  analytics          Show context window usage stats: peak context per session,
                       per-profile cache efficiency, per-project totals, and
                       recommendations for reducing context pressure.
  probe <profile>    Call FetchTools against each MCP server in the profile
   [server]            (or just <server>) and print the raw error/tool list.
  edit [name]        Edit a local profile: manage MCP tool allow/deny,
                       session settings, or open the JSON in $EDITOR.
  delete [name]      Delete a local profile (interactive if omitted)
  export [name]      Print profile JSON to stdout (interactive if omitted)
  import [file]      Import a profile JSON file (prompts if omitted)
  copy <alias/name>  Copy a repo profile to local for editing
                       [<local-name>]
  repo add <url>     Register a remote repo (auto-clones on add)
                       [--branch X] [--alias Y]
  repo list          Show registered repos and last sync status
  repo remove <id>   Unregister a repo (by alias or URL) and delete cache
  repo sync [id]     Sync repos now (foreground). Auto-sync runs every 5min.
  completion <shell> Emit a shell completion script (bash or zsh).
                       Wire up with: eval "$(claude-profiles completion zsh)"
  update             Check for a newer version and install it immediately.
                       Auto-update also runs in the background once per day.
                       Set CLAUDE_PROFILES_NO_UPDATE=1 to opt out.
  version            Print the binary version and exit
  help               Show this help

Flags (any position):
  --no-tmux          Skip tmux bootstrap even when tmux is installed.
                     Also hides /delegate (which requires tmux).
                     Equivalent to setting CLAUDE_PROFILES_NO_TMUX=1.

Profiles directory: %s
  Override the whole root with: CLAUDE_PROFILES_ROOT=/path claude-profiles

Direct launch shorthand: claude-profiles <profile-name> [claude-args...]
`, profilesDir())
}
