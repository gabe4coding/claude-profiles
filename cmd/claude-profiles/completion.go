package main

import (
	"fmt"
	"os"
)

// cmdComplete is the read-only feed used by shell completion scripts. One item
// per line on stdout; stderr is suppressed by the scripts so errors silently
// produce an empty candidate list rather than spam the shell.
//
// Kinds:
//   - profiles → all qualified profile ids (local, project, repo)
//   - repos    → all registered repo aliases
//
// Invoked on every TAB; main.go short-circuits migration + auto-sync for it
// so a press stays cheap and side-effect-free.
func cmdComplete(args []string) {
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "profiles":
		locs, err := listAllLocations()
		if err != nil {
			return
		}
		for _, loc := range locs {
			fmt.Println(loc.QualifiedID)
		}
	case "repos":
		cfg, err := loadReposConfig()
		if err != nil {
			return
		}
		for _, r := range cfg.Repos {
			fmt.Println(r.Alias)
		}
	}
}

// cmdCompletion prints the requested shell's completion script to stdout. The
// emitted scripts call back into `claude-profiles _complete <kind>` per TAB
// to discover live profile and repo names — so completion stays in sync as
// profiles are added/removed without re-sourcing.
func cmdCompletion(args []string) {
	shell := ""
	if len(args) > 0 {
		shell = args[0]
	}
	switch shell {
	case "bash":
		fmt.Print(bashCompletionScript)
	case "zsh":
		fmt.Print(zshCompletionScript)
	default:
		fmt.Fprintln(os.Stderr, "usage: claude-profiles completion <bash|zsh>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Wire it up by adding one of these to your shell rc:")
		fmt.Fprintln(os.Stderr, "  bash (~/.bashrc): eval \"$(claude-profiles completion bash)\"")
		fmt.Fprintln(os.Stderr, "  zsh  (~/.zshrc):  eval \"$(claude-profiles completion zsh)\"")
		os.Exit(1)
	}
}

const bashCompletionScript = `# bash completion for claude-profiles
_claude_profiles() {
  local cur cmd profiles repos
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"

  local subcmds="launch list ls new create ask run doctor analytics stats probe edit delete rm export import copy cp repo completion help version"

  if [ "$COMP_CWORD" -eq 1 ]; then
    profiles=$(claude-profiles _complete profiles 2>/dev/null)
    COMPREPLY=( $(compgen -W "$subcmds $profiles" -- "$cur") )
    return
  fi

  cmd="${COMP_WORDS[1]}"
  case "$cmd" in
    launch|run|edit|delete|rm|export|probe)
      profiles=$(claude-profiles _complete profiles 2>/dev/null)
      COMPREPLY=( $(compgen -W "$profiles" -- "$cur") )
      ;;
    copy|cp)
      profiles=$(claude-profiles _complete profiles 2>/dev/null | grep /)
      COMPREPLY=( $(compgen -W "$profiles" -- "$cur") )
      ;;
    completion)
      if [ "$COMP_CWORD" -eq 2 ]; then
        COMPREPLY=( $(compgen -W "bash zsh" -- "$cur") )
      fi
      ;;
    repo)
      if [ "$COMP_CWORD" -eq 2 ]; then
        COMPREPLY=( $(compgen -W "add list ls remove rm sync" -- "$cur") )
      else
        case "${COMP_WORDS[2]}" in
          remove|rm|sync)
            repos=$(claude-profiles _complete repos 2>/dev/null)
            COMPREPLY=( $(compgen -W "$repos" -- "$cur") )
            ;;
        esac
      fi
      ;;
  esac
}
complete -F _claude_profiles claude-profiles
`

const zshCompletionScript = `#compdef claude-profiles

_claude_profiles() {
  local -a subcmds repo_subcmds profiles repo_profiles repos
  subcmds=(
    'launch:Launch a profile'
    'list:List profiles'
    'new:Create a profile'
    'ask:Classify a prompt and launch the best-fit profile'
    'run:Launch claude in the /handoff wrapper'
    'doctor:Run sanity checks'
    'analytics:Context-window and cost stats'
    'probe:Probe MCP servers in a profile'
    'edit:Edit a profile'
    'delete:Delete a local profile'
    'export:Print profile JSON'
    'import:Import a profile JSON'
    'copy:Copy a repo profile to local'
    'repo:Manage registered repos'
    'completion:Emit a shell completion script'
    'help:Show help'
    'version:Print binary version'
  )
  repo_subcmds=(add list ls remove rm sync)
  profiles=( ${(f)"$(claude-profiles _complete profiles 2>/dev/null)"} )
  repo_profiles=( ${(M)profiles:#*/*} )
  repos=( ${(f)"$(claude-profiles _complete repos 2>/dev/null)"} )

  if (( CURRENT == 2 )); then
    _describe -t commands 'command' subcmds
    _describe -t profiles 'profile' profiles
    return
  fi

  case "${words[2]}" in
    launch|run|edit|delete|rm|export|probe)
      _describe -t profiles 'profile' profiles
      ;;
    copy|cp)
      _describe -t profiles 'repo profile' repo_profiles
      ;;
    completion)
      if (( CURRENT == 3 )); then
        _values 'shell' bash zsh
      fi
      ;;
    repo)
      if (( CURRENT == 3 )); then
        _values 'repo subcommand' $repo_subcmds
      else
        case "${words[3]}" in
          remove|rm|sync) _describe -t repos 'repo alias' repos ;;
        esac
      fi
      ;;
  esac
}

compdef _claude_profiles claude-profiles
`
