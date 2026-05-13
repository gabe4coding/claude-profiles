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

const (
	reposConfigBaseName = "mcp-profiles-repos.json"
	reposCacheBaseName  = "mcp-profiles-repos"
	syncThrottle        = 5 * time.Minute
	reposProfileDir     = "claude-profiles" // folder inside each repo
)

// RepoConfig describes one registered remote repo.
type RepoConfig struct {
	URL          string `json:"url"`
	Alias        string `json:"alias"`            // short display name; unique
	Branch       string `json:"branch,omitempty"` // optional
	LastSync     int64  `json:"lastSync,omitempty"`
	LastSyncOK   bool   `json:"lastSyncOK,omitempty"`
	LastSyncErr  string `json:"lastSyncErr,omitempty"`
}

// ReposConfig is the on-disk shape of mcp-profiles-repos.json.
type ReposConfig struct {
	Repos []RepoConfig `json:"repos"`
}

var (
	reposConfigPath = filepath.Join(mustHomeDir(), ".claude", reposConfigBaseName)
	reposCacheDir   = filepath.Join(mustHomeDir(), ".claude", reposCacheBaseName)
)

// ── Config I/O ────────────────────────────────────────────────────────────────

func loadReposConfig() (*ReposConfig, error) {
	data, err := os.ReadFile(reposConfigPath)
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
	if err := os.MkdirAll(filepath.Dir(reposConfigPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(reposConfigPath, append(data, '\n'), 0o644)
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
	return filepath.Join(reposCacheDir, fmt.Sprintf("%x", h[:10]))
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
	Name         string // bare profile name (no alias prefix)
	QualifiedID  string // "name" for local, "alias/name" for repo
	JSONPath     string // absolute path to the JSON file
	SettingsPath string // absolute path to sibling settings.json, "" if absent
	RepoAlias    string // empty for local
}

// listRepoProfiles returns all profiles found in registered repos.
func listRepoProfiles() []ProfileLocation {
	cfg, err := loadReposConfig()
	if err != nil || len(cfg.Repos) == 0 {
		return nil
	}
	var out []ProfileLocation
	for _, r := range cfg.Repos {
		root := filepath.Join(repoCachePath(r.URL), reposProfileDir)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			jsonPath := filepath.Join(root, name, "profile.json")
			if _, err := os.Stat(jsonPath); err != nil {
				continue
			}
			settingsPath := filepath.Join(root, name, "settings.json")
			if _, err := os.Stat(settingsPath); err != nil {
				settingsPath = ""
			}
			out = append(out, ProfileLocation{
				Name:         name,
				QualifiedID:  r.Alias + "/" + name,
				JSONPath:     jsonPath,
				SettingsPath: settingsPath,
				RepoAlias:    r.Alias,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QualifiedID < out[j].QualifiedID })
	return out
}

// resolveProfileLocation finds a profile by name, accepting either a bare
// local name or an "alias/name" qualified form.
func resolveProfileLocation(id string) (*ProfileLocation, error) {
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
	return nil, fmt.Errorf("profile not found: %s", id)
}

// listAllLocations returns local + repo profiles, sorted with local first.
func listAllLocations() ([]ProfileLocation, error) {
	var out []ProfileLocation
	localNames, err := listProfiles()
	if err != nil {
		return nil, err
	}
	for _, n := range localNames {
		out = append(out, ProfileLocation{
			Name:        n,
			QualifiedID: n,
			JSONPath:    profilePath(n),
		})
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
