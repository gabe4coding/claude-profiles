package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// cmdDoctor prints a one-shot health report — meant to be the first thing the
// user runs when /switch, the hook, or a launch starts misbehaving. Each row
// is one independent check; an overall exit code of 1 means at least one FAIL.

type docCheck struct {
	name   string
	status string // "ok" | "warn" | "fail"
	detail string
}

var (
	docOK   = lipgloss.NewStyle().Foreground(cdsSage).Bold(true).Render("✓")
	docWarn = lipgloss.NewStyle().Foreground(cdsAmber).Bold(true).Render("!")
	docFail = lipgloss.NewStyle().Foreground(cdsRust).Bold(true).Render("✗")
)

func cmdDoctor() {
	title("claude-profiles doctor")
	fmt.Fprintln(os.Stderr)

	checks := []docCheck{}
	checks = append(checks, checkClaudeBinary())
	checks = append(checks, checkClaudeProfilesPath())
	checks = append(checks, checkSwitchCommand())
	checks = append(checks, checkProfilesDir())
	checks = append(checks, checkAllProfiles()...)
	checks = append(checks, checkStaleMarker())
	checks = append(checks, checkRunningWrappers())
	checks = append(checks, checkBackgroundedSessions())
	checks = append(checks, checkSessionsDir())

	failed, warned := 0, 0
	for _, c := range checks {
		renderDocCheck(c)
		switch c.status {
		case "fail":
			failed++
		case "warn":
			warned++
		}
	}

	fmt.Fprintln(os.Stderr)
	switch {
	case failed > 0:
		warn("%d check(s) failed, %d warning(s).", failed, warned)
		os.Exit(1)
	case warned > 0:
		warn("All checks passed with %d warning(s).", warned)
	default:
		success("All checks passed.")
	}
}

func renderDocCheck(c docCheck) {
	mark := docOK
	switch c.status {
	case "warn":
		mark = docWarn
	case "fail":
		mark = docFail
	}
	label := lipgloss.NewStyle().Foreground(cdsInk).Render(c.name)
	if c.detail != "" {
		label = label + "  " + styleInfo.Render(c.detail)
	}
	fmt.Fprintf(os.Stderr, "  %s  %s\n", mark, label)
}

// ── Individual checks ────────────────────────────────────────────────────────

func checkClaudeBinary() docCheck {
	path, err := exec.LookPath("claude")
	if err != nil {
		return docCheck{"claude binary", "fail", "not found in PATH"}
	}
	out, err := exec.Command("claude", "--version").Output()
	version := strings.TrimSpace(string(out))
	if err != nil || version == "" {
		return docCheck{"claude binary", "warn", path + " (version unknown)"}
	}
	return docCheck{"claude binary", "ok", path + " (" + version + ")"}
}

// checkClaudeProfilesPath confirms `claude-profiles` resolves on PATH — the
// SessionStart hook embeds that bare name, so a missing entry breaks the
// free-form /switch flow.
func checkClaudeProfilesPath() docCheck {
	path, err := exec.LookPath("claude-profiles")
	if err != nil {
		return docCheck{"claude-profiles on PATH", "fail", "hook command will not resolve at fire time"}
	}
	self, _ := os.Executable()
	if self != "" && resolveSymlink(path) != resolveSymlink(self) {
		return docCheck{
			"claude-profiles on PATH", "warn",
			fmt.Sprintf("PATH points at %s but doctor is running %s", path, self),
		}
	}
	return docCheck{"claude-profiles on PATH", "ok", path}
}

func resolveSymlink(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

func checkSwitchCommand() docCheck {
	path := filepath.Join(claudeRootDirPath(), "commands", "switch.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return docCheck{"/switch slash command", "warn", "not installed yet — first run of any launch will install it"}
	}
	if string(data) != switchSlashCommand {
		return docCheck{"/switch slash command", "warn", "out of date; will be rewritten on next launch"}
	}
	return docCheck{"/switch slash command", "ok", path}
}

func checkProfilesDir() docCheck {
	dir := profilesDir()
	info, err := os.Stat(dir)
	if err != nil {
		return docCheck{"profiles dir", "warn", dir + " — does not exist yet"}
	}
	if !info.IsDir() {
		return docCheck{"profiles dir", "fail", dir + " — exists but is not a directory"}
	}
	locs, _ := listAllLocations()
	return docCheck{"profiles dir", "ok", fmt.Sprintf("%s (%d profile%s)", dir, len(locs), plural(len(locs)))}
}

// checkAllProfiles validates every profile (local + repo): JSON parse, MCP
// servers present, sidecar settings.json (if any) parses, hook augmentation
// works.
func checkAllProfiles() []docCheck {
	locs, err := listAllLocations()
	if err != nil {
		return []docCheck{{"profile inventory", "fail", err.Error()}}
	}
	out := make([]docCheck, 0, len(locs))
	for _, loc := range locs {
		out = append(out, checkProfile(loc))
	}
	return out
}

func checkProfile(loc ProfileLocation) docCheck {
	name := "profile " + loc.QualifiedID
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		return docCheck{name, "fail", "cannot parse: " + err.Error()}
	}
	bits := []string{fmt.Sprintf("%d MCP server%s", len(p.McpServers), plural(len(p.McpServers)))}
	if len(p.Settings) > 0 {
		var probe any
		if err := json.Unmarshal(p.Settings, &probe); err != nil {
			return docCheck{name, "fail", "_settings is not valid JSON: " + err.Error()}
		}
		bits = append(bits, "settings")
	}
	return docCheck{name, "ok", strings.Join(bits, ", ")}
}

func checkStaleMarker() docCheck {
	p := nextMarkerPath()
	info, err := os.Stat(p)
	if err != nil {
		return docCheck{"profile-switch marker", "ok", "absent (expected)"}
	}
	return docCheck{
		"profile-switch marker", "warn",
		fmt.Sprintf("stale file present at %s (mtime %s) — next `claude-profiles run` will clean it",
			p, info.ModTime().Format("15:04:05")),
	}
}

func checkRunningWrappers() docCheck {
	live := loadRunningWrappers() // also self-cleans dead entries
	stale := countStalePidfiles() // anything left after the cleanup pass
	detail := fmt.Sprintf("%d live wrapper%s", len(live), plural(len(live)))
	if stale > 0 {
		return docCheck{"running wrappers", "warn", fmt.Sprintf("%s, %d stale pidfile%s remain in %s", detail, stale, plural(stale), runningDir())}
	}
	return docCheck{"running wrappers", "ok", detail}
}

func checkBackgroundedSessions() docCheck {
	// Prune ledger entries whose session id is no longer in the supervisor.
	if ids := rosterSessionIDs(); ids != nil {
		_ = pruneSessionProfiles(ids)
	}
	all := loadBackgroundedSessions()
	mapped := 0
	for _, s := range all {
		if s.Profile != "" {
			mapped++
		}
	}
	if len(all) == 0 {
		return docCheck{"backgrounded sessions", "ok", "none in supervisor roster"}
	}
	return docCheck{
		"backgrounded sessions", "ok",
		fmt.Sprintf("%d in supervisor (%d mapped to claude-profiles)", len(all), mapped),
	}
}

func checkSessionsDir() docCheck {
	dir := projectSessionsDir()
	if dir == "" {
		return docCheck{"project sessions dir", "warn", "could not determine cwd"}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return docCheck{"project sessions dir", "ok", dir + " — no sessions yet in this cwd"}
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			count++
		}
	}
	return docCheck{"project sessions dir", "ok", fmt.Sprintf("%s (%d session%s)", dir, count, plural(count))}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
