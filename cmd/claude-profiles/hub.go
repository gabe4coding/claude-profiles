package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Hub view ──────────────────────────────────────────────────────────────────
//
// Two-pane layout:
//   - top: always-visible text input ("Ask" — describe what you want to do)
//   - body: profile list with single-key shortcuts for actions
//
// Initial focus is on the input. Down/Tab moves to the list. Up on the first
// list row jumps back to the input. Enter on the input runs the ask flow
// (classify with haiku, then launch). Enter on a list row launches that
// profile directly. All shortcuts (n/e/d/c/x/i/r/q) only fire while the list
// is focused — letter keys typed in the input go into the text.

type hubAction string

const (
	actLaunch   hubAction = "launch"
	actAsk      hubAction = "ask"
	actNew      hubAction = "new"
	actGenerate hubAction = "generate"
	actEdit     hubAction = "edit"
	actDelete   hubAction = "delete"
	actCopy     hubAction = "copy"
	actExport   hubAction = "export"
	actImport   hubAction = "import"
	actRepo     hubAction = "repo"
	actQuit     hubAction = "quit"
)

type hubResult struct {
	action  hubAction
	profile string // qualified id; carries the prompt text for actAsk
	prompt  string // only set for actAsk
}

type focusTarget int

const (
	focusInput focusTarget = iota
	focusList
)

type profileItem struct {
	loc      ProfileLocation
	titleStr string
	descStr  string
}

func (p profileItem) Title() string       { return p.titleStr }
func (p profileItem) Description() string { return p.descStr }
func (p profileItem) FilterValue() string { return p.loc.QualifiedID }

type hubModel struct {
	list   list.Model
	input  textinput.Model
	focus  focusTarget
	result hubResult
	help   string
	width  int
	height int

	// Two delegates: focusedDelegate uses the coral highlight for the selected
	// row, unfocusedDelegate makes the selected row look like a normal row.
	// We swap them as focus moves between the input and the list so the
	// highlight only appears when the list itself is interactive.
	focusedDelegate   list.DefaultDelegate
	unfocusedDelegate list.DefaultDelegate

	// Ask-prompt history cycling (shell-style ↑/↓ in the input).
	// historyIdx == -1 means "not cycling" — the input value is the user's
	// own in-progress text. 0 is the most recent saved entry, growing older.
	cachedHistory []AskHistoryEntry
	historyIdx    int
	savedInput    string // user's in-progress text, restored when cycle ends
}

// setFocus moves focus and swaps the list delegate so the selection highlight
// only appears when the list is the active pane.
func (m *hubModel) setFocus(t focusTarget) tea.Cmd {
	m.focus = t
	if t == focusInput {
		m.input.Focus()
		m.list.SetDelegate(m.unfocusedDelegate)
		return textinput.Blink
	}
	m.input.Blur()
	m.list.SetDelegate(m.focusedDelegate)
	return nil
}

func (m hubModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m hubModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Input takes 3 lines (border + text + spacer); footer 1; remainder is list
		listH := msg.Height - 4
		if listH < 4 {
			listH = 4
		}
		m.list.SetSize(msg.Width, listH)
		m.input.Width = msg.Width - 6
		return m, nil

	case tea.KeyMsg:
		if m.focus == focusInput {
			return m.updateInput(msg)
		}
		return m.updateList(msg)
	}

	var cmd tea.Cmd
	if m.focus == focusInput {
		m.input, cmd = m.input.Update(msg)
	} else {
		m.list, cmd = m.list.Update(msg)
	}
	return m, cmd
}

func (m hubModel) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.result = hubResult{action: actQuit}
		return m, tea.Quit
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.result = hubResult{action: actAsk, prompt: text}
		return m, tea.Quit
	case "up":
		return m.cycleAskHistory(+1)
	case "down":
		// If actively cycling, step forward; otherwise jump to the list.
		if m.historyIdx >= 0 {
			return m.cycleAskHistory(-1)
		}
		cmd := m.setFocus(focusList)
		return m, cmd
	case "tab":
		cmd := m.setFocus(focusList)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// cycleAskHistory walks the saved-ask history. delta=+1 → older entry,
// delta=-1 → newer entry. At delta=-1 from idx 0, restores the user's
// in-progress text and exits cycling mode.
func (m hubModel) cycleAskHistory(delta int) (tea.Model, tea.Cmd) {
	if m.cachedHistory == nil {
		m.cachedHistory = loadAskHistory()
		if m.cachedHistory == nil {
			m.cachedHistory = []AskHistoryEntry{}
		}
	}
	if len(m.cachedHistory) == 0 {
		return m, nil
	}
	if m.historyIdx == -1 && delta > 0 {
		m.savedInput = m.input.Value()
	}
	newIdx := m.historyIdx + delta
	if newIdx >= len(m.cachedHistory) {
		newIdx = len(m.cachedHistory) - 1
	}
	if newIdx < -1 {
		newIdx = -1
	}
	m.historyIdx = newIdx
	if m.historyIdx == -1 {
		m.input.SetValue(m.savedInput)
	} else {
		m.input.SetValue(m.cachedHistory[m.historyIdx].Text)
	}
	m.input.CursorEnd()
	return m, nil
}

func (m hubModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While filtering inside the list, let the list consume keystrokes.
	if m.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.result = hubResult{action: actQuit}
		return m, tea.Quit
	case "esc":
		// Esc from list bounces back to the input (less destructive than quit).
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "tab":
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "up":
		if m.list.Index() == 0 {
			cmd := m.setFocus(focusInput)
			return m, cmd
		}
	case "enter":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actLaunch, profile: it}
			return m, tea.Quit
		}
	case "a":
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "n":
		m.result = hubResult{action: actNew}
		return m, tea.Quit
	case "g":
		m.result = hubResult{action: actGenerate}
		return m, tea.Quit
	case "e":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actEdit, profile: it}
			return m, tea.Quit
		}
	case "d":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actDelete, profile: it}
			return m, tea.Quit
		}
	case "c":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actCopy, profile: it}
			return m, tea.Quit
		}
	case "x":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actExport, profile: it}
			return m, tea.Quit
		}
	case "i":
		m.result = hubResult{action: actImport}
		return m, tea.Quit
	case "r":
		m.result = hubResult{action: actRepo}
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m hubModel) selectedID() string {
	it, ok := m.list.SelectedItem().(profileItem)
	if !ok {
		return ""
	}
	return it.loc.QualifiedID
}

func (m hubModel) View() string {
	inputView := inputBlockStyle().Render(askPromptStyle.Render("Ask  ") + m.input.View())
	return inputView + "\n" + m.list.View() + "\n" + m.help
}

// ── Public entrypoint ─────────────────────────────────────────────────────────

func runHub() hubResult {
	locs, _ := listAllLocations()
	sortLocationsByRecency(locs, loadRecents())
	running := runningByProfile()
	bg := backgroundedByProfile()
	items := make([]list.Item, len(locs))
	for i, loc := range locs {
		items[i] = profileItem{
			loc:      loc,
			titleStr: hubTitle(loc, running[loc.QualifiedID], bg[loc.QualifiedID]),
			descStr:  hubDesc(loc),
		}
	}

	// Focused delegate: selected row highlighted coral.
	focused := list.NewDefaultDelegate()
	focused.SetSpacing(1)
	focused.ShowDescription = true
	focused.Styles.SelectedTitle = focused.Styles.SelectedTitle.
		Foreground(cdsCoral).
		BorderForeground(cdsCoral).
		Bold(true)
	focused.Styles.SelectedDesc = focused.Styles.SelectedDesc.
		Foreground(cdsCoral).
		BorderForeground(cdsCoral)
	focused.Styles.NormalTitle = focused.Styles.NormalTitle.Foreground(cdsInk)
	focused.Styles.NormalDesc = focused.Styles.NormalDesc.Foreground(cdsMuted)
	focused.Styles.DimmedTitle = focused.Styles.DimmedTitle.Foreground(cdsMuted)
	focused.Styles.DimmedDesc = focused.Styles.DimmedDesc.Foreground(cdsMuted)
	focused.Styles.FilterMatch = focused.Styles.FilterMatch.
		Foreground(cdsAmber).
		Bold(true)

	// Unfocused delegate: selected row is rendered like a normal row, so when
	// the user is typing in the Ask input the list shows no "selected" cue.
	unfocused := focused
	unfocused.Styles.SelectedTitle = focused.Styles.NormalTitle
	unfocused.Styles.SelectedDesc = focused.Styles.NormalDesc

	l := list.New(items, unfocused, 0, 0)
	l.Title = hubTitleBar()
	l.Styles.Title = lipgloss.NewStyle() // pre-rendered; no wrapping style
	l.Styles.StatusBar = l.Styles.StatusBar.Foreground(cdsMuted)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.SetShowStatusBar(false)

	ti := textinput.New()
	ti.Placeholder = "Describe what you want to do — ↑ recall past asks · ↓/Tab → list · Enter to ask"
	ti.PromptStyle = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	ti.Prompt = "› "
	ti.TextStyle = lipgloss.NewStyle().Foreground(cdsInk)
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(cdsMuted).Italic(true)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(cdsCoral)
	ti.Focus()

	m := hubModel{
		list:              l,
		input:             ti,
		focus:             focusInput,
		help:              hubHelpFooter(),
		historyIdx:        -1,
		focusedDelegate:   focused,
		unfocusedDelegate: unfocused,
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	out, err := p.Run()
	if err != nil {
		return hubResult{action: actQuit}
	}
	final, _ := out.(hubModel)
	return final.result
}

// sortLocationsByRecency reorders locs so the most-recently-launched profile
// is first. Profiles that have never been launched (absent from `recents`)
// fall to the bottom in alphabetical order.
func sortLocationsByRecency(locs []ProfileLocation, recents map[string]int64) {
	sort.SliceStable(locs, func(i, j int) bool {
		ti, hasI := recents[locs[i].QualifiedID]
		tj, hasJ := recents[locs[j].QualifiedID]
		if hasI && hasJ {
			return ti > tj
		}
		if hasI != hasJ {
			return hasI
		}
		return locs[i].QualifiedID < locs[j].QualifiedID
	})
}

// ── Rendering helpers ─────────────────────────────────────────────────────────

var (
	hubKeyStyle    = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	hubDimStyle    = lipgloss.NewStyle().Foreground(cdsMuted)
	askPromptStyle = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
)

func inputBlockStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cdsCoral).
		Padding(0, 1)
}

func hubTitle(loc ProfileLocation, running []RunningWrapper, bg []BackgroundedSession) string {
	source := "local"
	switch {
	case loc.RepoAlias == ".":
		source = "project"
	case loc.RepoAlias != "":
		source = "repo:" + loc.RepoAlias
	}
	tags := []string{hubDimStyle.Render(source)}
	if n := len(bg); n > 0 {
		marker := "● bg"
		if n > 1 {
			marker = fmt.Sprintf("● bg ×%d", n)
		}
		tags = append([]string{lipgloss.NewStyle().Foreground(cdsCoral).Bold(true).Render(marker)}, tags...)
	}
	if n := len(running); n > 0 {
		marker := "● running"
		if n > 1 {
			marker = fmt.Sprintf("● running ×%d", n)
		}
		tags = append([]string{lipgloss.NewStyle().Foreground(cdsSage).Bold(true).Render(marker)}, tags...)
	}

	p, err := loadProfileAt(loc.JSONPath)
	if err == nil {
		if len(p.McpServers) > 0 {
			names := make([]string, 0, len(p.McpServers))
			for k := range p.McpServers {
				names = append(names, k)
			}
			tags = append(tags, strings.Join(names, ","))
		}
		if len(p.DeniedTools) > 0 {
			tags = append(tags, fmt.Sprintf("deny:%d", len(p.DeniedTools)))
		}
		if p.Isolated {
			tags = append(tags, "isolated")
		}
		if kinds := profilePluginKinds(loc); len(kinds) > 0 {
			tags = append(tags, "+"+strings.Join(kinds, "/"))
		}
		s := parseSettings(p.Settings)
		if m := getModel(s); m != "" {
			tags = append(tags, "model:"+m)
		}
		if pm := getPermissionMode(s); pm != "" {
			tags = append(tags, "mode:"+pm)
		}
	}
	return loc.QualifiedID + "  " + hubDimStyle.Render("· "+strings.Join(tags, " · "))
}

func hubDesc(loc ProfileLocation) string {
	p, err := loadProfileAt(loc.JSONPath)
	if err == nil && p.Description != "" {
		return p.Description
	}
	return hubDimStyle.Render("(no description)")
}

func currentGitBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// formatVersion makes the version display-friendly.
// Proper semver (v1.2.3) is returned as-is.
// Pseudo-versions are shortened to a 7-char hash (with * suffix if dirty).
func formatVersion(v string) string {
	if v == "dev" {
		return "dev"
	}
	dirty := strings.HasSuffix(v, "+dirty")
	clean := strings.TrimSuffix(v, "+dirty")
	parts := strings.Split(clean, "-")
	if len(parts) >= 3 {
		hash := parts[len(parts)-1]
		if len(hash) > 7 {
			hash = hash[:7]
		}
		if dirty {
			return hash + "*"
		}
		return hash
	}
	return v
}

func hubTitleBar() string {
	badge := lipgloss.NewStyle().
		Bold(true).
		Foreground(cdsCream).
		Background(cdsCoral).
		Padding(0, 1).
		Render("claude-profiles")

	cwd, _ := os.Getwd()
	parts := []string{formatVersion(version), shortenCwd(cwd)}
	if branch := currentGitBranch(); branch != "" && branch != "HEAD" {
		parts = append(parts, branch)
	}
	status := lipgloss.NewStyle().
		Foreground(cdsMuted).
		Padding(0, 1).
		Render(strings.Join(parts, "  ·  "))

	return badge + status
}

func hubHelpFooter() string {
	keys := []struct{ k, v string }{
		{"↵", "ask/launch"},
		{"↓", "list"},
		{"n", "new"},
		{"g", "generate (AI)"},
		{"e", "edit"},
		{"d", "delete"},
		{"c", "copy"},
		{"x", "export"},
		{"i", "import"},
		{"r", "repos"},
		{"/", "filter"},
		{"q", "quit"},
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = hubKeyStyle.Render(k.k) + " " + k.v
	}
	return hubDimStyle.Render(strings.Join(parts, " · "))
}
