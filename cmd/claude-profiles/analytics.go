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

// sessionMetrics holds derived stats for a single session file.
type sessionMetrics struct {
	SessionID        string
	Cwd              string
	Profile          string // qualified id, or "(no profile)"
	Project          string // base name of cwd
	TurnCount        int    // main-chain assistant turns only
	TotalInput       int
	TotalOutput      int
	TotalCacheRead   int
	TotalCacheCreate int
	PeakContext      int // max(input+cache_read+cache_create) across turns
	ToolCallCount    int
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

type analyticsRawEvent struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Cwd         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	Message     struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message"`
}

func parseSessionFile(path string) *sessionMetrics {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)

	m := &sessionMetrics{}
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
		u := ev.Message.Usage
		contextAtTurn := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
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

	// Sort by peak context descending for the top-sessions table.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].PeakContext > sessions[j].PeakContext
	})

	sepStyle := lipgloss.NewStyle().Foreground(cdsMuted)
	boldStyle := lipgloss.NewStyle().Bold(true)
	amberStyle := lipgloss.NewStyle().Foreground(cdsAmber)

	sep := func() { fmt.Fprintln(os.Stderr, sepStyle.Render(strings.Repeat("─", 72))) }

	fmt.Fprintln(os.Stderr)
	title("=== Context Window Analytics ===")
	fmt.Fprintf(os.Stderr, "\n%s\n\n", styleInfo.Render(fmt.Sprintf("Scanned %d sessions across %d project(s)", len(sessions), countProjects(sessions))))

	// ── Top Heavy Sessions ────────────────────────────────────────────────

	fmt.Fprintln(os.Stderr, boldStyle.Render("Top Sessions by Peak Context"))
	sep()
	fmt.Fprintf(os.Stderr, "  %-10s %-22s %-22s %-18s %-7s %s\n",
		"Session", "Profile", "Project", "Peak Context", "Cache", "Turns")
	fmt.Fprintln(os.Stderr, sepStyle.Render("  "+strings.Repeat("·", 68)))

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
		cacheStr := fmt.Sprintf("%d%%", int(s.cacheHitRatio()*100))
		fmt.Fprintf(os.Stderr, "  %s  %-22s %-22s %s%-7s %d\n",
			shortID(s.SessionID),
			truncate(s.Profile, 20),
			truncate(s.Project, 20),
			ansiPad(peakColored, 18),
			cacheStr,
			s.TurnCount)
	}

	// ── Per-Profile Cache Efficiency ──────────────────────────────────────

	type profileStats struct {
		Sessions         int
		TotalCacheRead   int
		TotalCacheCreate int
		PeakSum          int
		MaxPeak          int
	}
	profileMap := map[string]*profileStats{}
	for _, s := range sessions {
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
	fmt.Fprintf(os.Stderr, "  %-26s %8s %10s %10s %-10s\n",
		"Profile", "Sessions", "Avg Peak", "Max Peak", "Cache Hit")
	fmt.Fprintln(os.Stderr, sepStyle.Render("  "+strings.Repeat("·", 68)))

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
		fmt.Fprintf(os.Stderr, "  %-26s %8d %10s %10s %s\n",
			truncate(p.Profile, 24),
			p.Sessions,
			fmt.Sprintf("%dk", avgPeak/1000),
			fmt.Sprintf("%dk", p.MaxPeak/1000),
			ansiPad(hitColored, 10))
	}

	// ── Per-Project Totals ────────────────────────────────────────────────

	type projectStats struct {
		Sessions      int
		TotalOutput   int
		PeakSum       int
		HeavySessions int
	}
	projectMap := map[string]*projectStats{}
	for _, s := range sessions {
		ps := projectMap[s.Project]
		if ps == nil {
			ps = &projectStats{}
			projectMap[s.Project] = ps
		}
		ps.Sessions++
		ps.TotalOutput += s.TotalOutput
		ps.PeakSum += s.PeakContext
		if s.peakPercent() >= 80 {
			ps.HeavySessions++
		}
	}
	type projectEntry struct {
		Project string
		*projectStats
	}
	var projectList []projectEntry
	for k, v := range projectMap {
		projectList = append(projectList, projectEntry{k, v})
	}
	sort.Slice(projectList, func(i, j int) bool {
		return projectList[i].Sessions > projectList[j].Sessions
	})

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, boldStyle.Render("Per-Project Totals"))
	sep()
	fmt.Fprintf(os.Stderr, "  %-26s %8s %13s %10s %-11s\n",
		"Project", "Sessions", "Total Output", "Avg Peak", "Heavy(>80%)")
	fmt.Fprintln(os.Stderr, sepStyle.Render("  "+strings.Repeat("·", 68)))

	for _, p := range projectList {
		avgPeak := 0
		if p.Sessions > 0 {
			avgPeak = p.PeakSum / p.Sessions
		}
		heavyStr := fmt.Sprintf("%d", p.HeavySessions)
		if p.HeavySessions > 0 {
			heavyStr = styleWarn.Render(heavyStr)
		}
		fmt.Fprintf(os.Stderr, "  %-26s %8d %13s %10s %s\n",
			truncate(p.Project, 24),
			p.Sessions,
			fmt.Sprintf("%dk", p.TotalOutput/1000),
			fmt.Sprintf("%dk", avgPeak/1000),
			ansiPad(heavyStr, 11))
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

	if !hasRecs {
		success("  All sessions look healthy.")
	}

	fmt.Fprintln(os.Stderr)
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
