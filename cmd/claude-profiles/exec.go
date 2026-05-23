package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// cmdExec resolves a profile and syscall.Execs into `claude` with the profile's
// MCP config, settings, plugin dir, and name. No wrapper loop, no tmux
// bootstrap, no /handoff or /delegate slash-command installs, no SessionStart
// or UserPromptSubmit hooks, no pidfile / recents / ask-history writes — a
// clean process replacement intended for CI and other non-interactive
// automation where the wrapper machinery is overhead.
//
// User args are appended verbatim, so `-p "<prompt>"` and `--resume <id>`
// pass straight through:
//
//	claude-profiles exec my-profile -p "do the thing"
//	claude-profiles exec my-profile --resume <session-id>
func cmdExec(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: claude-profiles exec <profile> [claude-args...]"))
	}
	profile := args[0]
	passThrough := args[1:]

	loc, err := resolveProfileLocation(profile)
	if err != nil {
		fatal(err)
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		fatal(err)
	}

	binary, err := exec.LookPath("claude")
	if err != nil {
		fatal(fmt.Errorf("claude not found in PATH"))
	}

	argv := []string{"claude"}
	// Real profiles: pin claude to the profile's MCP config (split format
	// prefers .mcp.json; combined falls back to profile.json). Built-ins: let
	// claude use native MCP discovery — they exist precisely to expose the
	// native hierarchy without a profile-imposed overlay. Same path is taken
	// for plugin-only profiles with no MCP file on disk.
	if loc.Builtin == "" {
		if mcp := resolveMCPConfigPath(*loc); mcp != "" {
			argv = append(argv, "--strict-mcp-config", "--mcp-config", mcp)
		}
	}
	// claudeFlags("") inlines p.Settings (no hook augmentation) and adds
	// --setting-sources= / --worktree when the profile requests them.
	argv = append(argv, claudeFlags(p, "")...)
	if dir := pluginDirFor(*loc); dir != "" {
		argv = append(argv, "--plugin-dir", dir)
	}
	argv = append(argv, "--name", loc.QualifiedID)
	argv = append(argv, passThrough...)

	if err := syscall.Exec(binary, argv, os.Environ()); err != nil {
		fatal(err)
	}
}
