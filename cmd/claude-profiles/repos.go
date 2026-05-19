package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const syncThrottle = 5 * time.Minute

// RepoConfig describes one registered remote repo.
type RepoConfig struct {
	URL          string `json:"url"`
	Alias        string `json:"alias"`            // short display name; unique
	Branch       string `json:"branch,omitempty"` // optional
	LastSync     int64  `json:"lastSync,omitempty"`
	LastSyncOK   bool   `json:"lastSyncOK,omitempty"`
	LastSyncErr  string `json:"lastSyncErr,omitempty"`
}

// ReposConfig is the on-disk shape of ~/.claude-profiles/repos.json.
type ReposConfig struct {
	Repos []RepoConfig `json:"repos"`
}

// MarketplacePlugin is one entry in .claude-plugin/marketplace.json.
type MarketplacePlugin struct {
	Name        string `json:"name"`
	Source      string `json:"source"`               // relative path from repo root
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// MarketplaceConfig is the on-disk shape of .claude-plugin/marketplace.json.
type MarketplaceConfig struct {
	Plugins []MarketplacePlugin `json:"plugins"`
}

// reposConfigPath / reposCacheDir are functions so CLAUDE_PROFILES_ROOT
// overrides take effect immediately. reposProfileDir is the per-repo
// directory we expect remote repos to publish profiles in.
func reposConfigPath() string { return reposConfigPathFn() }
func reposCacheDir() string   { return reposCacheDirPath() }

// ── Config I/O ────────────────────────────────────────────────────────────────

func loadReposConfig() (*ReposConfig, error) {
	data, err := os.ReadFile(reposConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &ReposConfig{}, nil
		}
		return nil, err
	}
	var cfg ReposConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveReposConfig(cfg *ReposConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(reposConfigPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(reposConfigPath(), append(data, '\n'), 0o644)
}

// ── URL normalisation + cache path ────────────────────────────────────────────

// normaliseURL collapses common alternative forms so the same repo doesn't get
// two cache entries (e.g. https://github.com/foo/bar.git vs git@github.com:foo/bar).
func normaliseURL(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	// git@github.com:foo/bar  →  github.com/foo/bar
	if strings.HasPrefix(u, "git@") {
		u = strings.TrimPrefix(u, "git@")
		u = strings.Replace(u, ":", "/", 1)
	}
	// https://github.com/foo/bar  →  github.com/foo/bar
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		u = strings.TrimPrefix(u, prefix)
	}
	return strings.ToLower(u)
}

func repoCachePath(url string) string {
	h := sha256.Sum256([]byte(normaliseURL(url)))
	return filepath.Join(reposCacheDir(), fmt.Sprintf("%x", h[:10]))
}

// defaultAlias derives a short alias from the URL ("github.com/foo/bar" → "bar").
func defaultAlias(url string) string {
	n := normaliseURL(url)
	parts := strings.Split(n, "/")
	return parts[len(parts)-1]
}

// findRepo returns the repo with the given alias or normalised URL match.
func findRepo(cfg *ReposConfig, aliasOrURL string) *RepoConfig {
	target := normaliseURL(aliasOrURL)
	for i := range cfg.Repos {
		r := &cfg.Repos[i]
		if r.Alias == aliasOrURL || normaliseURL(r.URL) == target {
			return r
		}
	}
	return nil
}

// ── Clone + sync ──────────────────────────────────────────────────────────────

func cloneRepo(r RepoConfig) error {
	dest := repoCachePath(r.URL)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("cache already exists: %s", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	args := []string{"clone", "--depth", "20"}
	if r.Branch != "" {
		args = append(args, "--branch", r.Branch)
	}
	args = append(args, r.URL, dest)

	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stderr // surface progress on stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// syncRepoForeground runs `git fetch && merge --ff-only` synchronously,
// streaming output. Returns nil on success.
func syncRepoForeground(r *RepoConfig) error {
	dest := repoCachePath(r.URL)
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		return fmt.Errorf("clone missing for %s — re-add the repo", r.Alias)
	}
	cmd := exec.Command("git", "-C", dest, "pull", "--ff-only", "--quiet")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		r.LastSync = time.Now().Unix()
		r.LastSyncOK = false
		r.LastSyncErr = err.Error()
		return err
	}
	r.LastSync = time.Now().Unix()
	r.LastSyncOK = true
	r.LastSyncErr = ""
	return nil
}

// syncRepoBackground launches `git pull` detached. Errors are written to the
// repo's .last-sync.log; the sync status is updated by the child via a small
// wrapper script (we just touch a marker file pre-/post-).
//
// This runs *before* syscall.Exec replaces the parent process. Output is
// redirected to /dev/null + log so it never bleeds into the claude session.
func syncRepoBackground(r RepoConfig) {
	dest := repoCachePath(r.URL)
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		return
	}
	logPath := filepath.Join(dest, ".last-sync.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return
	}
	cmd := exec.Command("git", "-C", dest, "pull", "--ff-only")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Setsid detaches from the controlling terminal so the child survives
	// when the parent calls syscall.Exec into `claude`.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return
	}
	// We don't Wait() — let the child run independently. Close our fd to it;
	// the child has its own descriptor.
	go func() {
		cmd.Wait()
		logFile.Close()
	}()
}

// kickAutoSync fires a background sync for every registered repo whose
// LastSync is older than the throttle window. Called once at the start
// of every claude-profiles invocation.
//
// We bump LastSync in the config *before* firing the detached process so the
// throttle behaves correctly even though the parent will be syscall.Exec'd
// away before the child completes. The sync's success/failure is recorded in
// the per-repo .last-sync.log; future invocations see it via repo list.
func kickAutoSync() {
	cfg, err := loadReposConfig()
	if err != nil || len(cfg.Repos) == 0 {
		return
	}
	now := time.Now().Unix()
	cutoff := int64(syncThrottle.Seconds())
	dirty := false
	for i := range cfg.Repos {
		if now-cfg.Repos[i].LastSync < cutoff {
			continue
		}
		cfg.Repos[i].LastSync = now
		syncRepoBackground(cfg.Repos[i])
		dirty = true
	}
	if dirty {
		_ = saveReposConfig(cfg)
	}
}

// ── Repo profiles: discovery ──────────────────────────────────────────────────

// ProfileLocation is a resolved source for one profile.
type ProfileLocation struct {
	Name        string // bare profile name (no alias prefix)
	QualifiedID string // "name" for local, "alias/name" for repo
	JSONPath    string // absolute path to profile.json
	RepoAlias   string // empty for local; "." for project (CWD)
	// OwnerRepo is the absolute path of the repo that "owns" the profile —
	// i.e. the directory containing the .claude-profiles/ subdir the profile
	// lives in. Empty for user-level profiles (~/.claude-profiles/profiles/)
	// and for profiles from registered remote repos (alias/name) — those are
	// usable on any --dir. Non-empty for project-local profiles discovered in
	// or above CWD or via the known-projects cache; for those, delegate-runner
	// enforces that req.Dir is under OwnerRepo when the profile has _worktree.
	OwnerRepo string
	// Builtin, when non-empty, marks this location as one of the synthetic
	// built-in profiles. JSONPath points at the "<builtin:kind>/profile.json"
	// sentinel — there's no real file on disk. Callers that build CLI flags
	// must skip --strict-mcp-config / --mcp-config for built-ins so claude
	// uses its native MCP discovery (.mcp.json for project scope, ~/.claude.json
	// for user scope).
	Builtin string
}

// findCwdProfilesDir walks up from the current working directory looking for a
// .claude-profiles folder (the convention repos use to publish profiles). Returns
// the absolute path of that folder, or "" if not found.
func findCwdProfilesDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, reposProfileDir)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// listCwdProfileLocations returns profiles found in .claude-profiles/ rooted at
// (or above) the current working directory. These are "project-local" profiles
// and use RepoAlias="." so callers can distinguish them from global local profiles.
func listCwdProfileLocations() []ProfileLocation {
	root := findCwdProfilesDir()
	if root == "" {
		return nil
	}
	owner := mainRepoRoot(filepath.Dir(root)) // parent of .claude-profiles/, canonicalised across worktrees
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []ProfileLocation
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := filepath.Join(root, name)
		if !isProfileDir(dir) {
			continue
		}
		out = append(out, ProfileLocation{
			Name:        name,
			QualifiedID: name,
			JSONPath:    filepath.Join(dir, "profile.json"),
			RepoAlias:   ".",
			OwnerRepo:   owner,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// loadMarketplace reads .claude-plugin/marketplace.json from root.
// Returns nil on any error (missing file, bad JSON, etc.).
func loadMarketplace(root string) *MarketplaceConfig {
	data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil
	}
	var mc MarketplaceConfig
	if json.Unmarshal(data, &mc) != nil {
		return nil
	}
	return &mc
}

// marketplaceLocations returns ProfileLocations for every valid plugin listed in
// the marketplace.json at repoRoot. alias is "." for CWD, or the repo alias.
func marketplaceLocations(repoRoot, alias string) []ProfileLocation {
	mc := loadMarketplace(repoRoot)
	if mc == nil {
		return nil
	}
	var out []ProfileLocation
	for _, plugin := range mc.Plugins {
		dir := filepath.Join(repoRoot, filepath.FromSlash(plugin.Source))
		if !isProfileDir(dir) {
			continue
		}
		qid := plugin.Name
		// Owner is set ONLY for project-local marketplaces (alias=="."). For
		// aliased remote repos, the profile is global ("works anywhere").
		owner := ""
		if alias == "." {
			owner = mainRepoRoot(repoRoot)
		}
		if alias != "." && alias != "" {
			qid = alias + "/" + plugin.Name
		}
		out = append(out, ProfileLocation{
			Name:        plugin.Name,
			QualifiedID: qid,
			JSONPath:    filepath.Join(dir, "profile.json"),
			RepoAlias:   alias,
			OwnerRepo:   owner,
		})
	}
	return out
}

// findCwdMarketplaceRoot walks up from the current working directory looking
// for a .claude-plugin/marketplace.json file. Returns the directory that
// contains .claude-plugin/, or "" if none is found.
func findCwdMarketplaceRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// listCwdMarketplaceProfiles returns profiles from a marketplace.json found
// at or above the current working directory.
func listCwdMarketplaceProfiles() []ProfileLocation {
	root := findCwdMarketplaceRoot()
	if root == "" {
		return nil
	}
	locs := marketplaceLocations(root, ".")
	sort.Slice(locs, func(i, j int) bool { return locs[i].Name < locs[j].Name })
	return locs
}

// listRepoProfiles returns all profiles found in registered repos. Each repo
// is expected to publish profiles under <repo-root>/.claude-profiles/<name>/profile.json.
// For backward compatibility we also recognise <repo-root>/claude-profiles/…
// if the new hidden directory is absent.
func listRepoProfiles() []ProfileLocation {
	cfg, err := loadReposConfig()
	if err != nil || len(cfg.Repos) == 0 {
		return nil
	}
	var out []ProfileLocation
	seen := make(map[string]bool)
	for _, r := range cfg.Repos {
		cacheRoot := repoCachePath(r.URL)

		// Explicit .claude-profiles/ entries take priority over marketplace.
		root := repoProfilesRoot(cacheRoot)
		if root != "" {
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				dir := filepath.Join(root, name)
				if !isProfileDir(dir) {
					continue
				}
				loc := ProfileLocation{
					Name:        name,
					QualifiedID: r.Alias + "/" + name,
					JSONPath:    filepath.Join(dir, "profile.json"),
					RepoAlias:   r.Alias,
				}
				seen[loc.QualifiedID] = true
				out = append(out, loc)
			}
		}

		// Marketplace plugins are additive; .claude-profiles/ wins on conflict.
		for _, loc := range marketplaceLocations(cacheRoot, r.Alias) {
			if !seen[loc.QualifiedID] {
				seen[loc.QualifiedID] = true
				out = append(out, loc)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QualifiedID < out[j].QualifiedID })
	return out
}

// repoProfilesRoot returns the first directory inside the repo that contains
// our published profiles. Prefers `.claude-profiles/` (new convention); falls
// back to `claude-profiles/` so users syncing older repos still see them.
func repoProfilesRoot(repoRoot string) string {
	for _, sub := range []string{reposProfileDir, "claude-profiles"} {
		p := filepath.Join(repoRoot, sub)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p
		}
	}
	return ""
}

// resolveProfileLocation finds a profile by name, accepting:
//   - bare name       → user-level local, then project-local
//   - "./name"        → project-local (.claude-profiles/ in CWD) explicitly
//   - "alias/name"    → registered repo profile
func resolveProfileLocation(id string) (*ProfileLocation, error) {
	// Built-in profiles (":default", ":project") — synthetic, no disk lookup.
	if loc := resolveBuiltinLocation(id); loc != nil {
		return loc, nil
	}
	// "./name" explicitly targets the project-local profile in the CWD.
	if strings.HasPrefix(id, "./") {
		name := id[2:]
		for _, loc := range listCwdProfileLocations() {
			if loc.Name == name {
				loc.QualifiedID = id
				return &loc, nil
			}
		}
		for _, loc := range listCwdMarketplaceProfiles() {
			if loc.Name == name {
				loc.QualifiedID = id
				return &loc, nil
			}
		}
		return nil, fmt.Errorf("project profile not found: %s", id)
	}
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		alias, name := parts[0], parts[1]
		for _, loc := range listRepoProfiles() {
			if loc.RepoAlias == alias && loc.Name == name {
				return &loc, nil
			}
		}
		return nil, fmt.Errorf("repo profile not found: %s", id)
	}
	// Local: flat-file in profilesDir
	if profileExists(id) {
		return &ProfileLocation{
			Name:        id,
			QualifiedID: id,
			JSONPath:    profilePath(id),
		}, nil
	}
	// Project-local: .claude-profiles/ in or above CWD (local takes precedence)
	for _, loc := range listCwdProfileLocations() {
		if loc.Name == id {
			return &loc, nil
		}
	}
	// CWD marketplace plugins (fallback after .claude-profiles/)
	for _, loc := range listCwdMarketplaceProfiles() {
		if loc.Name == id {
			return &loc, nil
		}
	}
	// Known project profiles from other repos (discovered via prefs store).
	for _, loc := range listKnownProjectLocations() {
		if loc.Name == id {
			return &loc, nil
		}
	}
	return nil, fmt.Errorf("profile not found: %s", id)
}

// listKnownProjectLocations returns profiles from every .claude-profiles/
// directory recorded in the prefs store that is NOT inside the user's own
// ~/.claude-profiles/ tree. This makes project profiles from other repos
// discoverable when a session is launched from a different working directory.
func listKnownProjectLocations() []ProfileLocation {
	store := loadPrefsStore()
	ownRoot := profilesRoot()
	roots := map[string]bool{}
	for dir := range store {
		parent := filepath.Dir(dir)
		if filepath.Base(parent) != reposProfileDir {
			continue
		}
		if strings.HasPrefix(parent+string(filepath.Separator), ownRoot+string(filepath.Separator)) {
			continue
		}
		roots[parent] = true
	}
	sortedRoots := make([]string, 0, len(roots))
	for r := range roots {
		sortedRoots = append(sortedRoots, r)
	}
	sort.Strings(sortedRoots)

	var out []ProfileLocation
	for _, root := range sortedRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			dir := filepath.Join(root, name)
			if !isProfileDir(dir) {
				continue
			}
			out = append(out, ProfileLocation{
				Name:        name,
				QualifiedID: name,
				JSONPath:    filepath.Join(dir, "profile.json"),
				RepoAlias:   ".",
				OwnerRepo:   mainRepoRoot(filepath.Dir(root)), // parent of .claude-profiles/, canonicalised
			})
		}
	}
	return out
}

// isCwdUnder reports whether cwd equals or is nested under pinCwd.
// Uses filepath.Rel so symlinks / ".." are handled correctly.
func isCwdUnder(cwd, pinCwd string) bool {
	if pinCwd == "" {
		return true
	}
	rel, err := filepath.Rel(pinCwd, cwd)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// listAllLocations returns local + project + repo profiles, sorted with local first.
// Local profiles that have a _cwd pin are omitted unless the current working
// directory is equal to or nested under that pin.
func listAllLocations() ([]ProfileLocation, error) {
	// Built-ins go first so they're easy to find in the hub and in the
	// SessionStart hook's "Available profiles" listing.
	out := builtinLocations()
	localNames, err := listProfiles()
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	localSet := make(map[string]bool, len(localNames))
	for _, n := range localNames {
		loc := ProfileLocation{
			Name:        n,
			QualifiedID: n,
			JSONPath:    profilePath(n),
		}
		if p, err := loadProfileAt(loc.JSONPath); err == nil && p.Cwd != "" {
			if !isCwdUnder(cwd, p.Cwd) {
				continue
			}
		}
		localSet[n] = true
		out = append(out, loc)
	}
	// Project-local profiles from .claude-profiles/ in/above CWD. When a
	// user-level local profile shares the same name, both are shown: the
	// project copy gets QualifiedID="./name" so they resolve independently.
	cwdQIDs := make(map[string]bool)
	for _, loc := range listCwdProfileLocations() {
		if localSet[loc.Name] {
			loc.QualifiedID = "./" + loc.Name
		}
		cwdQIDs[loc.QualifiedID] = true
		out = append(out, loc)
	}
	// CWD marketplace plugins are additive; .claude-profiles/ wins on conflict.
	for _, loc := range listCwdMarketplaceProfiles() {
		if localSet[loc.Name] {
			loc.QualifiedID = "./" + loc.Name
		}
		if !cwdQIDs[loc.QualifiedID] {
			cwdQIDs[loc.QualifiedID] = true
			out = append(out, loc)
		}
	}
	out = append(out, listRepoProfiles()...)
	return out, nil
}

// ── Helpers for displaying repo info ──────────────────────────────────────────

func (r *RepoConfig) syncStatus() string {
	if r.LastSync == 0 {
		return "never synced"
	}
	ago := time.Since(time.Unix(r.LastSync, 0)).Round(time.Second)
	if r.LastSyncOK {
		return fmt.Sprintf("synced %s ago", ago)
	}
	return fmt.Sprintf("FAILED %s ago: %s", ago, r.LastSyncErr)
}

// copyFile is used by `claude-profiles copy` to bring a repo profile local.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
