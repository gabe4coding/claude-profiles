package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const contextWindowLimit = 200_000 // tokens — all current Claude models

// modelPrices holds per-million-token prices: [input, output, cacheWrite, cacheRead].
// Matched by prefix: "opus" → opus tier, "haiku" → haiku tier, else → sonnet tier.
var modelPrices = map[string][4]float64{
	"sonnet": {3.00, 15.00, 3.75, 0.30},
	"opus":   {15.00, 75.00, 18.75, 1.50},
	"haiku":  {0.80, 4.00, 1.00, 0.08},
}

func priceTier(model string) string {
	switch {
	case strings.Contains(model, "opus"):
		return "opus"
	case strings.Contains(model, "haiku"):
		return "haiku"
	default:
		return "sonnet"
	}
}

func tokenCost(tier string, input, output, cacheCreate, cacheRead int) float64 {
	p := modelPrices[tier]
	const M = 1_000_000.0
	return (float64(input)*p[0] + float64(output)*p[1] +
		float64(cacheCreate)*p[2] + float64(cacheRead)*p[3]) / M
}

// modelUsage tracks per-model token counts within a session.
type modelUsage struct {
	Turns       int
	Input       int
	Output      int
	CacheCreate int
	CacheRead   int
}

func (m *modelUsage) cost(model string) float64 {
	return tokenCost(priceTier(model), m.Input, m.Output, m.CacheCreate, m.CacheRead)
}

// sessionMetrics holds derived stats for a single session file.
type sessionMetrics struct {
	SessionID        string
	Cwd              string
	Profile          string // qualified id, or "(no profile)"
	Project          string // base name of cwd
	TurnCount        int    // main-chain assistant turns (deduped by requestId)
	TotalInput       int
	TotalOutput      int
	TotalCacheRead   int
	TotalCacheCreate int
	PeakContext      int // max(input+cache_read+cache_create) across turns
	ToolCallCount    int
	SysPromptTokens  int // first turn's cache_read — pre-existing system cache
	FirstTurnCtx     int // first turn's total context (system prompt + initial content)
	ModelUsage       map[string]*modelUsage
}

func (s *sessionMetrics) totalCost() float64 {
	var total float64
	for model, u := range s.ModelUsage {
		total += u.cost(model)
	}
	return total
}

func (s sessionMetrics) cacheHitRatio() float64 {
	total := s.TotalCacheRead + s.TotalCacheCreate
	if total == 0 {
		return 0
	}
	return float64(s.TotalCacheRead) / float64(total)
}

func (s sessionMetrics) peakPercent() float64 {
	return float64(s.PeakContext) / float64(contextWindowLimit) * 100
}

// sysPct is the system prompt's share of peak context.
func (s sessionMetrics) sysPct() int {
	if s.PeakContext == 0 {
		return 0
	}
	return int(float64(s.SysPromptTokens) / float64(s.PeakContext) * 100)
}

// growthPct is the share of peak context added by conversation/tool accumulation
// (everything beyond the first turn's initial context load).
func (s sessionMetrics) growthPct() int {
	if s.PeakContext == 0 || s.FirstTurnCtx >= s.PeakContext {
		return 0
	}
	return int(float64(s.PeakContext-s.FirstTurnCtx) / float64(s.PeakContext) * 100)
}

type analyticsRawEvent struct {
	Type        string    `json:"type"`
	IsSidechain bool      `json:"isSidechain"`
	Cwd         string    `json:"cwd"`
	SessionID   string    `json:"sessionId"`
	RequestID   string    `json:"requestId"`
	Message     rawMessage `json:"message"`
}

type rawMessage struct {
	Model string `json:"model"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
	Content []struct {
		Type string `json:"type"`
	} `json:"content"`
}

func parseSessionFile(path string) *sessionMetrics {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)

	m := &sessionMetrics{ModelUsage: map[string]*modelUsage{}}
	seenReqIDs := map[string]bool{}
	firstTurn := true

	for scanner.Scan() {
		var ev analyticsRawEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if m.SessionID == "" && ev.SessionID != "" {
			m.SessionID = ev.SessionID
		}
		if m.Cwd == "" && ev.Cwd != "" {
			m.Cwd = ev.Cwd
		}
		// Only count main-chain assistant turns; subagent (isSidechain) traffic
		// inflates per-turn numbers without reflecting the user's own context usage.
		if ev.Type != "assistant" || ev.IsSidechain {
			continue
		}
		// Deduplicate: Claude Code writes multiple identical events per API
		// request (streaming chunks share the same requestId and final usage).
		if ev.RequestID != "" {
			if seenReqIDs[ev.RequestID] {
				continue
			}
			seenReqIDs[ev.RequestID] = true
		}

		u := ev.Message.Usage
		contextAtTurn := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens

		if firstTurn {
			m.SysPromptTokens = u.CacheReadInputTokens // pre-existing cache = system prompt
			m.FirstTurnCtx = contextAtTurn
			firstTurn = false
		}
		if contextAtTurn > m.PeakContext {
			m.PeakContext = contextAtTurn
		}

		m.TotalInput += u.InputTokens
		m.TotalOutput += u.OutputTokens
		m.TotalCacheRead += u.CacheReadInputTokens
		m.TotalCacheCreate += u.CacheCreationInputTokens
		m.TurnCount++

		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" {
				m.ToolCallCount++
			}
		}

		model := ev.Message.Model
		if model == "" {
			model = "unknown"
		}
		mu := m.ModelUsage[model]
		if mu == nil {
			mu = &modelUsage{}
			m.ModelUsage[model] = mu
		}
		mu.Turns++
		mu.Input += u.InputTokens
		mu.Output += u.OutputTokens
		mu.CacheCreate += u.CacheCreationInputTokens
		mu.CacheRead += u.CacheReadInputTokens
	}

	if m.SessionID == "" || m.TurnCount == 0 {
		return nil
	}
	m.Project = filepath.Base(m.Cwd)
	return m
}

func cmdAnalytics(_ []string) {
	ledger := loadSessionProfiles()

	projectsDir := filepath.Join(claudeRootDirPath(), "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		warn("Cannot read sessions directory: %v", err)
		return
	}

	var sessions []sessionMetrics
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, entry.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			m := parseSessionFile(filepath.Join(dir, f.Name()))
			if m == nil {
				continue
			}
			if p := ledger[m.SessionID]; p != "" {
				m.Profile = p
			} else {
				m.Profile = "(no profile)"
			}
			sessions = append(sessions, *m)
		}
	}

	if len(sessions) == 0 {
		info("No session data found.")
		return
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].PeakContext > sessions[j].PeakContext
	})

	sepStyle := lipgloss.NewStyle().Foreground(cdsMuted)
	boldStyle := lipgloss.NewStyle().Bold(true)
	amberStyle := lipgloss.NewStyle().Foreground(cdsAmber)
	dimStyle := lipgloss.NewStyle().Foreground(cdsMuted)

	sep := func() { fmt.Fprintln(os.Stderr, sepStyle.Render(strings.Repeat("─", 76))) }
	dot := func() { fmt.Fprintln(os.Stderr, sepStyle.Render("  "+strings.Repeat("·", 72))) }

	fmt.Fprintln(os.Stderr)
	title("=== Context Window Analytics ===")
	fmt.Fprintf(os.Stderr, "\n%s\n\n", styleInfo.Render(
		fmt.Sprintf("Scanned %d sessions across %d project(s)", len(sessions), countProjects(sessions))))

	// ── Legend ────────────────────────────────────────────────────────────

	legend := []struct{ key, desc string }{
		{"in tokens", "Σ(input + cache_read + cache_create) per turn, summed across all turns.\n" +
			"              Context is re-read in full every turn, so this greatly exceeds\n" +
			"              peak context × sessions — it reflects actual API processing load."},
		{"Peak / Sys%", "Highest single-turn context vs the 200k limit.\n" +
			"              Sys% = system prompt share of peak (first-turn cache_read).\n" +
			"              High Sys% → CLAUDE.md or hook output is large relative to work done."},
		{"Conv%", "Context added beyond the first turn: conversation history + tool results.\n" +
			"              High is normal in long sessions. Close to 0% = very short session."},
		{"Cache Hit", "cache_read / (cache_read + cache_create). High is good — it means\n" +
			"              the system prompt stays stable across turns and you're paying the\n" +
			"              cheaper $0.30/MTok cache-read rate instead of $3.00/MTok fresh input."},
		{"Est. Cost", "Calculated from token counts × Anthropic list prices. Not billed cost\n" +
			"              (discounts, free tier, or API credits are not reflected)."},
	}
	fmt.Fprintln(os.Stderr, dimStyle.Render("How to read this output:"))
	for _, l := range legend {
		fmt.Fprintf(os.Stderr, "%s\n",
			dimStyle.Render(fmt.Sprintf("  %-12s  %s", l.key, l.desc)))
	}
	fmt.Fprintln(os.Stderr)

	// ── Project Overview ──────────────────────────────────────────────────

	// Aggregate global and per-model totals.
	type globalStats struct {
		Sessions    int
		TotalInput  int
		TotalOutput int
		CacheCreate int
		CacheRead   int
	}
	global := globalStats{}
	modelGlobal := map[string]*modelUsage{} // model id → totals across all sessions

	for i := range sessions {
		s := &sessions[i]
		global.Sessions++
		global.TotalInput += s.TotalInput
		global.TotalOutput += s.TotalOutput
		global.CacheCreate += s.TotalCacheCreate
		global.CacheRead += s.TotalCacheRead
		for model, mu := range s.ModelUsage {
			g := modelGlobal[model]
			if g == nil {
				g = &modelUsage{}
				modelGlobal[model] = g
			}
			g.Turns += mu.Turns
			g.Input += mu.Input
			g.Output += mu.Output
			g.CacheCreate += mu.CacheCreate
			g.CacheRead += mu.CacheRead
		}
	}

	totalCost := 0.0
	for model, mu := range modelGlobal {
		totalCost += mu.cost(model)
	}
	totalTurns := 0
	for _, mu := range modelGlobal {
		totalTurns += mu.Turns
	}

	// Sort models by turn count descending.
	type modelEntry struct {
		Model string
		*modelUsage
	}
	var modelList []modelEntry
	for k, v := range modelGlobal {
		modelList = append(modelList, modelEntry{k, v})
	}
	sort.Slice(modelList, func(i, j int) bool {
		return modelList[i].Turns > modelList[j].Turns
	})

	fmt.Fprintln(os.Stderr, boldStyle.Render("Project Overview"))
	sep()
	fmt.Fprintf(os.Stderr, "  %s  %s in · %s out   Est. cost: %s\n\n",
		boldStyle.Render(fmt.Sprintf("%d sessions", global.Sessions)),
		formatTokens(global.TotalInput+global.CacheRead+global.CacheCreate),
		formatTokens(global.TotalOutput),
		styleTitle.Render(fmt.Sprintf("$%.2f", totalCost)))

	fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Render("Model distribution (by turns)"))
	dot()
	fmt.Fprintf(os.Stderr, "  %-30s %7s  %6s  %10s\n", "Model", "Turns", "Share", "Est. Cost")
	dot()
	for _, me := range modelList {
		pct := 0.0
		if totalTurns > 0 {
			pct = float64(me.Turns) / float64(totalTurns) * 100
		}
		cost := me.cost(me.Model)
		fmt.Fprintf(os.Stderr, "  %-30s %7d  %5.1f%%  %10s\n",
			truncate(me.Model, 28),
			me.Turns,
			pct,
			fmt.Sprintf("$%.2f", cost))
	}

	// ── Top Sessions by Peak Context ──────────────────────────────────────

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, boldStyle.Render("Top Sessions by Peak Context"))
	sep()
	fmt.Fprintf(os.Stderr, "  %-10s %-20s %-16s %-17s %-6s %-5s %-7s %s\n",
		"Session", "Profile", "Project", "Peak Context", "Cache", "Sys%", "Conv%", "Turns")
	dot()

	limit := 10
	if len(sessions) < limit {
		limit = len(sessions)
	}
	for _, s := range sessions[:limit] {
		pct := s.peakPercent()
		peakStr := fmt.Sprintf("%dk/%dk (%d%%)", s.PeakContext/1000, contextWindowLimit/1000, int(pct))
		var peakColored string
		switch {
		case pct >= 80:
			peakColored = styleWarn.Render(peakStr)
		case pct >= 50:
			peakColored = amberStyle.Render(peakStr)
		default:
			peakColored = peakStr
		}

		sysPctVal := s.sysPct()
		sysPctStr := fmt.Sprintf("%d%%", sysPctVal)
		var sysPctColored string
		switch {
		case sysPctVal >= 30:
			sysPctColored = styleWarn.Render(sysPctStr)
		case sysPctVal >= 15:
			sysPctColored = amberStyle.Render(sysPctStr)
		default:
			sysPctColored = dimStyle.Render(sysPctStr)
		}

		fmt.Fprintf(os.Stderr, "  %s  %-20s %-16s %s%-6s %s  %-7s %d\n",
			shortID(s.SessionID),
			truncate(s.Profile, 18),
			truncate(s.Project, 14),
			ansiPad(peakColored, 17),
			fmt.Sprintf("%d%%", int(s.cacheHitRatio()*100)),
			ansiPad(sysPctColored, 5),
			fmt.Sprintf("%d%%", s.growthPct()),
			s.TurnCount)
	}

	// ── Per-Profile Cache Efficiency ──────────────────────────────────────

	type profileStats struct {
		Sessions         int
		TotalCacheRead   int
		TotalCacheCreate int
		PeakSum          int
		MaxPeak          int
		TotalCost        float64
	}
	profileMap := map[string]*profileStats{}
	for i := range sessions {
		s := &sessions[i]
		ps := profileMap[s.Profile]
		if ps == nil {
			ps = &profileStats{}
			profileMap[s.Profile] = ps
		}
		ps.Sessions++
		ps.TotalCacheRead += s.TotalCacheRead
		ps.TotalCacheCreate += s.TotalCacheCreate
		ps.PeakSum += s.PeakContext
		if s.PeakContext > ps.MaxPeak {
			ps.MaxPeak = s.PeakContext
		}
		ps.TotalCost += s.totalCost()
	}
	type profileEntry struct {
		Profile string
		*profileStats
	}
	var profileList []profileEntry
	for k, v := range profileMap {
		profileList = append(profileList, profileEntry{k, v})
	}
	sort.Slice(profileList, func(i, j int) bool {
		return profileList[i].Sessions > profileList[j].Sessions
	})

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, boldStyle.Render("Per-Profile Cache Efficiency"))
	sep()
	fmt.Fprintf(os.Stderr, "  %-24s %8s %10s %10s %-10s %10s\n",
		"Profile", "Sessions", "Avg Peak", "Max Peak", "Cache Hit", "Est. Cost")
	dot()

	for _, p := range profileList {
		avgPeak := 0
		if p.Sessions > 0 {
			avgPeak = p.PeakSum / p.Sessions
		}
		total := p.TotalCacheRead + p.TotalCacheCreate
		hitRatio := 0.0
		if total > 0 {
			hitRatio = float64(p.TotalCacheRead) / float64(total) * 100
		}
		hitStr := fmt.Sprintf("%d%%", int(hitRatio))
		var hitColored string
		switch {
		case hitRatio < 40:
			hitColored = styleWarn.Render(hitStr)
		case hitRatio < 65:
			hitColored = amberStyle.Render(hitStr)
		default:
			hitColored = styleSuccess.Render(hitStr)
		}
		fmt.Fprintf(os.Stderr, "  %-24s %8d %10s %10s %s %10s\n",
			truncate(p.Profile, 22),
			p.Sessions,
			fmt.Sprintf("%dk", avgPeak/1000),
			fmt.Sprintf("%dk", p.MaxPeak/1000),
			ansiPad(hitColored, 10),
			fmt.Sprintf("$%.2f", p.TotalCost))
	}

	// ── Recommendations ───────────────────────────────────────────────────

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, boldStyle.Render("Recommendations"))
	sep()

	hasRecs := false

	for _, p := range profileList {
		total := p.TotalCacheRead + p.TotalCacheCreate
		if total == 0 || p.Sessions < 2 {
			continue
		}
		hitRatio := float64(p.TotalCacheRead) / float64(total) * 100
		if hitRatio < 40 {
			warn("  Profile %q has %.0f%% cache hit ratio — CLAUDE.md or system prompt may be changing between sessions", p.Profile, hitRatio)
			hasRecs = true
		}
	}

	heavyCount := 0
	for _, s := range sessions {
		if s.peakPercent() >= 80 {
			heavyCount++
		}
	}
	if heavyCount > 0 {
		warn("  %d session(s) exceeded 80%% of context limit (%dk tokens) — break large tasks or use /compact", heavyCount, contextWindowLimit/1000)
		hasRecs = true
	}

	noProfileHeavy := 0
	for _, s := range sessions {
		if s.Profile == "(no profile)" && s.peakPercent() >= 50 {
			noProfileHeavy++
		}
	}
	if noProfileHeavy > 0 {
		info("  %d session(s) with no profile used >50%% context — a profile with a targeted CLAUDE.md would improve cache hits", noProfileHeavy)
		hasRecs = true
	}

	// Flag sessions where system prompt dominates — only meaningful when the
	// session ran long enough that the system prompt isn't just the initial setup.
	bloatedSys := 0
	for _, s := range sessions {
		if s.sysPct() >= 30 && s.TurnCount >= 5 && s.PeakContext > 30_000 {
			bloatedSys++
		}
	}
	if bloatedSys > 0 {
		warn("  %d session(s) have system prompt >30%% of peak context — review CLAUDE.md / hook output size", bloatedSys)
		hasRecs = true
	}

	if !hasRecs {
		success("  All sessions look healthy.")
	}

	fmt.Fprintln(os.Stderr)
}

// formatTokens prints a token count in human-readable form (k / M).
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ansiPad right-pads s to width visual columns, accounting for ANSI escape
// codes that fmt's %-Ns verb counts as printable but the terminal does not.
func ansiPad(s string, width int) string {
	vis := lipgloss.Width(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

func countProjects(sessions []sessionMetrics) int {
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s.Project] = true
	}
	return len(seen)
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8] + "…"
	}
	return id
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
