package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Hub view ──────────────────────────────────────────────────────────────────
//
// Single-input model:
//   - the text input is always focused; typing filters the list as you type.
//   - ↑/↓ navigate the list (the input never moves them anywhere).
//   - Enter on a profile row launches it; Enter on the synthetic "Ask Claude"
//     row at the bottom sends the text to the classifier.
//   - When the query is empty, the list shows the full sectioned view; when
//     non-empty, it flattens to matches + the Ask row.
//   - Tab opens an action palette overlay for setup actions (n/e/d/...) so
//     letter keys never have to compete with the input.

type hubAction string

const (
	actLaunch    hubAction = "launch"
	actAsk       hubAction = "ask"
	actNew       hubAction = "new"
	actEdit      hubAction = "edit"
	actDelete    hubAction = "delete"
	actCopy      hubAction = "copy"
	actExport    hubAction = "export"
	actImport    hubAction = "import"
	actRepo      hubAction = "repo"
	actPin       hubAction = "pin"
	actAnalytics hubAction = "analytics"
	actQuit      hubAction = "quit"
)

type hubResult struct {
	action  hubAction
	profile string // qualified id; carries the prompt text for actAsk
	prompt  string // only set for actAsk
}

// profileItem is a launchable row.
type profileItem struct {
	loc              ProfileLocation
	titleStr         string
	descStr          string
	pinnedPromptText string // pre-resolved prompt text; empty if not pinned or no prompt set
	filterStr        string // QualifiedID + description (lowercased) for substring matching
}

func (p profileItem) Title() string       { return p.titleStr }
func (p profileItem) Description() string { return p.descStr }
func (p profileItem) FilterValue() string { return p.filterStr }

// sectionHeaderItem is a non-selectable divider inserted between profile groups.
type sectionHeaderItem struct{ label string }

func (h sectionHeaderItem) Title() string       { return h.label }
func (h sectionHeaderItem) Description() string { return "" }
func (h sectionHeaderItem) FilterValue() string { return "" }

// askItem is a synthetic row shown at the bottom of the list when the user
// has typed a query. Selecting it sends the text to the Ask classifier.
type askItem struct{ query string }

func (a askItem) Title() string       { return a.query }
func (a askItem) Description() string { return "" }
func (a askItem) FilterValue() string { return "" }

// hubDelegate renders the three item kinds with appropriate styles.
type hubDelegate struct {
	rowDelegate list.DefaultDelegate
}

func (d hubDelegate) Height() int  { return d.rowDelegate.Height() }
func (d hubDelegate) Spacing() int { return d.rowDelegate.Spacing() }

func (d hubDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return d.rowDelegate.Update(msg, m)
}

func (d hubDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	switch v := item.(type) {
	case sectionHeaderItem:
		label := sectionLabelStyle.Render(v.label)
		rule := sectionRuleStyle.Render(strings.Repeat("─", 4))
		fmt.Fprintln(w, "  "+rule+" "+label+" "+rule)
	case askItem:
		selected := index == m.Index()
		arrow := "↵"
		titleText := "Ask Claude with this text"
		query := v.query
		if query != "" {
			titleText = fmt.Sprintf("Ask Claude  ›  %q", truncate(query, 60))
		}
		var title, desc string
		if selected {
			title = askSelectedTitleStyle.Render(arrow + "  " + titleText)
			desc = askSelectedDescStyle.Render("    let Claude classify and pick the best profile")
		} else {
			title = askIdleTitleStyle.Render(arrow + "  " + titleText)
			desc = askIdleDescStyle.Render("    let Claude classify and pick the best profile")
		}
		fmt.Fprintln(w, title)
		fmt.Fprintln(w, desc)
	default:
		d.rowDelegate.Render(w, m, index, item)
	}
}

type hubModel struct {
	list   list.Model
	input  textinput.Model
	result hubResult
	width  int
	height int

	paletteOpen bool
	detailMode  bool
	showHidden  bool

	// Raw data for rebuilding list items after hide/unhide.
	locs       []ProfileLocation
	pins       []PinEntry
	pinMap     map[string]PinEntry
	runningMap map[string][]RunningWrapper
	bgMap      map[string][]BackgroundedSession
	prefsStore ProfilePrefsStore

	delegate hubDelegate

	// Pre-built full item list (sectioned, query-empty view).
	fullItems []list.Item

	// Pre-built flat list of just profileItems for filtering.
	flatProfiles []profileItem
}

func (m hubModel) Init() tea.Cmd {
	return textinput.Blink
}

// rebuildIndex precomputes both the sectioned full view and the flat profile
// list used for filtering. Called once at startup and after hide/unhide.
func (m *hubModel) rebuildIndex() {
	m.fullItems = m.buildSectionedItems()
	m.flatProfiles = m.buildFlatProfiles()
}

// applyFilter updates the list contents based on the current input query.
// Empty query → full sectioned view. Non-empty → matching profileItems
// followed by the askItem.
func (m *hubModel) applyFilter() {
	query := strings.TrimSpace(m.input.Value())
	if query == "" {
		m.list.SetItems(m.fullItems)
		// Cursor to first selectable item.
		if idx := firstSelectableIndex(m.fullItems); idx >= 0 {
			m.list.Select(idx)
		}
		return
	}
	needle := strings.ToLower(query)
	var matches []list.Item
	for _, p := range m.flatProfiles {
		if strings.Contains(p.filterStr, needle) {
			matches = append(matches, p)
		}
	}
	matches = append(matches, askItem{query: query})
	m.list.SetItems(matches)
	m.list.Select(0)
}

// buildFlatProfiles returns every non-hidden profile as a profileItem,
// excluding section headers — used as the corpus for filtering.
func (m *hubModel) buildFlatProfiles() []profileItem {
	isHidden := func(loc ProfileLocation) bool {
		return m.prefsStore[filepath.Dir(loc.JSONPath)].Hidden
	}
	pinnedSet := map[string]bool{}
	pinPromptName := map[string]string{}
	for _, pe := range m.pins {
		pinnedSet[pe.ProfileID] = true
		pinPromptName[pe.ProfileID] = pe.PromptName
	}
	resolvePromptText := func(loc ProfileLocation, promptName string) string {
		if promptName == "" {
			return ""
		}
		p, err := loadProfileAt(loc.JSONPath)
		if err != nil {
			return ""
		}
		for _, pp := range p.Prompts {
			if pp.Name == promptName {
				return pp.Text
			}
		}
		return ""
	}

	var visible []ProfileLocation
	for _, loc := range m.locs {
		if isHidden(loc) && !m.showHidden {
			continue
		}
		visible = append(visible, loc)
	}
	modal := computeModalTags(visible)

	var out []profileItem
	for _, loc := range visible {
		pinned := pinnedSet[loc.QualifiedID]
		promptName := pinPromptName[loc.QualifiedID]
		promptText := ""
		if pinned {
			promptText = resolvePromptText(loc, promptName)
		}
		out = append(out, profileItem{
			loc:              loc,
			titleStr:         hubTitle(loc, m.runningMap[loc.QualifiedID], m.bgMap[loc.QualifiedID], pinned, promptName, modal, ""),
			descStr:          hubDesc(loc),
			pinnedPromptText: promptText,
			filterStr:        buildFilterString(loc),
		})
	}
	return out
}

// buildSectionedItems constructs the sectioned view (Pinned, Project, User,
// repo:*, Hidden). Profiles marked hidden in prefsStore are excluded from
// their normal sections and collected into a "Hidden" section at the bottom.
func (m *hubModel) buildSectionedItems() []list.Item {
	isHidden := func(loc ProfileLocation) bool {
		return m.prefsStore[filepath.Dir(loc.JSONPath)].Hidden
	}

	// Modal tags are computed over the visible (non-hidden) corpus so the
	// "common case" reflects what's actually on screen.
	var visible []ProfileLocation
	for _, loc := range m.locs {
		if !isHidden(loc) {
			visible = append(visible, loc)
		}
	}
	modal := computeModalTags(visible)

	makeItem := func(loc ProfileLocation, pinned bool, promptName, promptText, sectionRepoAlias string) profileItem {
		return profileItem{
			loc:              loc,
			titleStr:         hubTitle(loc, m.runningMap[loc.QualifiedID], m.bgMap[loc.QualifiedID], pinned, promptName, modal, sectionRepoAlias),
			descStr:          hubDesc(loc),
			pinnedPromptText: promptText,
			filterStr:        buildFilterString(loc),
		}
	}

	var projectLocs, userLocs, hiddenLocs []ProfileLocation
	repoLocs := map[string][]ProfileLocation{}
	for _, loc := range m.locs {
		if isHidden(loc) {
			hiddenLocs = append(hiddenLocs, loc)
			continue
		}
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

	pinnedSet := map[string]bool{}
	pinPromptName := map[string]string{}
	for _, pe := range m.pins {
		pinnedSet[pe.ProfileID] = true
		pinPromptName[pe.ProfileID] = pe.PromptName
	}

	var items []list.Item

	// Pinned section: rows do NOT carry the ★ prefix (the header is the marker
	// here) but they DO carry the [promptName] tag — that's what distinguishes
	// this view of the profile from its natural-section duplicate.
	if len(m.pins) > 0 {
		var pinnedItems []list.Item
		for _, pe := range m.pins {
			for _, loc := range m.locs {
				if loc.QualifiedID != pe.ProfileID || isHidden(loc) {
					continue
				}
				promptText := ""
				if pe.PromptName != "" {
					if p, err := loadProfileAt(loc.JSONPath); err == nil {
						for _, pp := range p.Prompts {
							if pp.Name == pe.PromptName {
								promptText = pp.Text
								break
							}
						}
					}
				}
				// pinned=false so the ★ does not appear in this section;
				// promptName is passed so [name] tag still shows.
				pinnedItems = append(pinnedItems, makeItem(loc, false, pe.PromptName, promptText, ""))
				break
			}
		}
		if len(pinnedItems) > 0 {
			items = append(items, sectionHeaderItem{label: "Pinned"})
			items = append(items, pinnedItems...)
		}
	}

	addLocs := func(label string, locs []ProfileLocation, sectionAlias string) {
		if len(locs) == 0 {
			return
		}
		items = append(items, sectionHeaderItem{label: label})
		for _, loc := range locs {
			pinned := pinnedSet[loc.QualifiedID]
			items = append(items, makeItem(loc, pinned, "", "", sectionAlias))
		}
	}
	addLocs("Project", projectLocs, "")
	addLocs("User", userLocs, "")
	for _, alias := range sortedAliases {
		addLocs(alias, repoLocs[alias], alias)
	}

	if len(hiddenLocs) > 0 {
		if m.showHidden {
			items = append(items, sectionHeaderItem{label: fmt.Sprintf("Hidden (%d)  ·  Tab → palette → H to collapse", len(hiddenLocs))})
			for _, loc := range hiddenLocs {
				pinned := pinnedSet[loc.QualifiedID]
				items = append(items, makeItem(loc, pinned, "", "", ""))
			}
		} else {
			items = append(items, sectionHeaderItem{label: fmt.Sprintf("Hidden (%d)  ·  Tab → palette → H to reveal", len(hiddenLocs))})
		}
	}

	return items
}

func (m hubModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = msg.Width - 6
		m.resizeListForLayout()
		return m, nil

	case tea.KeyMsg:
		if m.paletteOpen {
			return m.updatePalette(msg)
		}
		return m.updateMain(msg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// updateMain handles keystrokes in the default state: input is focused,
// arrow keys navigate the list, Enter launches/asks.
func (m hubModel) updateMain(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.result = hubResult{action: actQuit}
		return m, tea.Quit
	case "esc":
		if m.detailMode {
			m.detailMode = false
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.SetValue("")
			m.applyFilter()
			return m, nil
		}
		m.result = hubResult{action: actQuit}
		return m, tea.Quit
	case "tab":
		m.paletteOpen = true
		m.detailMode = false
		return m, nil
	case "enter":
		return m.activateSelection()
	case "up", "down":
		return m.navigateList(msg)
	case " ":
		// Space toggles the inline detail panel for the currently-selected
		// profile — but ONLY when the input is empty, so it doesn't conflict
		// with typing multi-word filter queries.
		if m.input.Value() == "" {
			if _, ok := m.list.SelectedItem().(profileItem); ok {
				m.detailMode = !m.detailMode
				m.resizeListForLayout()
			}
			return m, nil
		}
		// Otherwise fall through to the input so it appends a space to the query.
	}

	// Everything else: forward to input, then re-filter.
	prev := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != prev {
		m.applyFilter()
	}
	return m, cmd
}

// updatePalette handles keystrokes while the action palette is open.
func (m hubModel) updatePalette(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.result = hubResult{action: actQuit}
		return m, tea.Quit
	case "esc", "tab":
		m.paletteOpen = false
		return m, nil
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
	case "s":
		m.result = hubResult{action: actAnalytics}
		return m, tea.Quit
	case "p":
		if it := m.selectedID(); it != "" {
			m.result = hubResult{action: actPin, profile: it}
			return m, tea.Quit
		}
	case "h":
		// Hide / unhide the selected profile in place.
		if selectedID := m.selectedID(); selectedID != "" {
			for _, loc := range m.locs {
				if loc.QualifiedID != selectedID {
					continue
				}
				dir := filepath.Dir(loc.JSONPath)
				prefs := m.prefsStore[dir]
				prefs.Hidden = !prefs.Hidden
				if saveProfilePrefs(dir, prefs) == nil {
					m.prefsStore[dir] = prefs
					m.rebuildIndex()
					m.applyFilter()
				}
				break
			}
		}
		m.paletteOpen = false
		return m, nil
	case "H":
		m.showHidden = !m.showHidden
		m.rebuildIndex()
		m.applyFilter()
		m.paletteOpen = false
		return m, nil
	}
	return m, nil
}

// activateSelection acts on whatever is currently highlighted.
func (m hubModel) activateSelection() (tea.Model, tea.Cmd) {
	switch it := m.list.SelectedItem().(type) {
	case profileItem:
		if it.loc.QualifiedID == "" {
			return m, nil
		}
		m.result = hubResult{action: actLaunch, profile: it.loc.QualifiedID, prompt: it.pinnedPromptText}
		return m, tea.Quit
	case askItem:
		m.result = hubResult{action: actAsk, prompt: it.query}
		return m, tea.Quit
	}
	// Header or no selection: if input is non-empty, treat as ask.
	q := strings.TrimSpace(m.input.Value())
	if q != "" {
		m.result = hubResult{action: actAsk, prompt: q}
		return m, tea.Quit
	}
	return m, nil
}

// navigateList forwards an up/down keystroke to the list and skips any header
// row we land on.
func (m hubModel) navigateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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

// firstSelectableIndex returns the index of the first non-header item, or -1
// if no selectable item exists.
func firstSelectableIndex(items []list.Item) int {
	for i, it := range items {
		if _, ok := it.(sectionHeaderItem); !ok {
			return i
		}
	}
	return -1
}

func (m hubModel) selectedID() string {
	it, ok := m.list.SelectedItem().(profileItem)
	if !ok {
		return ""
	}
	return it.loc.QualifiedID
}

func (m hubModel) View() string {
	inputView := inputBlockStyle(!m.paletteOpen).Render(askPromptStyle.Render("›  ") + m.input.View())
	body := m.list.View()
	if m.paletteOpen {
		body = paletteOverlayStyle.Render(m.paletteContent())
	} else if m.detailMode && m.width > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, m.renderDetailPane())
	}
	return "\n" + hubTitleBar() + "\n" + inputView + "\n" + body + "\n" + m.hubHelpFooter()
}

// listWidth returns the column count the list should occupy under the current
// layout. In detail mode we hand roughly half the width to the detail pane.
func (m *hubModel) listWidth() int {
	if m.width <= 0 {
		return 0
	}
	if m.detailMode {
		// Left ~45% for the list, leave at least 30 cols if the terminal is wide
		// enough; otherwise split evenly.
		w := m.width * 9 / 20
		if w < 30 && m.width >= 60 {
			w = 30
		}
		if w < 20 {
			w = 20
		}
		return w
	}
	return m.width
}

// resizeListForLayout pushes the current width+height down to the list. Called
// on WindowSizeMsg and whenever detailMode toggles.
func (m *hubModel) resizeListForLayout() {
	if m.height <= 0 {
		return
	}
	// padding(1) + title(1) + input-box(3) + spacer(1) + footer(1) = 7 overhead
	listH := m.height - 7
	if listH < 4 {
		listH = 4
	}
	m.list.SetSize(m.listWidth(), listH)
}

// renderDetailPane builds the bordered detail panel shown to the right of the
// list when detailMode is active. Content is the same plain-text rendering
// that `show <profile>` would print.
func (m hubModel) renderDetailPane() string {
	id := m.selectedID()
	width := m.width - m.listWidth() - 2 // 2 cols for the border
	if width < 20 {
		width = 20
	}
	listH := m.height - 7
	if listH < 4 {
		listH = 4
	}
	contentH := listH - 2 // border eats 2 rows

	var body string
	if id == "" {
		body = hubDimStyle.Render("(no profile selected)")
	} else {
		loc, err := resolveProfileLocation(id)
		if err != nil {
			body = hubDimStyle.Render(fmt.Sprintf("(failed to resolve: %v)", err))
		} else {
			body = renderProfileDetail(loc)
		}
	}
	body = clampToBox(body, width-2, contentH)
	return detailPaneStyle.Width(width).Height(listH).Render(body)
}

// clampToBox wraps long lines at word boundaries to fit width w, then drops
// any overflow past h rows with a trailing ellipsis line. Wrapped continuation
// lines align with the indent of the source line so key/value pairs and
// bulleted entries still read as a block.
func clampToBox(s string, w, h int) string {
	if w <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapLine(line, w)...)
		if len(out) >= h {
			break
		}
	}
	if len(out) > h {
		out = out[:h-1]
		out = append(out, hubDimStyle.Render("…"))
	}
	return strings.Join(out, "\n")
}

// wrapLine breaks a single line at word boundaries to fit width w. Subsequent
// wrap lines are prefixed with the source line's leading whitespace so the
// visual indent carries over.
func wrapLine(line string, w int) []string {
	if len(line) <= w {
		return []string{line}
	}
	indent := ""
	for _, r := range line {
		if r != ' ' && r != '\t' {
			break
		}
		indent += string(r)
	}
	if len(indent) >= w {
		// Pathological indent vs width — fall back to a hard cut to avoid
		// infinite-loop wrap.
		return []string{line[:w]}
	}
	rest := strings.TrimLeft(line, " \t")
	words := strings.Fields(rest)
	if len(words) == 0 {
		return []string{line}
	}
	var lines []string
	current := indent
	for _, word := range words {
		// If a single word is wider than the remaining budget, hard-break it.
		for len(word) > w-len(indent) {
			if len(current) > len(indent) {
				lines = append(lines, current)
				current = indent
			}
			cut := w - len(indent)
			lines = append(lines, indent+word[:cut])
			word = word[cut:]
		}
		candidate := current
		if len(current) > len(indent) {
			candidate += " "
		}
		candidate += word
		if len(candidate) > w {
			lines = append(lines, current)
			current = indent + word
			continue
		}
		current = candidate
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func (m hubModel) paletteContent() string {
	target := m.selectedID()
	header := paletteHeaderStyle.Render("Actions")
	if target != "" {
		header += "  " + hubDimStyle.Render("on "+target)
	}
	rows := []struct{ k, v string }{
		{"n", "new profile"},
		{"e", "edit selected"},
		{"d", "delete selected"},
		{"c", "copy selected"},
		{"x", "export selected"},
		{"i", "import"},
		{"p", "pin / unpin"},
		{"h", "hide / unhide"},
		{"H", "toggle Hidden section"},
		{"r", "manage repos"},
		{"s", "analytics"},
	}
	var lines []string
	lines = append(lines, header, "")
	for _, r := range rows {
		lines = append(lines, "  "+hubKeyStyle.Render(r.k)+"   "+r.v)
	}
	lines = append(lines, "", hubDimStyle.Render("  Esc / Tab to close"))
	return strings.Join(lines, "\n")
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

	// Row delegate: bold selected row, no foreground recolor — keeps coral
	// reserved for hotkeys/brand instead of also doing selection.
	row := list.NewDefaultDelegate()
	row.SetSpacing(1)
	row.ShowDescription = true
	row.Styles.SelectedTitle = row.Styles.SelectedTitle.
		Foreground(cdsInk).
		BorderForeground(cdsCoral).
		Bold(true)
	row.Styles.SelectedDesc = row.Styles.SelectedDesc.
		Foreground(cdsMuted).
		BorderForeground(cdsCoral)
	row.Styles.NormalTitle = row.Styles.NormalTitle.Foreground(cdsInk)
	row.Styles.NormalDesc = row.Styles.NormalDesc.Foreground(cdsMuted)
	row.Styles.DimmedTitle = row.Styles.DimmedTitle.Foreground(cdsMuted)
	row.Styles.DimmedDesc = row.Styles.DimmedDesc.Foreground(cdsMuted)
	row.Styles.FilterMatch = row.Styles.FilterMatch.
		Foreground(cdsAmber).
		Bold(true)

	del := hubDelegate{rowDelegate: row}

	ti := textinput.New()
	ti.Placeholder = "Type to filter profiles, or describe what you want to do — Enter on the highlighted row"
	ti.PromptStyle = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	ti.Prompt = ""
	ti.TextStyle = lipgloss.NewStyle().Foreground(cdsInk)
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(cdsMuted).Italic(true)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(cdsCoral)
	ti.Focus()

	m := hubModel{
		input:    ti,
		delegate: del,
		locs:       locs,
		pins:       pins,
		pinMap:     pinMap,
		runningMap: running,
		bgMap:      bg,
		prefsStore: loadPrefsStore(),
	}
	m.rebuildIndex()

	l := list.New(m.fullItems, del, 0, 0)
	l.SetShowTitle(false)
	l.Styles.StatusBar = l.Styles.StatusBar.Foreground(cdsMuted)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false) // we manage filtering ourselves via the input
	l.SetShowStatusBar(false)
	if idx := firstSelectableIndex(m.fullItems); idx >= 0 {
		l.Select(idx)
	}
	m.list = l
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
	hubKeyStyle          = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	hubDimStyle          = lipgloss.NewStyle().Foreground(cdsMuted)
	askPromptStyle       = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	sectionLabelStyle    = lipgloss.NewStyle().Foreground(cdsInk).Bold(true)
	sectionRuleStyle     = lipgloss.NewStyle().Foreground(cdsMuted)
	askIdleTitleStyle    = lipgloss.NewStyle().Foreground(cdsMuted).Italic(true).PaddingLeft(2)
	askIdleDescStyle     = lipgloss.NewStyle().Foreground(cdsMuted).PaddingLeft(2)
	askSelectedTitleStyle = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true).PaddingLeft(2)
	askSelectedDescStyle  = lipgloss.NewStyle().Foreground(cdsMuted).PaddingLeft(2)
	paletteHeaderStyle   = lipgloss.NewStyle().Foreground(cdsCoral).Bold(true)
	paletteOverlayStyle  = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(cdsCoral).
				Padding(1, 2).
				MarginTop(1).
				MarginLeft(2)
	detailPaneStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(cdsMuted).
				Padding(0, 1).
				MarginLeft(1)
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

// modalTags captures the most common values for source/model/mode across the
// visible profile set, so hubTitle can elide them from row tag chains.
type modalTags struct {
	source string
	model  string
	mode   string
}

// computeModalTags returns the modal value for source, model, and mode across
// the given locations. A value is considered "modal" only if it covers at
// least half of profiles AND has at least 2 occurrences — otherwise eliding
// would hide a rare, distinguishing value.
func computeModalTags(locs []ProfileLocation) modalTags {
	if len(locs) < 2 {
		return modalTags{}
	}
	sourceCounts := map[string]int{}
	modelCounts := map[string]int{}
	modeCounts := map[string]int{}
	for _, loc := range locs {
		switch {
		case loc.RepoAlias == ".":
			sourceCounts["project"]++
		case loc.RepoAlias != "":
			sourceCounts["repo:"+loc.RepoAlias]++
		default:
			sourceCounts["local"]++
		}
		p, err := loadProfileAt(loc.JSONPath)
		if err != nil {
			continue
		}
		s := parseSettings(p.Settings)
		if m := getModel(s); m != "" {
			modelCounts[m]++
		}
		if pm := getPermissionMode(s); pm != "" {
			modeCounts[pm]++
		}
	}
	threshold := (len(locs) + 1) / 2 // ceil(n/2)
	pickModal := func(counts map[string]int) string {
		bestVal := ""
		bestN := 0
		for v, n := range counts {
			if n > bestN {
				bestN = n
				bestVal = v
			}
		}
		if bestN >= threshold && bestN >= 2 {
			return bestVal
		}
		return ""
	}
	return modalTags{
		source: pickModal(sourceCounts),
		model:  pickModal(modelCounts),
		mode:   pickModal(modeCounts),
	}
}

// hubTitle builds the row title with elision applied. sectionRepoAlias, when
// non-empty, strips the matching prefix from the displayed QualifiedID (the
// section header already labels the repo).
func hubTitle(loc ProfileLocation, running []RunningWrapper, bg []BackgroundedSession, pinned bool, pinnedPromptName string, modal modalTags, sectionRepoAlias string) string {
	source := "local"
	switch {
	case loc.RepoAlias == ".":
		source = "project"
	case loc.RepoAlias != "":
		source = "repo:" + loc.RepoAlias
	}

	var tags []string
	if source != modal.source {
		tags = append(tags, hubDimStyle.Render(source))
	}
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
			sort.Strings(names)
			tags = append(tags, strings.Join(names, ","))
		}
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
		s := parseSettings(p.Settings)
		if m := getModel(s); m != "" && m != modal.model {
			tags = append(tags, "model:"+m)
		}
		if pm := getPermissionMode(s); pm != "" && pm != modal.mode {
			tags = append(tags, "mode:"+pm)
		}
	}

	displayID := loc.QualifiedID
	if sectionRepoAlias != "" {
		prefix := sectionRepoAlias + "/"
		if strings.HasPrefix(displayID, prefix) {
			displayID = strings.TrimPrefix(displayID, prefix)
		}
	}

	pinStyle := lipgloss.NewStyle().Foreground(cdsAmber).Bold(true)
	prefix := ""
	if pinned {
		prefix = pinStyle.Render("★") + " "
	}
	if pinnedPromptName != "" {
		tags = append([]string{pinStyle.Render("[" + pinnedPromptName + "]")}, tags...)
	}
	if len(tags) == 0 {
		return prefix + displayID
	}
	return prefix + displayID + "  " + hubDimStyle.Render("· "+strings.Join(tags, " · "))
}

func hubDesc(loc ProfileLocation) string {
	p, err := loadProfileAt(loc.JSONPath)
	if err == nil && p.Description != "" {
		return p.Description
	}
	return hubDimStyle.Render("(no description)")
}

// buildFilterString returns lower-cased text combining the qualified ID and
// description, so typing a description keyword finds the profile.
func buildFilterString(loc ProfileLocation) string {
	parts := []string{loc.QualifiedID}
	if p, err := loadProfileAt(loc.JSONPath); err == nil {
		if p.Description != "" {
			parts = append(parts, p.Description)
		}
		for k := range p.McpServers {
			parts = append(parts, k)
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
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
	if m.paletteOpen {
		return hubDimStyle.Render("action palette open — press a letter, or Esc to close")
	}
	if m.detailMode {
		keys = []struct{ k, v string }{
			{"↑↓", "preview"},
			{"Space", "close details"},
			{"↵", "launch"},
			{"Esc", "back"},
		}
	} else {
		keys = []struct{ k, v string }{
			{"↵", "launch / ask"},
			{"↑↓", "navigate"},
			{"Space", "details"},
			{"type", "filter"},
			{"Tab", "actions"},
			{"Esc", "clear / quit"},
		}
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = hubKeyStyle.Render(k.k) + " " + k.v
	}
	return hubDimStyle.Render(strings.Join(parts, " · "))
}
