package main

import (
	"fmt"
	"io"
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
	actEdit     hubAction = "edit"
	actDelete   hubAction = "delete"
	actCopy     hubAction = "copy"
	actExport   hubAction = "export"
	actImport   hubAction = "import"
	actRepo     hubAction = "repo"
	actPin      hubAction = "pin"
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
	loc              ProfileLocation
	titleStr         string
	descStr          string
	pinnedPromptText string // pre-resolved prompt text; empty if not pinned or no prompt set
}

func (p profileItem) Title() string       { return p.titleStr }
func (p profileItem) Description() string { return p.descStr }
func (p profileItem) FilterValue() string { return p.loc.QualifiedID }

// sectionHeaderItem is a non-selectable divider inserted between profile groups.
// FilterValue returns "" so headers disappear when the user activates the filter.
type sectionHeaderItem struct{ label string }

func (h sectionHeaderItem) Title() string       { return h.label }
func (h sectionHeaderItem) Description() string { return "" }
func (h sectionHeaderItem) FilterValue() string { return "" }

// hubDelegate renders sectionHeaderItems as styled dividers and profileItems
// using focused/unfocused coral highlight depending on whether the list pane
// currently has keyboard focus.
type hubDelegate struct {
	focused   list.DefaultDelegate
	unfocused list.DefaultDelegate
	isFocused bool
}

func (d hubDelegate) Height() int  { return d.focused.Height() }
func (d hubDelegate) Spacing() int { return d.focused.Spacing() }

func (d hubDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return d.focused.Update(msg, m)
}

func (d hubDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if h, ok := item.(sectionHeaderItem); ok {
		label := sectionLabelStyle.Render(h.label)
		rule := sectionRuleStyle.Render(strings.Repeat("─", 4))
		fmt.Fprintln(w, "  "+rule+" "+label+" "+rule)
		return
	}
	if d.isFocused {
		d.focused.Render(w, m, index, item)
	} else {
		d.unfocused.Render(w, m, index, item)
	}
}

type hubModel struct {
	list   list.Model
	input  textinput.Model
	focus  focusTarget
	result hubResult
	width  int
	height int
	showOtherActions bool

	// delegate renders profileItems with coral highlight when the list pane has
	// focus and sectionHeaderItems as styled dividers in all cases.
	delegate hubDelegate

	// Ask-prompt history cycling (shell-style ↑/↓ in the input).
	// historyIdx == -1 means "not cycling" — the input value is the user's
	// own in-progress text. 0 is the most recent saved entry, growing older.
	cachedHistory []AskHistoryEntry
	historyIdx    int
	savedInput    string // user's in-progress text, restored when cycle ends
}

// setFocus moves focus and updates the delegate so the selection highlight
// only appears when the list is the active pane.
func (m *hubModel) setFocus(t focusTarget) tea.Cmd {
	m.focus = t
	m.showOtherActions = false
	if t == focusInput {
		m.input.Focus()
		m.delegate.isFocused = false
		m.list.SetDelegate(m.delegate)
		return textinput.Blink
	}
	m.input.Blur()
	m.delegate.isFocused = true
	m.list.SetDelegate(m.delegate)
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
		// padding(1) + title(1) + input-box(3) + spacer(1) + footer(1) = 7 overhead
		listH := msg.Height - 7
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
		if m.showOtherActions {
			m.showOtherActions = false
			return m, nil
		}
		// Esc from list bounces back to the input (less destructive than quit).
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "?":
		m.showOtherActions = !m.showOtherActions
		return m, nil
	case "tab":
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "up":
		if m.list.Index() == firstSelectableIndex(m.list.Items()) {
			cmd := m.setFocus(focusInput)
			return m, cmd
		}
	case "p":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actPin, profile: it}
			return m, tea.Quit
		}
	case "enter":
		if it, ok := m.list.SelectedItem().(profileItem); ok && it.loc.QualifiedID != "" {
			m.result = hubResult{action: actLaunch, profile: it.loc.QualifiedID, prompt: it.pinnedPromptText}
			return m, tea.Quit
		}
	case "a":
		cmd := m.setFocus(focusInput)
		return m, cmd
	case "n":
		m.result = hubResult{action: actNew}
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
	prevIdx := m.list.Index()
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	m.skipHeaders(prevIdx)
	return m, cmd
}

// skipHeaders advances the cursor past any sectionHeaderItem the list just
// landed on. Direction is inferred from prevIdx vs the current index.
func (m *hubModel) skipHeaders(prevIdx int) {
	if _, isHeader := m.list.SelectedItem().(sectionHeaderItem); !isHeader {
		return
	}
	items := m.list.Items()
	idx := m.list.Index()
	n := len(items)
	forward := idx >= prevIdx
	if forward {
		for i := idx + 1; i < n; i++ {
			if _, ok := items[i].(sectionHeaderItem); !ok {
				m.list.Select(i)
				return
			}
		}
		for i := idx - 1; i >= 0; i-- {
			if _, ok := items[i].(sectionHeaderItem); !ok {
				m.list.Select(i)
				return
			}
		}
	} else {
		for i := idx - 1; i >= 0; i-- {
			if _, ok := items[i].(sectionHeaderItem); !ok {
				m.list.Select(i)
				return
			}
		}
		for i := idx + 1; i < n; i++ {
			if _, ok := items[i].(sectionHeaderItem); !ok {
				m.list.Select(i)
				return
			}
		}
	}
}

// firstSelectableIndex returns the index of the first non-header item.
func firstSelectableIndex(items []list.Item) int {
	for i, it := range items {
		if _, ok := it.(sectionHeaderItem); !ok {
			return i
		}
	}
	return 0
}

func (m hubModel) selectedID() string {
	it, ok := m.list.SelectedItem().(profileItem)
	if !ok {
		return ""
	}
	return it.loc.QualifiedID
}

func (m hubModel) View() string {
	inputView := inputBlockStyle(m.focus == focusInput).Render(askPromptStyle.Render("Ask  ") + m.input.View())
	return "\n" + hubTitleBar() + "\n" + inputView + "\n" + m.list.View() + "\n" + m.hubHelpFooter()
}

// ── Public entrypoint ─────────────────────────────────────────────────────────

func runHub() hubResult {
	locs, _ := listAllLocations()
	pins := loadPins()
	pinMap := map[string]PinEntry{}
	for _, pe := range pins {
		pinMap[pe.ProfileID] = pe
	}
	sortLocationsByRecency(locs, loadRecents())
	running := runningByProfile()
	bg := backgroundedByProfile()

	// Classify locations into display groups.
	var projectLocs, userLocs []ProfileLocation
	repoLocs := map[string][]ProfileLocation{}
	for _, loc := range locs {
		switch {
		case loc.RepoAlias == ".":
			projectLocs = append(projectLocs, loc)
		case loc.RepoAlias != "":
			repoLocs[loc.RepoAlias] = append(repoLocs[loc.RepoAlias], loc)
		default:
			userLocs = append(userLocs, loc)
		}
	}
	sortedAliases := make([]string, 0, len(repoLocs))
	for alias := range repoLocs {
		sortedAliases = append(sortedAliases, alias)
	}
	sort.Strings(sortedAliases)

	makeProfileItem := func(loc ProfileLocation, pinned bool, promptName, promptText string) profileItem {
		return profileItem{
			loc:              loc,
			titleStr:         hubTitle(loc, running[loc.QualifiedID], bg[loc.QualifiedID], pinned, promptName),
			descStr:          hubDesc(loc),
			pinnedPromptText: promptText,
		}
	}

	// Pinned profiles appear twice: once at the top as a quick-launch entry
	// (with the associated prompt auto-selected on Enter) and again at their
	// natural recency-sorted position so all prompts remain accessible.
	var items []list.Item
	if len(pins) > 0 {
		items = append(items, sectionHeaderItem{label: "Pinned"})
		for _, pe := range pins {
			for _, loc := range locs {
				if loc.QualifiedID != pe.ProfileID {
					continue
				}
				pinnedPromptText := ""
				if pe.PromptName != "" {
					if p, err := loadProfileAt(loc.JSONPath); err == nil {
						for _, pp := range p.Prompts {
							if pp.Name == pe.PromptName {
								pinnedPromptText = pp.Text
								break
							}
						}
					}
				}
				items = append(items, makeProfileItem(loc, true, pe.PromptName, pinnedPromptText))
				break
			}
		}
	}
	if len(projectLocs) > 0 {
		items = append(items, sectionHeaderItem{label: "Project"})
		for _, loc := range projectLocs {
			items = append(items, makeProfileItem(loc, false, "", ""))
		}
	}
	if len(userLocs) > 0 {
		items = append(items, sectionHeaderItem{label: "User"})
		for _, loc := range userLocs {
			items = append(items, makeProfileItem(loc, false, "", ""))
		}
	}
	for _, alias := range sortedAliases {
		items = append(items, sectionHeaderItem{label: alias})
		for _, loc := range repoLocs[alias] {
			items = append(items, makeProfileItem(loc, false, "", ""))
		}
	}

	// Focused delegate: selected row highlighted coral.
	focusedDel := list.NewDefaultDelegate()
	focusedDel.SetSpacing(1)
	focusedDel.ShowDescription = true
	focusedDel.Styles.SelectedTitle = focusedDel.Styles.SelectedTitle.
		Foreground(cdsCoral).
		BorderForeground(cdsCoral).
		Bold(true)
	focusedDel.Styles.SelectedDesc = focusedDel.Styles.SelectedDesc.
		Foreground(cdsCoral).
		BorderForeground(cdsCoral)
	focusedDel.Styles.NormalTitle = focusedDel.Styles.NormalTitle.Foreground(cdsInk)
	focusedDel.Styles.NormalDesc = focusedDel.Styles.NormalDesc.Foreground(cdsMuted)
	focusedDel.Styles.DimmedTitle = focusedDel.Styles.DimmedTitle.Foreground(cdsMuted)
	focusedDel.Styles.DimmedDesc = focusedDel.Styles.DimmedDesc.Foreground(cdsMuted)
	focusedDel.Styles.FilterMatch = focusedDel.Styles.FilterMatch.
		Foreground(cdsAmber).
		Bold(true)

	// Unfocused delegate: selected row is rendered like a normal row, so when
	// the user is typing in the Ask input the list shows no "selected" cue.
	unfocusedDel := focusedDel
	unfocusedDel.Styles.SelectedTitle = focusedDel.Styles.NormalTitle
	unfocusedDel.Styles.SelectedDesc = focusedDel.Styles.NormalDesc

	del := hubDelegate{focused: focusedDel, unfocused: unfocusedDel, isFocused: false}

	l := list.New(items, del, 0, 0)
	l.SetShowTitle(false)
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
		list:       l,
		input:      ti,
		focus:      focusInput,
		historyIdx: -1,
		delegate:   del,
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
// is first. Profiles that have never been launched fall to the bottom alphabetically.
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
	hubKeyStyle      = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	hubDimStyle      = lipgloss.NewStyle().Foreground(cdsMuted)
	askPromptStyle   = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	sectionLabelStyle = lipgloss.NewStyle().Foreground(cdsInk).Bold(true)
	sectionRuleStyle  = lipgloss.NewStyle().Foreground(cdsMuted)
)

func inputBlockStyle(focused bool) lipgloss.Style {
	borderColor := cdsMuted
	if focused {
		borderColor = cdsCoral
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)
}

func hubTitle(loc ProfileLocation, running []RunningWrapper, bg []BackgroundedSession, pinned bool, pinnedPromptName string) string {
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
		if p.Cwd != "" {
			tags = append(tags, "cwd")
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

	pinStyle := lipgloss.NewStyle().Foreground(cdsAmber).Bold(true)
	prefix := ""
	if pinned {
		prefix = pinStyle.Render("★") + " "
		if pinnedPromptName != "" {
			tags = append([]string{pinStyle.Render("[" + pinnedPromptName + "]")}, tags...)
		}
	}
	return prefix + loc.QualifiedID + "  " + hubDimStyle.Render("· "+strings.Join(tags, " · "))
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

func (m hubModel) hubHelpFooter() string {
	var keys []struct{ k, v string }
	switch {
	case m.focus == focusInput:
		keys = []struct{ k, v string }{
			{"↵", "ask"},
			{"↓", "list"},
			{"q", "quit"},
		}
	case m.showOtherActions:
		keys = []struct{ k, v string }{
			{"p", "pin/unpin"},
			{"c", "copy"},
			{"x", "export"},
			{"i", "import"},
			{"r", "repos"},
			{"esc", "back"},
		}
	default:
		keys = []struct{ k, v string }{
			{"↵", "launch"},
			{"n", "new"},
			{"e", "edit"},
			{"d", "delete"},
			{"/", "filter"},
			{"?", "other"},
			{"q", "quit"},
		}
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = hubKeyStyle.Render(k.k) + " " + k.v
	}
	return hubDimStyle.Render(strings.Join(parts, " · "))
}
