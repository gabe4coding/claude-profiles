package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// runForm wraps multiple huh Fields in a Form with the same preferred bindings
// as runField. Use when a single step needs more than one input.
func runForm(fields ...huh.Field) error {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(
		key.WithKeys("esc", "ctrl+c"),
		key.WithHelp("esc/ctrl+c", "back"),
	)
	return huh.NewForm(huh.NewGroup(fields...)).
		WithShowHelp(true).
		WithKeyMap(km).
		WithTheme(claudeTheme()).
		Run()
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

// pickPrompt shows a selection list of profile prompts and returns the text of
// the chosen one. Returns ("", nil) when the user picks "start interactive" or
// aborts so the caller can treat a blank return as "no initial message."
func pickPrompt(prompts []ProfilePrompt) (string, error) {
	if !isTTY() {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "   0) [start interactive — no prompt]")
		for i, p := range prompts {
			fmt.Fprintf(os.Stderr, "  %2d) %s\n", i+1, p.Name)
		}
		fmt.Fprintln(os.Stderr)
		raw := promptLine(fmt.Sprintf("Select starting prompt [0-%d]: ", len(prompts)))
		var n int
		fmt.Sscanf(strings.TrimSpace(raw), "%d", &n)
		if n < 0 || n > len(prompts) {
			return "", fmt.Errorf("invalid selection")
		}
		if n == 0 {
			return "", nil
		}
		return prompts[n-1].Text, nil
	}

	opts := make([]huh.Option[string], len(prompts)+1)
	opts[0] = huh.NewOption("[start interactive — no prompt]", "")
	for i, p := range prompts {
		opts[i+1] = huh.NewOption(p.Name, p.Text)
	}
	var selected string
	err := runField(huh.NewSelect[string]().
		Title("Select a starting prompt").
		Options(opts...).
		Value(&selected))
	if err != nil {
		handleAbort(err)
		return "", err
	}
	return selected, nil
}

// pickPinPromptName presents the profile's prompts so the user can optionally
// pin one for quick launch. Returns the selected prompt name, or "" for none.
// Esc / abort returns an error — callers treat that as "no prompt selected".
func pickPinPromptName(prompts []ProfilePrompt) (string, error) {
	if !isTTY() {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "   0) [no prompt — launch interactively]")
		for i, p := range prompts {
			fmt.Fprintf(os.Stderr, "  %2d) %s\n", i+1, p.Name)
		}
		fmt.Fprintln(os.Stderr)
		raw := promptLine(fmt.Sprintf("Pin a prompt for quick launch [0-%d, 0 = none]: ", len(prompts)))
		var n int
		fmt.Sscanf(strings.TrimSpace(raw), "%d", &n)
		if n < 0 || n > len(prompts) {
			return "", fmt.Errorf("invalid selection")
		}
		if n == 0 {
			return "", nil
		}
		return prompts[n-1].Name, nil
	}

	opts := make([]huh.Option[string], len(prompts)+1)
	opts[0] = huh.NewOption("[no prompt — launch interactively]", "")
	for i, p := range prompts {
		opts[i+1] = huh.NewOption(p.Name, p.Name)
	}
	var selected string
	err := runField(huh.NewSelect[string]().
		Title("Pin a prompt for quick launch (optional)").
		Options(opts...).
		Value(&selected))
	if err != nil {
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
		label := fmt.Sprintf("%s · %s · %s",
			shortSession(bs.SessionID),
			bs.StartedAt.Format("Jan 2 15:04"),
			shortenCwd(bs.Cwd))
		if bs.Hint != "" {
			label += fmt.Sprintf(" — %q", bs.Hint)
		}
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

// worktreeChoice is the result of pickWorktreeOrNew. Exactly one of the fields
// is set: isNew=true for a fresh worktree, or worktree for an existing one.
type worktreeChoice struct {
	isNew    bool
	worktree *WorktreeInfo
}

// pickWorktreeOrNew shows a picker listing existing worktrees plus a "[+ new
// worktree]" option. Sessions that are currently backgrounded are annotated
// with [bg], and sessions running in a live wrapper are annotated with [live]
// and excluded from selection (shown for reference only). Returns nil if the
// user cancelled (Esc).
func pickWorktreeOrNew(
	profile string,
	worktrees []WorktreeInfo,
	bgs []BackgroundedSession,
	runningSessionIDs map[string]bool,
) *worktreeChoice {
	if !isTTY() {
		return &worktreeChoice{isNew: true}
	}

	// Build a session-id → bg mapping for annotation
	bgSessionIDs := make(map[string]bool, len(bgs))
	for _, b := range bgs {
		bgSessionIDs[b.SessionID] = true
	}

	const newKey = "__new__"
	opts := []huh.Option[string]{huh.NewOption("[+ new worktree]", newKey)}

	for _, wt := range worktrees {
		label := wt.Name
		if wt.Branch != "" {
			label += "  branch:" + wt.Branch
		}
		if !wt.LastSessionTime.IsZero() {
			label += "  " + humanAge(wt.LastSessionTime)
		}
		if runningSessionIDs[wt.LastSessionID] {
			label += "  [live]"
		} else if bgSessionIDs[wt.LastSessionID] {
			label += "  [bg]"
		}
		if wt.Hint != "" {
			label += fmt.Sprintf(" — %q", wt.Hint)
		}
		opts = append(opts, huh.NewOption(label, wt.Name))
	}

	selected := newKey
	err := runFieldBack(huh.NewSelect[string]().
		Title(fmt.Sprintf("%s — open an existing worktree or start a new one:", profile)).
		Options(opts...).
		Value(&selected))
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	if selected == newKey || selected == "" {
		return &worktreeChoice{isNew: true}
	}
	for i := range worktrees {
		if worktrees[i].Name == selected {
			return &worktreeChoice{worktree: &worktrees[i]}
		}
	}
	return &worktreeChoice{isNew: true}
}

// humanAge returns a short human-readable string like "2h ago" for a past time.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
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
	case loc.Builtin != "":
		source = styleInfo.Render("builtin")
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

// pickEditAction renders the top-level edit menu for a profile. Each option
// label embeds a one-glance summary of the current state so the user doesn't
// have to drill in to see what is set.
func pickEditAction(loc ProfileLocation, p *Profile) string {
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

	worktreeState := "off"
	if p.Worktree {
		worktreeState = "on"
	}
	worktreeLabel := fmt.Sprintf("Worktree mode: %s (start each session in a fresh git worktree)", worktreeState)

	distillState := "off"
	if strings.EqualFold(p.Distill, "on") {
		distillState = "on"
	}
	distillLabel := fmt.Sprintf("Session distillation: %s (record findings to CLAUDE.md / rules at session end)", distillState)

	subagentState := "—"
	if p.SubagentModel != "" {
		subagentState = shortModelLabel(p.SubagentModel)
	}
	subagentLabel := fmt.Sprintf("Subagent model: %s (pins model for subagents spawned inside /delegate bg sessions)", subagentState)

	kinds := profilePluginKinds(loc)
	pluginSummary := "none"
	if len(kinds) > 0 {
		pluginSummary = strings.Join(kinds, ", ")
	}
	pluginLabel := fmt.Sprintf("Profile-bundled commands/skills/agents/hooks (%s)", pluginSummary)

	promptCount := len(p.Prompts)
	promptsLabel := fmt.Sprintf("Session prompts — pre-filled starters (%d defined)", promptCount)

	var opts []huh.Option[string]
	if loc.RepoAlias != "" {
		// Project and repo profiles: only prefs-backed options are safe to edit.
		opts = []huh.Option[string]{
			huh.NewOption(isolatedLabel, "isolated"),
			huh.NewOption(worktreeLabel, "worktree"),
			huh.NewOption(distillLabel, "distill"),
			huh.NewOption(subagentLabel, "subagent_model"),
			huh.NewOption(promptsLabel, "prompts"),
			huh.NewOption(settingsLabel, "settings"),
			huh.NewOption("Open user settings in $EDITOR", "editor"),
			huh.NewOption("Done", "done"),
		}
	} else {
		opts = []huh.Option[string]{
			huh.NewOption(toolsLabel, "tools"),
			huh.NewOption(settingsLabel, "settings"),
			huh.NewOption(isolatedLabel, "isolated"),
			huh.NewOption(worktreeLabel, "worktree"),
			huh.NewOption(distillLabel, "distill"),
			huh.NewOption(subagentLabel, "subagent_model"),
			huh.NewOption(promptsLabel, "prompts"),
			huh.NewOption(pluginLabel, "plugin"),
			huh.NewOption("Open profile folder in $EDITOR", "editor"),
			huh.NewOption("Done", "done"),
		}
	}
	action := opts[0].Value
	err := runField(huh.NewSelect[string]().
		Title("Edit " + loc.QualifiedID).
		Options(opts...).
		Value(&action))
	handleAbort(err)
	return action
}

// manageToolFilters lets the user pick a server, refetch its tools, and re-run
// the same allow/deny picker used in `claude-profiles new`. A confirm gate
// protects existing filters from being wiped by a no-op pass. Esc returns to
// the edit menu — there's no explicit "Back" option, since one default-
// selected sentinel item would conflict with the natural first server choice.
func manageToolFilters(p *Profile, loc ProfileLocation) {
	if len(p.McpServers) == 0 {
		warn("No MCP servers in this profile — add one via `claude-profiles edit %s` → $EDITOR, or `new` from scratch.", loc.QualifiedID)
		return
	}
	dir := filepath.Dir(loc.JSONPath)
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
		if err := saveProfileAt(dir, p); err != nil {
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
func manageProfilePlugin(loc ProfileLocation) {
	root := filepath.Dir(loc.JSONPath)
	info("Profile folder: %s", root)
	info("Existing plugin content: %s", strings.Join(profilePluginKinds(loc), ", "))
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
			openInEditor(root)
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

// ── Prompt management ─────────────────────────────────────────────────────────

// managePrompts is the interactive sub-menu for adding, editing, and deleting
// _prompts entries on a profile. Saves after each change; Esc returns to caller.
func managePrompts(p *Profile, loc ProfileLocation, save func(*Profile) error) {
	for {
		opts := make([]huh.Option[string], 0, len(p.Prompts)+1)
		for i, pr := range p.Prompts {
			preview := pr.Text
			if len(preview) > 48 {
				preview = preview[:48] + "…"
			}
			label := fmt.Sprintf("%-20s  %s", pr.Name, styleInfo.Render(preview))
			opts = append(opts, huh.NewOption(label, fmt.Sprintf("%d", i)))
		}
		opts = append(opts, huh.NewOption("+ Add new prompt", "add"))

		sel := "add"
		if len(p.Prompts) > 0 {
			sel = "0"
		}
		err := runFieldBack(huh.NewSelect[string]().
			Title(fmt.Sprintf("Manage prompts for %s (Esc to go back)", loc.QualifiedID)).
			Options(opts...).
			Value(&sel))
		if errors.Is(err, huh.ErrUserAborted) {
			return
		}

		if sel == "add" {
			if addPrompt(p) {
				if err := save(p); err != nil {
					fatal(err)
				}
			}
			continue
		}

		var idx int
		fmt.Sscanf(sel, "%d", &idx)
		if idx < 0 || idx >= len(p.Prompts) {
			continue
		}
		if promptAction(p, idx) {
			if err := save(p); err != nil {
				fatal(err)
			}
		}
	}
}

// addPrompt runs the add-prompt form and appends the result to p.Prompts.
// Returns true when a prompt was actually added.
func addPrompt(p *Profile) bool {
	var pname, ptext string
	err := runForm(
		huh.NewInput().Title("Prompt name").Placeholder("e.g. Daily standup").Value(&pname),
		huh.NewText().Title("Prompt text").Placeholder("Type the message text…").Value(&ptext),
	)
	if errors.Is(err, huh.ErrUserAborted) || err != nil {
		return false
	}
	pname = strings.TrimSpace(pname)
	if pname == "" {
		return false
	}
	p.Prompts = append(p.Prompts, ProfilePrompt{Name: pname, Text: strings.TrimSpace(ptext)})
	success("  + Added prompt %q", pname)
	return true
}

// promptAction shows edit/delete options for p.Prompts[idx].
// Returns true when the slice was mutated.
func promptAction(p *Profile, idx int) bool {
	pr := p.Prompts[idx]
	var action string
	err := runFieldBack(huh.NewSelect[string]().
		Title(fmt.Sprintf("Prompt: %q", pr.Name)).
		Options(
			huh.NewOption("Edit name and text", "edit"),
			huh.NewOption("Delete this prompt", "delete"),
		).
		Value(&action))
	if errors.Is(err, huh.ErrUserAborted) {
		return false
	}
	switch action {
	case "edit":
		pname, ptext := pr.Name, pr.Text
		err := runForm(
			huh.NewInput().Title("Prompt name").Value(&pname),
			huh.NewText().Title("Prompt text").Value(&ptext),
		)
		if errors.Is(err, huh.ErrUserAborted) || err != nil {
			return false
		}
		pname = strings.TrimSpace(pname)
		if pname == "" {
			return false
		}
		p.Prompts[idx].Name = pname
		p.Prompts[idx].Text = strings.TrimSpace(ptext)
		success("  + Updated prompt %q", pname)
		return true
	case "delete":
		if !confirm(fmt.Sprintf("Delete prompt %q?", pr.Name)) {
			return false
		}
		p.Prompts = append(p.Prompts[:idx], p.Prompts[idx+1:]...)
		success("  + Deleted prompt %q", pr.Name)
		return true
	}
	return false
}

// shortModelLabel returns a compact display label for a Claude Code model id.
// Long ids (claude-haiku-4-5-20251001) become "haiku" so the edit menu and
// hub tags stay readable. Falls back to the raw id if no canonical family
// matches — that way custom / future models still render correctly.
func shortModelLabel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "haiku"):
		return "haiku"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "opus"):
		return "opus"
	}
	return model
}

// pickSubagentModel prompts the user to choose a model for SubagentModel.
// Four canonical families plus a "clear" sentinel that unsets the field.
// Returns the picked model id (or "" for clear / abort) and whether the
// caller should persist the value.
func pickSubagentModel(current string) (string, bool) {
	const (
		idHaiku  = "claude-haiku-4-5-20251001"
		idSonnet = "claude-sonnet-4-6"
		idOpus   = "claude-opus-4-7"
		clear    = "__clear__"
	)
	choice := current
	if choice == "" {
		choice = idHaiku
	}
	err := runField(huh.NewSelect[string]().
		Title("Subagent model — used by subagents spawned inside /delegate bg sessions").
		Description("Requires Claude Code v2.1.146+. Clear to fall back to whatever the delegate decides.").
		Options(
			huh.NewOption("Haiku (claude-haiku-4-5-20251001) — fast / cheap", idHaiku),
			huh.NewOption("Sonnet (claude-sonnet-4-6) — balanced", idSonnet),
			huh.NewOption("Opus (claude-opus-4-7) — most capable", idOpus),
			huh.NewOption("Clear (unset — let the delegate decide)", clear),
		).
		Value(&choice))
	if err != nil {
		// Esc / abort — keep current value unchanged.
		return current, false
	}
	if choice == clear {
		return "", true
	}
	return choice, true
}
