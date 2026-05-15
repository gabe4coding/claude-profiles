package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// claudeTheme paints huh forms with the Claude design system palette.
func claudeTheme() *huh.Theme {
	t := huh.ThemeBase()
	// Titles + descriptions
	t.Focused.Title = t.Focused.Title.Foreground(cdsCoral).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(cdsMuted)
	t.Blurred.Title = t.Blurred.Title.Foreground(cdsMuted)
	t.Blurred.Description = t.Blurred.Description.Foreground(cdsMuted)
	// Selected option indicator + text
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(cdsCoral)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(cdsCoral).Bold(true)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(cdsCoral)
	t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(cdsCoral)
	// Text input
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(cdsCoral)
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(cdsCoral)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(cdsMuted)
	// Confirm buttons
	t.Focused.FocusedButton = t.Focused.FocusedButton.
		Foreground(cdsCream).
		Background(cdsCoral).
		Bold(true)
	t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(cdsMuted)
	// Help footer
	t.Help.ShortKey = t.Help.ShortKey.Foreground(cdsCoral).Bold(true)
	t.Help.ShortDesc = t.Help.ShortDesc.Foreground(cdsMuted)
	t.Help.FullKey = t.Help.FullKey.Foreground(cdsCoral).Bold(true)
	t.Help.FullDesc = t.Help.FullDesc.Foreground(cdsMuted)
	return t
}

// runField wraps a huh Field in a Form configured with our preferred bindings:
//   - Esc and Ctrl+C both abort (return huh.ErrUserAborted)
//   - help footer visible (↑↓ navigate, enter select, esc back …)
//   - Claude design-system palette applied to the field's theme
func runField(field huh.Field) error {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(
		key.WithKeys("esc", "ctrl+c"),
		key.WithHelp("esc/ctrl+c", "back"),
	)
	return huh.NewForm(huh.NewGroup(field)).
		WithShowHelp(true).
		WithKeyMap(km).
		WithTheme(claudeTheme()).
		Run()
}

// runFieldBack is runField for sub-menus that want Esc to go back one level
// instead of unwinding all the way to the hub. Returns ErrUserAborted on Esc
// so the caller can early-return; everything else fatals as usual.
func runFieldBack(field huh.Field) error {
	err := runField(field)
	if errors.Is(err, huh.ErrUserAborted) {
		return err
	}
	if err != nil {
		fatal(err)
	}
	return nil
}

// ── Styling ───────────────────────────────────────────────────────────────────
//
// Built on the Claude design system palette in colors.go.

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(cdsCoral)
	styleSuccess  = lipgloss.NewStyle().Foreground(cdsSage)
	styleInfo     = lipgloss.NewStyle().Foreground(cdsMuted)
	styleReadOnly = lipgloss.NewStyle().Foreground(cdsSage)
	styleWarn     = lipgloss.NewStyle().Foreground(cdsAmber)
)

func info(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleInfo.Render(fmt.Sprintf(format, a...)))
}

func success(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleSuccess.Render(fmt.Sprintf(format, a...)))
}

func warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleWarn.Render(fmt.Sprintf(format, a...)))
}

func title(format string, a ...any) {
	fmt.Fprintln(os.Stderr, styleTitle.Render(fmt.Sprintf(format, a...)))
}

// isTTY reports whether stdin is a terminal — huh requires it.
func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// hubMode is true while we're running the interactive hub. When set, an abort
// from any sub-flow (Esc/Ctrl+C inside huh) panics with errHubBack instead of
// exiting; the hub loop recovers and returns to the action menu.
var hubMode bool

// errHubBack is recovered by the hub loop to navigate back to the action menu.
var errHubBack = fmt.Errorf("hub: back")

// handleAbort: in hub mode, panics so the loop can recover and re-show the
// menu; otherwise exits the process cleanly with code 130.
func handleAbort(err error) {
	if errors.Is(err, huh.ErrUserAborted) {
		if hubMode {
			panic(errHubBack)
		}
		fmt.Fprintln(os.Stderr, "\nAborted.")
		os.Exit(130)
	}
}

// ── Line-based fallbacks (for piped stdin) ────────────────────────────────────

var stdinReader = bufio.NewReader(os.Stdin)

func promptLine(msg string) string {
	fmt.Fprint(os.Stderr, msg)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func confirmLine(msg string) bool {
	ans := promptLine(msg)
	return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")
}

// ── Public prompt API ─────────────────────────────────────────────────────────

func prompt(msg string) string {
	if !isTTY() {
		return promptLine(msg)
	}
	var out string
	err := runField(huh.NewInput().
		Title(strings.TrimRight(msg, ": ")).
		Value(&out))
	handleAbort(err)
	return out
}

func promptWithDefault(msg, def string) string {
	if !isTTY() {
		raw := promptLine(fmt.Sprintf("%s [%s]: ", msg, def))
		if raw == "" {
			return def
		}
		return raw
	}
	out := def
	err := runField(huh.NewInput().
		Title(msg).
		Value(&out))
	handleAbort(err)
	if out == "" {
		return def
	}
	return out
}

func confirm(msg string) bool {
	if !isTTY() {
		return confirmLine(msg)
	}
	var out bool
	err := runField(huh.NewConfirm().
		Title(strings.TrimRight(msg, " [y/N]? ")).
		Affirmative("Yes").
		Negative("No").
		Value(&out))
	handleAbort(err)
	return out
}

// ── Top-level action picker (hub mode) ────────────────────────────────────────

// ── Profile picker ────────────────────────────────────────────────────────────

func pickProfile() (string, error) {
	locs, err := listAllLocations()
	if err != nil {
		return "", err
	}
	if len(locs) == 0 {
		return "", fmt.Errorf("no profiles found — run: claude-profiles new")
	}

	if !isTTY() {
		fmt.Fprintln(os.Stderr)
		for i, loc := range locs {
			fmt.Fprintf(os.Stderr, "  %2d) %s\n", i+1, locationLabel(loc))
		}
		fmt.Fprintln(os.Stderr)
		raw := promptLine(fmt.Sprintf("Select profile [1-%d]: ", len(locs)))
		var n int
		fmt.Sscanf(strings.TrimSpace(raw), "%d", &n)
		if n < 1 || n > len(locs) {
			return "", fmt.Errorf("invalid selection")
		}
		return locs[n-1].QualifiedID, nil
	}

	opts := make([]huh.Option[string], len(locs))
	for i, loc := range locs {
		opts[i] = huh.NewOption(locationLabel(loc), loc.QualifiedID)
	}
	var selected string
	err = runField(huh.NewSelect[string]().
		Title("Select a profile").
		Options(opts...).
		Value(&selected))
	if err != nil {
		handleAbort(err)
		return "", err
	}
	return selected, nil
}

// pickBackgroundedSession shows a huh select with all backgrounded sessions
// for the profile so the user can pick exactly which one to wrapper-resume.
// Returns nil if the user cancelled or chose "skip".
func pickBackgroundedSession(profile string, bgs []BackgroundedSession) *BackgroundedSession {
	if !isTTY() || len(bgs) == 0 {
		return nil
	}
	opts := make([]huh.Option[string], len(bgs))
	for i, bs := range bgs {
		label := fmt.Sprintf("%s · started %s · %s",
			shortSession(bs.SessionID),
			bs.StartedAt.Format("Jan 2 15:04"),
			shortenCwd(bs.Cwd))
		opts[i] = huh.NewOption(label, bs.SessionID)
	}

	selected := bgs[0].SessionID // pre-select the newest session
	err := runFieldBack(huh.NewSelect[string]().
		Title(fmt.Sprintf("%s has %d backgrounded sessions — pick one to resume (Esc to skip)", profile, len(bgs))).
		Options(opts...).
		Value(&selected))
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	if selected == "" {
		return nil
	}
	for i := range bgs {
		if bgs[i].SessionID == selected {
			return &bgs[i]
		}
	}
	return nil
}

func shortenCwd(cwd string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(cwd, home) {
		return "~" + cwd[len(home):]
	}
	return cwd
}

func locationLabel(loc ProfileLocation) string {
	var source string
	switch {
	case loc.RepoAlias == ".":
		source = styleInfo.Render("project")
	case loc.RepoAlias != "":
		source = styleInfo.Render("repo:" + loc.RepoAlias)
	default:
		source = styleInfo.Render("local")
	}
	return fmt.Sprintf("%-30s  %s", loc.QualifiedID, source)
}

// pickScope asks where to save a new profile: user (~/.claude-profiles) or
// project (.claude-profiles/ in the current directory).
func pickScope() string {
	if !isTTY() {
		raw := promptLine("Save to: (1) user  (2) project  [default: 1]: ")
		if strings.TrimSpace(raw) == "2" {
			return "project"
		}
		return "user"
	}
	cwd, _ := os.Getwd()
	scope := "user"
	err := runField(huh.NewSelect[string]().
		Title("Save scope").
		Description("User profiles are available everywhere; project profiles live in .claude-profiles/ and can be shared via the repo.").
		Options(
			huh.NewOption("user — ~/.claude-profiles/profiles/  (available everywhere)", "user"),
			huh.NewOption(fmt.Sprintf("project — .claude-profiles/  in %s", shortenCwd(cwd)), "project"),
		).
		Value(&scope))
	handleAbort(err)
	return scope
}

// fitHeight returns a Height that caps the viewport for large lists (used only
// when option count exceeds maxAutoOptions). For small lists, callers should
// omit Height() so huh sizes the viewport to fit every option.
func fitHeight(n int) int {
	const maxViewport = 20
	return maxViewport
}

func profileLabel(name string) string {
	p, err := loadProfile(name)
	if err != nil {
		return name
	}
	servers := make([]string, 0, len(p.McpServers))
	for k := range p.McpServers {
		servers = append(servers, k)
	}
	sort.Strings(servers)
	label := fmt.Sprintf("%-24s %s", name, "["+strings.Join(servers, ", ")+"]")
	if len(p.DeniedTools) > 0 {
		label += styleInfo.Render(fmt.Sprintf(" deny:%d", len(p.DeniedTools)))
	}
	return label
}

// resolveProfile returns a profile name from an explicit arg or interactive picker.
func resolveProfile(arg string) (string, error) {
	if arg != "" {
		if profileExists(arg) {
			return arg, nil
		}
		return "", fmt.Errorf("profile not found: %s", arg)
	}
	return pickProfile()
}

// ── Claude settings: permission mode + model ──────────────────────────────────

func configureSettings(p *Profile) {
	fmt.Fprintln(os.Stderr)
	if !confirm("Configure session settings (permission mode, model)?") {
		return
	}
	s := parseSettings(p.Settings)
	setPermissionMode(s, pickPermissionMode(getPermissionMode(s)))
	setModel(s, pickModel(getModel(s)))
	p.Settings = marshalSettings(s)
}

func pickPermissionMode(current string) string {
	if !isTTY() {
		return current
	}
	mode := current
	if mode == "" {
		mode = "skip"
	}
	err := runField(huh.NewSelect[string]().
		Title("Permission mode").
		Description("Controls when Claude asks before running tools.").
		Options(
			huh.NewOption("(leave unset — use global default)", "skip"),
			huh.NewOption("default — prompt for each tool use", "default"),
			huh.NewOption("acceptEdits — auto-approve edits, prompt for the rest", "acceptEdits"),
			huh.NewOption("auto — auto-approve safe actions, prompt for risky", "auto"),
			huh.NewOption("plan — read-only planning mode (no writes/edits)", "plan"),
			huh.NewOption("bypassPermissions — auto-approve everything (DANGEROUS)", "bypassPermissions"),
		).
		Value(&mode))
	handleAbort(err)
	if mode == "skip" {
		return ""
	}
	return mode
}

func pickModel(current string) string {
	if !isTTY() {
		return current
	}
	model := current
	if model == "" {
		model = "skip"
	}
	err := runField(huh.NewSelect[string]().
		Title("Model").
		Description("Pin a Claude model for this profile.").
		Options(
			huh.NewOption("(leave unset — use global default)", "skip"),
			huh.NewOption("haiku — fastest, cheapest", "haiku"),
			huh.NewOption("sonnet — balanced (default Claude Code model)", "sonnet"),
			huh.NewOption("opus — most capable, slowest", "opus"),
		).
		Value(&model))
	handleAbort(err)
	if model == "skip" {
		return ""
	}
	return model
}

// ── Server-type picker ────────────────────────────────────────────────────────

func pickServerType() string {
	if !isTTY() {
		raw := promptLine("  Type: (1) HTTP/remote  (2) stdio/local  [default: 1]: ")
		if strings.TrimSpace(raw) == "2" {
			return "stdio"
		}
		return "http"
	}
	var t string
	err := runField(huh.NewSelect[string]().
		Title("Server type").
		Options(
			huh.NewOption("HTTP / remote MCP", "http"),
			huh.NewOption("stdio / local command", "stdio"),
		).
		Value(&t))
	handleAbort(err)
	if t == "" {
		t = "http"
	}
	return t
}

// ── Tool filter selection ─────────────────────────────────────────────────────

func selectToolFilter(p *Profile, sname string, tools []ToolInfo) {
	count := len(tools)
	info("  %d tool(s) available for %q.", count, sname)

	mode := pickFilterMode()
	if mode == "all" {
		return
	}

	// Mode "ro": auto-whitelist read-only tools, no manual picking
	if mode == "ro" {
		var roNames []string
		for _, t := range tools {
			if t.ReadOnlyHint {
				roNames = append(roNames, t.Name)
			}
		}
		if len(roNames) == 0 {
			warn("  No read-only tools found (server may not provide annotations) — keeping all.")
			return
		}
		applyWhitelist(p, tools, roNames)
		success("  + read-only mode: %d allowed [R], %d denied", len(roNames), count-len(roNames))
		return
	}

	// Modes "whitelist" / "deny": multi-select tools
	picked := multiSelectTools(tools, mode)
	if len(picked) == 0 {
		warn("  No tools selected — keeping all.")
		return
	}

	switch mode {
	case "whitelist":
		applyWhitelist(p, tools, picked)
		denied := count - len(picked)
		success("  + whitelist: %d allowed, %d denied", len(picked), denied)
	case "deny":
		applyDeny(p, picked)
		success("  + deny: %d tool(s)", len(picked))
	}
}

func pickFilterMode() string {
	if !isTTY() {
		raw := promptLine("    (1) Allow all  (2) Whitelist  (3) Deny specific  (4) Read-only only  [default: 1]: ")
		switch strings.TrimSpace(raw) {
		case "2":
			return "whitelist"
		case "3":
			return "deny"
		case "4":
			return "ro"
		default:
			return "all"
		}
	}
	var mode string
	err := runField(huh.NewSelect[string]().
		Title("Tool filter mode").
		Description("How should Claude be allowed to use this server's tools?").
		Options(
			huh.NewOption("Allow all tools", "all"),
			huh.NewOption("Whitelist — pick which to allow", "whitelist"),
			huh.NewOption("Deny — block specific tools", "deny"),
			huh.NewOption("Read-only only — auto-allow read-only tools", "ro"),
		).
		Value(&mode))
	handleAbort(err)
	if mode == "" {
		return "all"
	}
	return mode
}

func multiSelectTools(tools []ToolInfo, mode string) []string {
	if !isTTY() {
		fmt.Fprintln(os.Stderr)
		for i, t := range tools {
			marker := ""
			if t.ReadOnlyHint {
				marker = " [R]"
			}
			fmt.Fprintf(os.Stderr, "    %3d) %s%s\n", i+1, shortToolName(t.Name), marker)
		}
		fmt.Fprintln(os.Stderr)
		raw := promptLine("  Select (comma-separated numbers, e.g. 1,3,5): ")
		return parseSelectionNumbers(raw, tools)
	}

	var titleText, descText string
	if mode == "whitelist" {
		titleText = "Whitelist — pick tools to ALLOW"
		descText = "Selected tools will be allowed; all others denied. Use ↑/↓, space to toggle, / to search, enter to confirm."
	} else {
		titleText = "Deny — pick tools to BLOCK"
		descText = "Selected tools will be denied; others allowed. Use ↑/↓, space to toggle, / to search, enter to confirm."
	}

	opts := make([]huh.Option[string], len(tools))
	for i, t := range tools {
		label := shortToolName(t.Name)
		if t.ReadOnlyHint {
			label = label + " " + styleReadOnly.Render("[R]")
		}
		opts[i] = huh.NewOption(label, t.Name)
	}

	var picked []string
	ms := huh.NewMultiSelect[string]().
		Title(titleText).
		Description(descText).
		Options(opts...).
		Value(&picked).
		Filterable(true)
	// Only cap viewport for large lists; small lists auto-size to fit.
	if len(opts) > 15 {
		ms = ms.Height(fitHeight(len(opts)))
	}
	err := runField(ms)
	handleAbort(err)
	return picked
}

func parseSelectionNumbers(raw string, tools []ToolInfo) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 || n > len(tools) {
			continue
		}
		out = append(out, tools[n-1].Name)
	}
	return out
}

func shortToolName(full string) string {
	parts := strings.SplitN(full, "__", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return full
}

// ── Edit menu ─────────────────────────────────────────────────────────────────

// pickEditAction renders the top-level edit menu for a local profile. Each
// option label embeds a one-glance summary of the current state so the user
// doesn't have to drill in to see what is set.
func pickEditAction(name string, p *Profile) string {
	servers := len(p.McpServers)
	denied := len(p.DeniedTools)
	toolsLabel := fmt.Sprintf("Manage MCP tool filters (%d server%s, %d denied)",
		servers, plural(servers), denied)

	s := parseSettings(p.Settings)
	mode := getPermissionMode(s)
	if mode == "" {
		mode = "—"
	}
	model := getModel(s)
	if model == "" {
		model = "—"
	}
	settingsLabel := fmt.Sprintf("Session settings (mode: %s, model: %s)", mode, model)
	isolatedState := "off"
	if p.Isolated {
		isolatedState = "on"
	}
	isolatedLabel := fmt.Sprintf("Isolated mode: %s (ignore user/project settings.json)", isolatedState)

	loc, _ := resolveProfileLocation(name)
	kinds := []string{}
	if loc != nil {
		kinds = profilePluginKinds(*loc)
	}
	pluginSummary := "none"
	if len(kinds) > 0 {
		pluginSummary = strings.Join(kinds, ", ")
	}
	pluginLabel := fmt.Sprintf("Profile-bundled commands/skills/agents/hooks (%s)", pluginSummary)

	action := "tools"
	err := runField(huh.NewSelect[string]().
		Title("Edit " + name).
		Options(
			huh.NewOption(toolsLabel, "tools"),
			huh.NewOption(settingsLabel, "settings"),
			huh.NewOption(isolatedLabel, "isolated"),
			huh.NewOption(pluginLabel, "plugin"),
			huh.NewOption("Open profile.json in $EDITOR", "editor"),
			huh.NewOption("Done", "done"),
		).
		Value(&action))
	handleAbort(err)
	return action
}

// manageToolFilters lets the user pick a server, refetch its tools, and re-run
// the same allow/deny picker used in `claude-profiles new`. A confirm gate
// protects existing filters from being wiped by a no-op pass. Esc returns to
// the edit menu — there's no explicit "Back" option, since one default-
// selected sentinel item would conflict with the natural first server choice.
func manageToolFilters(p *Profile, name string) {
	if len(p.McpServers) == 0 {
		warn("No MCP servers in this profile — add one via `claude-profiles edit %s` → $EDITOR, or `new` from scratch.", name)
		return
	}
	for {
		snames := make([]string, 0, len(p.McpServers))
		for k := range p.McpServers {
			snames = append(snames, k)
		}
		sort.Strings(snames)

		opts := make([]huh.Option[string], len(snames))
		for i, sname := range snames {
			n := countServerDenied(p, sname)
			tag := styleInfo.Render(fmt.Sprintf("(%d denied)", n))
			opts[i] = huh.NewOption(fmt.Sprintf("%-24s %s", sname, tag), sname)
		}

		picked := snames[0] // pre-select the first server so huh highlights it
		err := runFieldBack(huh.NewSelect[string]().
			Title("Reconfigure tool filter — pick a server (Esc to go back)").
			Options(opts...).
			Value(&picked))
		if errors.Is(err, huh.ErrUserAborted) {
			return
		}
		reconfigureServerFilter(p, picked)
		if err := saveProfile(name, p); err != nil {
			fatal(err)
		}
	}
}

// reconfigureServerFilter re-fetches tools for sname, prompts the user before
// discarding any pre-existing filter, then runs the same picker `new` uses.
// Stays silent (and leaves state untouched) when the server can't be reached.
func reconfigureServerFilter(p *Profile, sname string) {
	cfg, ok := p.McpServers[sname]
	if !ok {
		return
	}
	if n := countServerDenied(p, sname); n > 0 {
		info("  Current filter denies %d tool(s) for %q.", n, sname)
		if !confirm("Replace this server's tool filter?") {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "  Fetching available tools from %q...\n", sname)
	tools, err := FetchTools(cfg, sname)
	if err != nil {
		if errors.Is(err, errNeedsAuth) {
			warn("  Could not authenticate to %q — leaving filter unchanged.", sname)
		} else {
			warn("  Could not reach %q — leaving filter unchanged.", sname)
		}
		return
	}
	clearServerDeniedTools(p, sname)
	info("  %d tool(s) found.", len(tools))
	selectToolFilter(p, sname, tools)
}

// manageProfilePlugin offers to scaffold commands/, skills/, agents/, hooks/
// folders inside the profile dir. The wrapper auto-detects them on next launch
// and passes the profile folder via --plugin-dir; no further config needed.
func manageProfilePlugin(name string) {
	loc, err := resolveProfileLocation(name)
	if err != nil {
		fatal(err)
	}
	root := filepath.Dir(loc.JSONPath)
	info("Profile folder: %s", root)
	info("Existing plugin content: %s", strings.Join(profilePluginKinds(*loc), ", "))
	info("Claude's --plugin-dir auto-discovers these subdirs:")
	for _, sub := range pluginSubdirs {
		exists := ""
		if _, err := os.Stat(filepath.Join(root, sub)); err == nil {
			exists = " " + styleSuccess.Render("(present)")
		}
		fmt.Fprintf(os.Stderr, "    · %s/%s\n", sub, exists)
	}

	for {
		var picked string
		err := runFieldBack(huh.NewSelect[string]().
			Title("Scaffold which? (Esc to go back)").
			Options(
				huh.NewOption("commands/  — slash commands (.md files)", "commands"),
				huh.NewOption("skills/    — per-skill folder with SKILL.md", "skills"),
				huh.NewOption("agents/    — subagent definitions (.md)", "agents"),
				huh.NewOption("hooks/     — hooks.json", "hooks"),
				huh.NewOption("Open the profile folder in $EDITOR", "open"),
			).
			Value(&picked))
		if errors.Is(err, huh.ErrUserAborted) {
			return
		}
		switch picked {
		case "commands", "skills", "agents", "hooks":
			scaffoldPluginSubdir(root, picked)
		case "open":
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			cmd := exec.Command(editor, root)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
		}
	}
}

// scaffoldPluginSubdir creates the named plugin folder (commands/, skills/,
// etc.) inside the profile dir if missing, and seeds a starter file so it's
// obvious what shape to follow. Idempotent.
func scaffoldPluginSubdir(root, kind string) {
	dir := filepath.Join(root, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		warn("could not create %s: %v", dir, err)
		return
	}
	stub := ""
	stubPath := ""
	switch kind {
	case "commands":
		stubPath = filepath.Join(dir, "hello.md")
		stub = "---\ndescription: Sample slash command bundled with this profile\nallowed-tools: Bash\n---\nReply: \"hello from " + filepath.Base(root) + "\".\n"
	case "skills":
		stubDir := filepath.Join(dir, "hello-skill")
		_ = os.MkdirAll(stubDir, 0o755)
		stubPath = filepath.Join(stubDir, "SKILL.md")
		stub = "---\nname: hello-skill\ndescription: Sample skill bundled with this profile\n---\nWhen invoked, greet the user.\n"
	case "agents":
		stubPath = filepath.Join(dir, "reviewer.md")
		stub = "---\nname: reviewer\ndescription: Sample subagent bundled with this profile\n---\nReview code changes pragmatically. Flag risks, suggest tests.\n"
	case "hooks":
		stubPath = filepath.Join(dir, "hooks.json")
		stub = `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit",
        "hooks": [
          { "type": "command", "command": "echo edited >&2" }
        ]
      }
    ]
  }
}
`
	}
	if _, err := os.Stat(stubPath); err == nil {
		info("%s already exists — leaving it alone.", stubPath)
		return
	}
	if err := os.WriteFile(stubPath, []byte(stub), 0o644); err != nil {
		warn("could not seed %s: %v", stubPath, err)
		return
	}
	success("Scaffolded %s", stubPath)
}

func countServerDenied(p *Profile, sname string) int {
	prefix := "mcp__" + sname + "__"
	n := 0
	for _, t := range p.DeniedTools {
		if strings.HasPrefix(t, prefix) {
			n++
		}
	}
	return n
}

func clearServerDeniedTools(p *Profile, sname string) {
	prefix := "mcp__" + sname + "__"
	kept := p.DeniedTools[:0]
	for _, t := range p.DeniedTools {
		if !strings.HasPrefix(t, prefix) {
			kept = append(kept, t)
		}
	}
	p.DeniedTools = kept
}
