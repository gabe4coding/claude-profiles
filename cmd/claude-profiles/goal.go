package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// `claude-profiles goal` is a read-only view over the Agent View supervisor's
// state files: ~/.claude/jobs/<short-id>/state.json. Bg delegate sessions
// dispatched with `/delegate --bg --goal <name>` carry a "goal:<name> | …"
// prefix in their display name (see bgDisplayName); this command parses that
// prefix back out and groups sessions by goal.
//
// Sessions without a goal prefix are ignored — this is deliberately a
// best-effort grouping for users who opt in, not an exhaustive bg session
// browser (that's `claude agents`).

// goalSession is one row's worth of state we care about for grouping.
type goalSession struct {
	BgID  string // short id ("abc12345"), i.e. the jobs/<id> dirname
	Name  string // full display name as written by bgDisplayName
	Goal  string // parsed goal label (never empty in goalSession instances)
	Rest  string // display name with the "goal:<name> | " prefix stripped
	State string // working|blocked|completed|failed|stopped (raw from state.json)
}

func cmdGoal(args []string) {
	if len(args) == 0 {
		cmdGoalList()
		return
	}
	switch args[0] {
	case "list", "ls":
		cmdGoalList()
	case "show":
		if len(args) < 2 {
			fatal(fmt.Errorf("usage: claude-profiles goal show <name>"))
		}
		cmdGoalShow(args[1])
	default:
		fatal(fmt.Errorf("unknown goal subcommand: %s (use list or show <name>)", args[0]))
	}
}

func cmdGoalList() {
	sessions := loadGoalSessions()
	if len(sessions) == 0 {
		fmt.Println("No goal-tagged bg sessions. Dispatch with `/delegate --bg --goal <name> ...` to create one.")
		return
	}
	// Group by goal. Within a goal, count states.
	type counts struct {
		working, blocked, completed, other int
		total                              int
	}
	byGoal := map[string]*counts{}
	for _, s := range sessions {
		c, ok := byGoal[s.Goal]
		if !ok {
			c = &counts{}
			byGoal[s.Goal] = c
		}
		c.total++
		switch s.State {
		case "working":
			c.working++
		case "blocked":
			c.blocked++
		case "completed":
			c.completed++
		default:
			c.other++
		}
	}
	names := make([]string, 0, len(byGoal))
	for g := range byGoal {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		c := byGoal[g]
		// Keep the line compact and parseable: "<goal> → N total · W working, B blocked, C completed[, O other]"
		extra := ""
		if c.other > 0 {
			extra = fmt.Sprintf(", %d other", c.other)
		}
		fmt.Printf("%s → %d total · %d working, %d blocked, %d completed%s\n",
			g, c.total, c.working, c.blocked, c.completed, extra)
	}
}

func cmdGoalShow(name string) {
	if err := validateGoalName(name); err != nil {
		fatal(err)
	}
	sessions := loadGoalSessions()
	var matched []goalSession
	for _, s := range sessions {
		if s.Goal == name {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		fmt.Printf("No bg sessions tagged goal:%s.\n", name)
		return
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].State != matched[j].State {
			return matched[i].State < matched[j].State
		}
		return matched[i].BgID < matched[j].BgID
	})
	fmt.Printf("goal:%s — %d session(s)\n", name, len(matched))
	for _, s := range matched {
		fmt.Printf("  %s  [%s]  %s\n", s.BgID, s.State, s.Rest)
	}
}

// loadGoalSessions reads every ~/.claude/jobs/*/state.json, parses the .name
// field, and returns the subset whose name carries a `goal:<name> | ` prefix.
// Unreadable / malformed state.json files are skipped silently — this is a
// best-effort grouping view, not a debugger.
func loadGoalSessions() []goalSession {
	root := claudeJobsDir()
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []goalSession
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		statePath := filepath.Join(root, e.Name(), "state.json")
		state, err := readBgJobState(statePath)
		if err != nil {
			continue
		}
		if state.Name == "" {
			continue
		}
		goal := parseGoalFromName(state.Name)
		if goal == "" {
			continue
		}
		rest := strings.TrimPrefix(state.Name, goalPrefix+goal+goalDelim)
		out = append(out, goalSession{
			BgID:  e.Name(),
			Name:  state.Name,
			Goal:  goal,
			Rest:  rest,
			State: state.State,
		})
	}
	return out
}
