package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Built-in profiles are synthetic profile locations that don't have a directory
// on disk. They expose Claude Code's native settings hierarchy as first-class
// hub entries, so the user can launch claude through the wrapper (with
// /handoff, recents, ledger, tmux) without imposing any overlay of their own.
//
// They cover two gaps in the on-disk profile model:
//
//   :default — no --setting-sources flag, no --mcp-config. Equivalent to a
//              bare `claude` call, but inside the wrapper machinery.
//
//   :project — --setting-sources=project,local. Only .claude/settings.json,
//              .claude/settings.local.json and .mcp.json are loaded; the user's
//              ~/.claude/settings.json is suppressed. Note: CLAUDE.md hierarchy
//              is NOT controlled by --setting-sources, so ~/.claude/CLAUDE.md
//              still applies.

const (
	builtinDefaultID = ":default"
	builtinProjectID = ":project"
)

// builtinSpec describes one built-in profile. Kept private so the rest of the
// codebase routes through builtinLocations / builtinProfileFor instead of
// touching the list directly.
type builtinSpec struct {
	id          string
	kind        string // value stored in Profile.Builtin / ProfileLocation.Builtin
	description string
}

var builtinSpecs = []builtinSpec{
	{
		id:          builtinDefaultID,
		kind:        "default",
		description: "Plain claude — no overlay, all native settings sources. Useful when you want the wrapper (handoff, recents, ledger) without a profile imposing anything.",
	},
	{
		id:          builtinProjectID,
		kind:        "project",
		description: "Only repo-level settings: .claude/settings.json, .claude/settings.local.json, .mcp.json. Suppresses ~/.claude/settings.json. (CLAUDE.md hierarchy is unaffected — that's not controlled by --setting-sources.)",
	},
}

// builtinSentinelPath returns the synthetic JSONPath we attach to a built-in
// location. Path-shaped so filepath.Dir produces a unique, stable prefs-store
// key per built-in (otherwise every built-in would collide on ".").
func builtinSentinelPath(kind string) string {
	return filepath.Join("<builtin:"+kind+">", "profile.json")
}

// builtinKindFromPath returns ("kind", true) when path is a built-in sentinel.
func builtinKindFromPath(path string) (string, bool) {
	dir := filepath.Dir(path)
	if !strings.HasPrefix(dir, "<builtin:") || !strings.HasSuffix(dir, ">") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(dir, "<builtin:"), ">"), true
}

// builtinLocations returns synthetic locations for every built-in profile.
// Order matches builtinSpecs (default first, project second).
func builtinLocations() []ProfileLocation {
	out := make([]ProfileLocation, 0, len(builtinSpecs))
	for _, b := range builtinSpecs {
		out = append(out, ProfileLocation{
			Name:        b.id,
			QualifiedID: b.id,
			JSONPath:    builtinSentinelPath(b.kind),
			Builtin:     b.kind,
		})
	}
	return out
}

// resolveBuiltinLocation returns a synthetic ProfileLocation for id if it is a
// built-in identifier; returns nil otherwise.
func resolveBuiltinLocation(id string) *ProfileLocation {
	for _, b := range builtinSpecs {
		if b.id == id {
			return &ProfileLocation{
				Name:        b.id,
				QualifiedID: b.id,
				JSONPath:    builtinSentinelPath(b.kind),
				Builtin:     b.kind,
			}
		}
	}
	return nil
}

// validateNewProfileName returns an error when name would collide with the
// reserved built-in namespace or contain characters that break QualifiedID
// parsing ("/" is the repo-alias separator). Called from cmdNew / cmdImport /
// cmdCopy so the bad state never reaches disk.
func validateNewProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	if strings.HasPrefix(name, ":") {
		return fmt.Errorf("profile name %q is reserved — names starting with ':' belong to built-in profiles (:default, :project)", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("profile name %q cannot contain '/' or '\\' — reserved for repo qualifiers (alias/name)", name)
	}
	return nil
}

// builtinProfileFor returns the synthesized Profile for a built-in kind, or
// nil if kind isn't recognised.
func builtinProfileFor(kind string) *Profile {
	for _, b := range builtinSpecs {
		if b.kind == kind {
			return &Profile{
				Description: b.description,
				McpServers:  map[string]ServerConfig{},
				Builtin:     b.kind,
			}
		}
	}
	return nil
}
