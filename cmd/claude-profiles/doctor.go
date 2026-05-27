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

// ── Version helpers ──────────────────────────────────────────────────────────
//
// Version thresholds. Single source of truth for the "minimum Claude Code
// release that ships a fix claude-profiles depends on" constants — used by
// checkClaudeBinary, checkVersionConstraints, and the dispatcher gating in
// delegate_bg.go. When a future regression lands, add one entry to
// knownBadVersions and (if it introduces a new feature dependency) one
// const here; doc tables in README.md / CLAUDE.md reference these names
// rather than re-stating literals so code and docs can't drift.
const (
	// minDelegateBg is the first release that doesn't silently time out bg
	// delegate sessions on permission re-prompting (#27).
	minDelegateBgMaj, minDelegateBgMin, minDelegateBgPat = 2, 1, 146
	// minWorktreeSandbox narrows the sandbox write allowlist inside a worktree
	// session to just the shared .git object store (#41). Below this, _worktree
	// profiles have weaker OS-level isolation than expected.
	minWorktreeSandboxMaj, minWorktreeSandboxMin, minWorktreeSandboxPat = 2, 1, 149
	// minSubagentStability is the first release without the background-worker
	// crash on subagent cancellation (#44); also the first release where
	// reloadSkills, sessionTitle, and the PushNotification tool exist
	// (#42, #43, #45) — the dispatcher gates PushNotification injection on it.
	minSubagentStabilityMaj, minSubagentStabilityMin, minSubagentStabilityPat = 2, 1, 152
)

// parseClaudeVersion extracts the semver triple from the output of
// `claude --version`. The string can be "claude X.Y.Z", "X.Y.Z", or any
// sentence that contains a dotted triple — we take the first token that
// matches. Returns ok=false if no triple is found.
func parseClaudeVersion(s string) (major, minor, patch int, ok bool) {
	for _, tok := range strings.Fields(s) {
		n, _ := fmt.Sscanf(tok, "%d.%d.%d", &major, &minor, &patch)
		if n == 3 {
			return major, minor, patch, true
		}
	}
	return 0, 0, 0, false
}

// versionAtLeast returns true when the parsed (maj, min, pat) triple is ≥ the
// (needMaj, needMin, needPat) triple. Mirrors the ordering used by semver.
func versionAtLeast(maj, min, pat, needMaj, needMin, needPat int) bool {
	if maj != needMaj {
		return maj > needMaj
	}
	if min != needMin {
		return min > needMin
	}
	return pat >= needPat
}

// readClaudeVersion runs `claude --version` once and parses its output.
// Returns (raw string, parsed triple, ok). Used to share one fork between
// checkClaudeBinary and checkVersionConstraints — the doctor used to shell
// out twice for the same data, paying ~30–50ms per duplicate invocation.
func readClaudeVersion() (raw string, maj, min, pat int, ok bool) {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return "", 0, 0, 0, false
	}
	raw = strings.TrimSpace(string(out))
	if raw == "" {
		return "", 0, 0, 0, false
	}
	maj, min, pat, ok = parseClaudeVersion(raw)
	return raw, maj, min, pat, ok
}

// badVersionRange describes an inclusive [from, to] range of Claude Code
// releases known to have a regression that directly affects claude-profiles.
type badVersionRange struct {
	fromMaj, fromMin, fromPat int
	toMaj, toMin, toPat       int
	description               string
}

// knownBadVersions is the table of Claude Code releases with regressions that
// break claude-profiles features. Checked in checkClaudeBinary(); extend here
// when a new regression is confirmed and fixed in a subsequent release.
var knownBadVersions = []badVersionRange{
	// v2.1.147: Bash tool exits with code 127 on every command for some users.
	// Fixed in v2.1.148. Affects Stop-hook distill, delegate sessions, and
	// the PromptSubmit hook which all rely on Bash tool calls.
	{2, 1, 147, 2, 1, 147,
		"Bash tool exits code 127 on every command — upgrade to v2.1.148+"},
}

// cmdDoctor prints a one-shot health report — meant to be the first thing the
// user runs when /handoff, the hook, or a launch starts misbehaving. Each row
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

	// One `claude --version` fork shared across the binary check and the
	// profile-feature checks. If the binary isn't installed at all,
	// readClaudeVersion returns ok=false and checkClaudeBinary handles the
	// not-found case via exec.LookPath separately.
	rawVersion, vMaj, vMin, vPat, vOK := readClaudeVersion()

	checks := []docCheck{}
	checks = append(checks, checkClaudeBinary(rawVersion, vMaj, vMin, vPat, vOK))
	checks = append(checks, checkVersionConstraints(vMaj, vMin, vPat, vOK, rawVersion)...)
	checks = append(checks, checkClaudeProfilesPath())
	checks = append(checks, checkTmux())
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

func checkClaudeBinary(version string, maj, min, pat int, ok bool) docCheck {
	path, err := exec.LookPath("claude")
	if err != nil {
		return docCheck{"claude binary", "fail", "not found in PATH"}
	}
	if version == "" {
		return docCheck{"claude binary", "warn", path + " (version unknown)"}
	}
	if !ok {
		return docCheck{"claude binary", "warn", path + " (version unparseable: " + version + ")"}
	}

	// Known-bad releases take priority — emit fail so it's unmissable.
	for _, bad := range knownBadVersions {
		inRange := versionAtLeast(maj, min, pat, bad.fromMaj, bad.fromMin, bad.fromPat) &&
			!versionAtLeast(maj, min, pat, bad.toMaj, bad.toMin, bad.toPat+1)
		if inRange {
			return docCheck{"claude binary", "fail",
				fmt.Sprintf("%s (%s) — known-bad: %s", path, version, bad.description)}
		}
	}

	// Minimum reliable baseline for delegate bg sessions (#27): v2.1.146 fixes
	// permission re-prompting that caused delegates to block indefinitely in bg
	// sessions, eventually timing out with a vague dispatch-error.md.
	if !versionAtLeast(maj, min, pat, minDelegateBgMaj, minDelegateBgMin, minDelegateBgPat) {
		return docCheck{"claude binary", "warn",
			fmt.Sprintf("%s (%s) — below v%d.%d.%d: delegate bg sessions may silently "+
				"timeout due to permission re-prompting; upgrade recommended",
				path, version, minDelegateBgMaj, minDelegateBgMin, minDelegateBgPat)}
	}

	return docCheck{"claude binary", "ok", path + " (" + version + ")"}
}

// checkVersionConstraints runs version-sensitive profile checks that require
// both a parsed Claude Code version and knowledge of which profile features are
// active. Returns zero or more docChecks — all "ok" entries are omitted to
// keep doctor output focused on actionable items.
//
// Scope is intentionally global: we iterate every profile across every repo,
// not just the cwd repo, because the user could launch any of them from
// anywhere. Surfacing the warn here once is cheaper than waiting for the user
// to hit the regression after a launch from an unrelated directory.
func checkVersionConstraints(maj, min, pat int, ok bool, version string) []docCheck {
	if !ok {
		return nil // checkClaudeBinary already surfaced the binary problem
	}

	locs, _ := listAllLocations()
	hasWorktree, hasSubagentModel := false, false
	for _, loc := range locs {
		p, _ := loadProfileAt(loc.JSONPath)
		if p == nil {
			continue
		}
		if p.Worktree {
			hasWorktree = true
		}
		if p.SubagentModel != "" {
			hasSubagentModel = true
		}
		// Short-circuit: once both flags are set the rest of the inventory
		// can't change the outcome. Saves an O(N) loadProfileAt sweep on
		// every `doctor` invocation for users with many profiles.
		if hasWorktree && hasSubagentModel {
			break
		}
	}

	var checks []docCheck

	// Worktree sandbox isolation (#41): v2.1.149 fixed the write allowlist that
	// incorrectly covered the entire main repo root instead of just the shared
	// .git directory. Users on older versions running _worktree profiles have
	// weaker OS-level isolation than they expect.
	if hasWorktree && !versionAtLeast(maj, min, pat, minWorktreeSandboxMaj, minWorktreeSandboxMin, minWorktreeSandboxPat) {
		checks = append(checks, docCheck{
			"worktree isolation", "warn",
			fmt.Sprintf("running %s — below v%d.%d.%d: sandbox write allowlist covers entire "+
				"main repo root; _worktree profiles lack full OS-level isolation; upgrade to v%d.%d.%d+",
				version,
				minWorktreeSandboxMaj, minWorktreeSandboxMin, minWorktreeSandboxPat,
				minWorktreeSandboxMaj, minWorktreeSandboxMin, minWorktreeSandboxPat),
		})
	}

	// Subagent cancellation stability (#44): v2.1.152 fixed a background worker
	// crash when a subagent is cancelled mid-flight and its stale permission
	// prompt is processed. Affects profiles with _subagent_model set.
	if hasSubagentModel && !versionAtLeast(maj, min, pat, minSubagentStabilityMaj, minSubagentStabilityMin, minSubagentStabilityPat) {
		checks = append(checks, docCheck{
			"subagent stability", "warn",
			fmt.Sprintf("running %s — below v%d.%d.%d: delegates using _subagent_model exposed to "+
				"background worker crash on subagent cancellation; upgrade to v%d.%d.%d+",
				version,
				minSubagentStabilityMaj, minSubagentStabilityMin, minSubagentStabilityPat,
				minSubagentStabilityMaj, minSubagentStabilityMin, minSubagentStabilityPat),
		})
	}

	return checks
}

// checkClaudeProfilesPath confirms `claude-profiles` resolves on PATH — the
// SessionStart hook embeds that bare name, so a missing entry breaks the
// free-form /handoff flow.
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

func checkTmux() docCheck {
	if os.Getenv("CLAUDE_PROFILES_NO_TMUX") != "" {
		return docCheck{"tmux", "ok", "disabled via --no-tmux / CLAUDE_PROFILES_NO_TMUX (/delegate unavailable)"}
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		return docCheck{"tmux", "warn", "not found in PATH — /delegate unavailable; run claude-profiles to be offered an install"}
	}
	if os.Getenv("TMUX") != "" {
		return docCheck{"tmux", "ok", path + " (inside tmux session)"}
	}
	return docCheck{"tmux", "ok", path}
}

func checkSwitchCommand() docCheck {
	path := filepath.Join(wrapperPluginPath(), "commands", "handoff.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return docCheck{"/handoff slash command", "warn", "not installed yet — first run of any launch will install it"}
	}
	if string(data) != handoffSlashCommand {
		return docCheck{"/handoff slash command", "warn", "out of date; will be rewritten on next launch"}
	}
	return docCheck{"/handoff slash command", "ok", path}
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
			return docCheck{name, "fail", "settings.json is not valid JSON: " + err.Error()}
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
