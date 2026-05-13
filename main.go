package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	args := os.Args[1:]
	// Fire background sync for every registered repo whose lastSync is stale.
	// Runs ahead of any command so repo profiles seen by this invocation come
	// from the freshest local cache. The sync itself completes async.
	kickAutoSync()

	if len(args) == 0 {
		cmdInteractive()
		return
	}
	switch args[0] {
	case "launch":
		cmdLaunch(args[1:])
	case "list", "ls":
		cmdList()
	case "new", "create":
		cmdNew()
	case "edit":
		cmdEdit(args[1:])
	case "delete", "rm":
		cmdDelete(args[1:])
	case "export":
		cmdExport(args[1:])
	case "import":
		cmdImport(args[1:])
	case "repo":
		cmdRepo(args[1:])
	case "copy", "cp":
		cmdCopy(args[1:])
	case "help", "--help", "-h":
		usage()
	default:
		// Treat as profile name shorthand (supports "alias/name" too)
		cmdLaunch(args)
	}
}

// ── interactive hub ───────────────────────────────────────────────────────────

func cmdInteractive() {
	hubMode = true
	defer func() { hubMode = false }()

	for {
		action := pickAction()
		if action == "quit" || action == "" {
			return
		}
		runHubAction(action)
	}
}

// runHubAction dispatches a hub action and recovers from errHubBack (Esc/Ctrl+C
// inside a sub-flow), allowing the loop to redisplay the menu.
func runHubAction(action string) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if r != errHubBack {
			panic(r) // not a hub-back; re-raise
		}
	}()
	switch action {
	case "launch":
		cmdLaunch(nil) // syscall.Exec on success — does not return
	case "new":
		cmdNew()
	case "edit":
		cmdEdit(nil)
	case "delete":
		cmdDelete(nil)
	case "export":
		cmdExport(nil)
	case "import":
		cmdImport(nil)
	}
}

// ── launch ────────────────────────────────────────────────────────────────────

func cmdLaunch(args []string) {
	id := ""
	if len(args) > 0 {
		id = args[0]
		args = args[1:]
	}
	loc, err := resolveProfileForLaunch(id)
	if err != nil {
		fatal(err)
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		fatal(err)
	}
	// Local folder-format profiles may also carry a sibling settings.json.
	settingsPath := loc.SettingsPath
	if settingsPath == "" && loc.RepoAlias == "" {
		settingsPath = localSettingsPath(loc.Name)
	}

	fmt.Fprintf(os.Stderr, "Launching Claude with profile: %s\n", loc.QualifiedID)

	claudeArgs := []string{"claude", "--strict-mcp-config", "--mcp-config", loc.JSONPath}
	claudeArgs = append(claudeArgs, claudeFlags(p, settingsPath)...)
	claudeArgs = append(claudeArgs, args...)

	binary, err := exec.LookPath("claude")
	if err != nil {
		fatal(fmt.Errorf("claude not found in PATH"))
	}
	if err := syscall.Exec(binary, claudeArgs, os.Environ()); err != nil {
		fatal(err)
	}
}

// resolveProfileForLaunch turns a user-supplied id (or "" for interactive) into
// a ProfileLocation, looking in both local profiles and registered repos.
func resolveProfileForLaunch(id string) (*ProfileLocation, error) {
	if id == "" {
		picked, err := pickProfile()
		if err != nil {
			return nil, err
		}
		id = picked
	}
	return resolveProfileLocation(id)
}

// ── list ──────────────────────────────────────────────────────────────────────

func cmdList() {
	locs, err := listAllLocations()
	if err != nil {
		fatal(err)
	}
	if len(locs) == 0 {
		fmt.Println("No profiles. Run: claude-profiles new")
		return
	}
	for _, loc := range locs {
		p, err := loadProfileAt(loc.JSONPath)
		var servers string
		var tags []string
		if err == nil {
			var snames []string
			for k := range p.McpServers {
				snames = append(snames, k)
			}
			servers = strings.Join(snames, ", ")
			if len(p.DeniedTools) > 0 {
				tags = append(tags, fmt.Sprintf("deny:%d", len(p.DeniedTools)))
			}
			if p.PermissionMode != "" {
				tags = append(tags, "mode:"+p.PermissionMode)
			}
			if p.Model != "" {
				tags = append(tags, "model:"+p.Model)
			}
			settingsPath := loc.SettingsPath
			if settingsPath == "" && loc.RepoAlias == "" {
				settingsPath = localSettingsPath(loc.Name)
			}
			if settingsPath != "" || len(p.Settings) > 0 {
				tags = append(tags, "settings")
			}
		}
		source := "[local]"
		if loc.RepoAlias != "" {
			source = "[repo:" + loc.RepoAlias + "]"
		}
		var extras string
		if len(tags) > 0 {
			extras = " [" + strings.Join(tags, " ") + "]"
		}
		fmt.Printf("%-30s %-10s %s%s\n", loc.QualifiedID, source, servers, extras)
	}
}

// ── new ───────────────────────────────────────────────────────────────────────

func cmdNew() {
	fmt.Fprintln(os.Stderr)
	title("=== New MCP Profile ===")
	fmt.Fprintln(os.Stderr)

	name := strings.ReplaceAll(prompt("Profile name"), " ", "-")
	if name == "" {
		fatal(fmt.Errorf("name required"))
	}
	if profileExists(name) {
		fatal(fmt.Errorf("profile %q already exists", name))
	}

	p := &Profile{McpServers: map[string]ServerConfig{}}

	for {
		fmt.Fprintln(os.Stderr)
		sname := prompt("Add server name (empty to finish)")
		if sname == "" {
			break
		}
		addServer(p, name, sname)
	}

	if len(p.McpServers) == 0 {
		fmt.Fprintln(os.Stderr, "No servers added — profile not saved.")
		os.Exit(1)
	}

	configureSettings(p)

	if err := saveProfile(name, p); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr)
	success("Profile %q saved with %d server(s) → %s", name, len(p.McpServers), profilePath(name))

	if confirm("Launch now?") {
		cmdLaunch([]string{name})
	}
}

func addServer(p *Profile, profileName, sname string) {
	t := pickServerType()
	var cfg ServerConfig

	if t == "stdio" {
		cmd := prompt("Command")
		argsStr := prompt("Args (space-separated, or empty)")
		var args []string
		if strings.TrimSpace(argsStr) != "" {
			args = strings.Fields(argsStr)
		}
		cfg = ServerConfig{Type: "stdio", Command: cmd, Args: args}
	} else {
		u := prompt("URL")
		cfg = ServerConfig{Type: "http", URL: u}
	}

	p.McpServers[sname] = cfg
	success("  + Added: %s", sname)

	probeAndFilter(p, profileName, sname, cfg)
}

func probeAndFilter(p *Profile, profileName, sname string, cfg ServerConfig) {
	fmt.Fprintf(os.Stderr, "  Fetching available tools from %q...\n", sname)

	tools, err := FetchTools(cfg, sname)
	if err != nil {
		if errors.Is(err, errNeedsAuth) {
			fmt.Fprintf(os.Stderr, "  (Could not authenticate — skipping filter setup)\n")
		} else {
			fmt.Fprintf(os.Stderr, "  (Could not reach server — skipping filter setup)\n")
		}
		return
	}

	fmt.Fprintf(os.Stderr, "  %d tool(s) found.\n", len(tools))
	selectToolFilter(p, sname, tools)

	// Persist after each server in case user exits early
	saveProfile(profileName, p)
}

// ── edit ──────────────────────────────────────────────────────────────────────

func cmdEdit(args []string) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	if arg == "" {
		picked, err := pickProfile()
		if err != nil {
			fatal(err)
		}
		arg = picked
	}
	if strings.Contains(arg, "/") {
		fatal(fmt.Errorf("repo profiles are read-only — run: claude-profiles copy %s <local-name>", arg))
	}
	if !profileExists(arg) {
		fatal(fmt.Errorf("profile not found: %s", arg))
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	binary, err := exec.LookPath(editor)
	if err != nil {
		fatal(fmt.Errorf("%s not found", editor))
	}
	syscall.Exec(binary, []string{editor, profilePath(arg)}, os.Environ())
}

// ── delete ────────────────────────────────────────────────────────────────────

func cmdDelete(args []string) {
	var name string
	if len(args) > 0 {
		name = args[0]
	} else {
		var err error
		name, err = pickProfile()
		if err != nil {
			fatal(err)
		}
	}
	if strings.Contains(name, "/") {
		fatal(fmt.Errorf("repo profiles can't be deleted from here — manage them in the source repo"))
	}
	if !profileExists(name) {
		fatal(fmt.Errorf("profile %q not found", name))
	}
	if !confirm(fmt.Sprintf("Delete %q?", name)) {
		return
	}
	// Remove flat OR folder format
	flat := filepath.Join(profilesDir, name+".json")
	folder := filepath.Join(profilesDir, name)
	os.Remove(flat)
	os.RemoveAll(folder)
	success("Deleted.")
}

// ── export ────────────────────────────────────────────────────────────────────

func cmdExport(args []string) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	name, err := resolveProfile(arg)
	if err != nil {
		fatal(err)
	}
	p, err := loadProfile(name)
	if err != nil {
		fatal(err)
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(data))
}

// ── import ────────────────────────────────────────────────────────────────────

func cmdImport(args []string) {
	var src string
	if len(args) > 0 {
		src = args[0]
	} else {
		src = prompt("File path to import")
		if src == "" {
			fatal(fmt.Errorf("file path required"))
		}
		src = expandPath(src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		fatal(err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil || p.McpServers == nil {
		fatal(fmt.Errorf("invalid profile: missing or malformed mcpServers"))
	}

	defaultName := strings.TrimSuffix(fileBase(src), ".json")
	name := strings.ReplaceAll(promptWithDefault("Profile name", defaultName), " ", "-")
	if name == "" {
		name = defaultName
	}

	if profileExists(name) {
		if !confirm(fmt.Sprintf("Profile %q already exists. Overwrite?", name)) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return
		}
	}

	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(profilePath(name), append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
	success("Imported %q → %s", name, profilePath(name))
}

// ── repo subcommands ──────────────────────────────────────────────────────────

func cmdRepo(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo {add|list|remove|sync} ..."))
	}
	switch args[0] {
	case "add":
		cmdRepoAdd(args[1:])
	case "list", "ls":
		cmdRepoList()
	case "remove", "rm":
		cmdRepoRemove(args[1:])
	case "sync":
		cmdRepoSync(args[1:])
	default:
		fatal(fmt.Errorf("unknown repo subcommand: %s", args[0]))
	}
}

func cmdRepoAdd(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo add <git-url> [--branch X] [--alias Y]"))
	}
	url := args[0]
	branch := ""
	alias := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--branch":
			if i+1 < len(args) {
				branch = args[i+1]
				i++
			}
		case "--alias":
			if i+1 < len(args) {
				alias = args[i+1]
				i++
			}
		}
	}
	if alias == "" {
		alias = defaultAlias(url)
	}

	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	if findRepo(cfg, alias) != nil {
		fatal(fmt.Errorf("alias already registered: %s", alias))
	}
	if findRepo(cfg, url) != nil {
		fatal(fmt.Errorf("URL already registered: %s", url))
	}

	r := RepoConfig{URL: url, Alias: alias, Branch: branch}
	fmt.Fprintf(os.Stderr, "Cloning %s → %s\n", url, repoCachePath(url))
	if err := cloneRepo(r); err != nil {
		fatal(fmt.Errorf("clone failed: %w", err))
	}
	r.LastSync = time.Now().Unix()
	r.LastSyncOK = true
	cfg.Repos = append(cfg.Repos, r)
	if err := saveReposConfig(cfg); err != nil {
		fatal(err)
	}
	success("Registered repo %q (alias: %s)", url, alias)
}

func cmdRepoList() {
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	if len(cfg.Repos) == 0 {
		fmt.Println("No repos registered. Run: claude-profiles repo add <url>")
		return
	}
	for _, r := range cfg.Repos {
		branch := r.Branch
		if branch == "" {
			branch = "<default>"
		}
		fmt.Printf("%-16s %-50s branch=%s  %s\n", r.Alias, r.URL, branch, r.syncStatus())
	}
}

func cmdRepoRemove(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles repo remove <alias-or-url>"))
	}
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	target := findRepo(cfg, args[0])
	if target == nil {
		fatal(fmt.Errorf("repo not found: %s", args[0]))
	}
	if !confirm(fmt.Sprintf("Remove repo %q and delete its cache?", target.Alias)) {
		return
	}
	cachePath := repoCachePath(target.URL)
	var newRepos []RepoConfig
	for _, r := range cfg.Repos {
		if r.Alias != target.Alias {
			newRepos = append(newRepos, r)
		}
	}
	cfg.Repos = newRepos
	if err := saveReposConfig(cfg); err != nil {
		fatal(err)
	}
	if err := os.RemoveAll(cachePath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove cache: %v\n", err)
	}
	success("Removed repo %q", target.Alias)
}

func cmdRepoSync(args []string) {
	cfg, err := loadReposConfig()
	if err != nil {
		fatal(err)
	}
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(os.Stderr, "No repos registered.")
		return
	}
	// Sync targets: all, or just the named one
	for i := range cfg.Repos {
		r := &cfg.Repos[i]
		if len(args) > 0 && r.Alias != args[0] && normaliseURL(r.URL) != normaliseURL(args[0]) {
			continue
		}
		fmt.Fprintf(os.Stderr, "Syncing %s...\n", r.Alias)
		if err := syncRepoForeground(r); err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
		} else {
			success("  + %s synced", r.Alias)
		}
	}
	saveReposConfig(cfg)
}

// ── copy ──────────────────────────────────────────────────────────────────────

func cmdCopy(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles copy <alias/profile> [<local-name>]"))
	}
	srcID := args[0]
	if !strings.Contains(srcID, "/") {
		fatal(fmt.Errorf("source must be a repo profile (alias/name); local profiles are already editable"))
	}
	loc, err := resolveProfileLocation(srcID)
	if err != nil {
		fatal(err)
	}

	dstName := loc.Name
	if len(args) >= 2 {
		dstName = args[1]
	} else {
		dstName = promptWithDefault("Local profile name", loc.Name)
	}
	dstName = strings.ReplaceAll(dstName, " ", "-")
	if profileExists(dstName) {
		if !confirm(fmt.Sprintf("Local profile %q exists. Overwrite?", dstName)) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return
		}
		// Remove existing in either format
		os.Remove(filepath.Join(profilesDir, dstName+".json"))
		os.RemoveAll(filepath.Join(profilesDir, dstName))
	}

	// If source has settings.json, write folder format; else flat.
	if loc.SettingsPath != "" {
		dstDir := filepath.Join(profilesDir, dstName)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			fatal(err)
		}
		if err := copyFile(loc.JSONPath, filepath.Join(dstDir, "profile.json")); err != nil {
			fatal(err)
		}
		if err := copyFile(loc.SettingsPath, filepath.Join(dstDir, "settings.json")); err != nil {
			fatal(err)
		}
	} else {
		if err := os.MkdirAll(profilesDir, 0o755); err != nil {
			fatal(err)
		}
		if err := copyFile(loc.JSONPath, filepath.Join(profilesDir, dstName+".json")); err != nil {
			fatal(err)
		}
	}
	success("Copied %s → %s (local)", srcID, dstName)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	if hubMode {
		fmt.Fprint(os.Stderr, "\nPress Enter to return to the menu...")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		panic(errHubBack)
	}
	os.Exit(1)
}

func fileBase(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}

func usage() {
	fmt.Printf(`Usage: claude-profiles [command] [profile] [claude-args...]

Commands:
  (none)             Interactive hub: launch / new / edit / delete / export / import
  launch [name]      Launch claude with profile (local "name" or "repo-alias/name")
  list               List all profiles (local + repos)
  new                Create a new profile interactively
  edit [name]        Open local profile in $EDITOR (interactive if omitted)
  delete [name]      Delete a local profile (interactive if omitted)
  export [name]      Print profile JSON to stdout (interactive if omitted)
  import [file]      Import a profile JSON file (prompts if omitted)
  copy <alias/name>  Copy a repo profile to local for editing
                       [<local-name>]
  repo add <url>     Register a remote repo (auto-clones on add)
                       [--branch X] [--alias Y]
  repo list          Show registered repos and last sync status
  repo remove <id>   Unregister a repo (by alias or URL) and delete cache
  repo sync [id]     Sync repos now (foreground). Auto-sync runs every 5min.
  help               Show this help

Profiles directory: %s
  Override with: CLAUDE_MCP_PROFILES_DIR=/path claude-profiles

Direct launch shorthand: claude-profiles <profile-name> [claude-args...]
`, profilesDir)
}
