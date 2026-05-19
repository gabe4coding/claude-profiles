package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cmdShow prints a human-readable detail view of a single profile to stdout.
// Pure on-disk read — no MCP probing (use `probe` for that), no live tool
// listing — so it stays cheap and pipeable. Sections are omitted entirely
// when they have no content.
func cmdShow(args []string) {
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
	loc, err := resolveProfileLocation(arg)
	if err != nil {
		fatal(err)
	}
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		fatal(err)
	}
	writeProfileDetail(os.Stdout, loc, p)
}

// renderProfileDetail returns the same plain-text rendering that `cmdShow`
// prints — used by the hub's inline detail mode.
func renderProfileDetail(loc *ProfileLocation) string {
	p, err := loadProfileAt(loc.JSONPath)
	if err != nil {
		return fmt.Sprintf("(failed to load profile: %v)", err)
	}
	var b strings.Builder
	writeProfileDetail(&b, loc, p)
	return b.String()
}

func writeProfileDetail(w io.Writer, loc *ProfileLocation, p *Profile) {
	dir := filepath.Dir(loc.JSONPath)

	source := "local"
	switch {
	case loc.Builtin != "":
		source = "builtin"
	case loc.RepoAlias == ".":
		source = "project"
	case loc.RepoAlias != "":
		source = "repo:" + loc.RepoAlias
	}

	fmt.Fprintf(w, "Profile %s\n", loc.QualifiedID)
	if d := strings.TrimSpace(p.Description); d != "" {
		fmt.Fprintf(w, "\n%s\n", d)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  source:   %s\n", source)
	fmt.Fprintf(w, "  path:     %s\n", dir)
	if t, ok := loadRecents()[loc.QualifiedID]; ok {
		fmt.Fprintf(w, "  last run: %s\n", time.Unix(t, 0).Format("2006-01-02 15:04"))
	}

	if len(p.McpServers) > 0 {
		fmt.Fprintln(w, "\nMCP servers")
		names := sortedKeysOf(p.McpServers)
		for _, n := range names {
			cfg := p.McpServers[n]
			t := cfg.Type
			if t == "" {
				t = "http"
			}
			switch t {
			case "stdio":
				cmd := strings.TrimSpace(cfg.Command + " " + strings.Join(cfg.Args, " "))
				fmt.Fprintf(w, "  %s · stdio · %s\n", n, cmd)
			default:
				tok := "no token cached"
				if loadToken(cfg.URL) != "" {
					tok = "token cached"
				}
				fmt.Fprintf(w, "  %s · http · %s · %s\n", n, cfg.URL, tok)
			}
		}
	}

	if len(p.DeniedTools) > 0 {
		fmt.Fprintf(w, "\nDenied tools (%d)\n", len(p.DeniedTools))
		for _, t := range p.DeniedTools {
			fmt.Fprintf(w, "  %s\n", t)
		}
	}

	s := parseSettings(p.Settings)
	if len(s) > 0 {
		var rows [][2]string
		if m := getModel(s); m != "" {
			rows = append(rows, [2]string{"model", m})
		}
		if pm := getPermissionMode(s); pm != "" {
			rows = append(rows, [2]string{"permission mode", pm})
		}
		if a, _ := s["agent"].(string); a != "" {
			rows = append(rows, [2]string{"agent", a})
		}
		if sb := sandboxSummary(s); sb != "" {
			rows = append(rows, [2]string{"sandbox", sb})
		}
		if cmd := statusLineCommand(s); cmd != "" {
			rows = append(rows, [2]string{"statusLine", cmd})
		}
		if env := mapKeyList(s, "env"); env != "" {
			rows = append(rows, [2]string{"env vars", env})
		}
		if mk := mapKeyList(s, "extraKnownMarketplaces"); mk != "" {
			rows = append(rows, [2]string{"marketplaces", mk})
		}
		if pl := mapKeyList(s, "enabledPlugins"); pl != "" {
			rows = append(rows, [2]string{"plugins", pl})
		}
		if hk := mapKeyList(s, "hooks"); hk != "" {
			rows = append(rows, [2]string{"hooks", hk})
		}
		if len(rows) > 0 {
			fmt.Fprintln(w, "\nSettings")
			width := 0
			for _, r := range rows {
				if len(r[0]) > width {
					width = len(r[0])
				}
			}
			for _, r := range rows {
				fmt.Fprintf(w, "  %-*s  %s\n", width+1, r[0]+":", r[1])
			}
		}
	}

	var flags []string
	if p.Isolated {
		flags = append(flags, "isolated")
	}
	if p.Worktree {
		flags = append(flags, "worktree")
	}
	if strings.EqualFold(p.Distill, "on") {
		flags = append(flags, "distill")
	}
	if p.Cwd != "" {
		flags = append(flags, "cwd:"+p.Cwd)
	}
	if len(flags) > 0 {
		fmt.Fprintln(w, "\nFlags")
		for _, f := range flags {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}

	if len(p.Prompts) > 0 {
		fmt.Fprintf(w, "\nQuick-start prompts (%d)\n", len(p.Prompts))
		for _, q := range p.Prompts {
			preview := strings.ReplaceAll(q.Text, "\n", " ")
			if len(preview) > 80 {
				preview = preview[:77] + "..."
			}
			fmt.Fprintf(w, "  %s · %s\n", q.Name, preview)
		}
	}

	if kinds := profilePluginKinds(*loc); len(kinds) > 0 {
		fmt.Fprintln(w, "\nPlugin content")
		for _, k := range kinds {
			files, _ := os.ReadDir(filepath.Join(dir, k))
			noun := "entries"
			if len(files) == 1 {
				noun = "entry"
			}
			fmt.Fprintf(w, "  %s/  %d %s\n", k, len(files), noun)
		}
	}
}

// sandboxSummary formats settings.sandbox into a compact line like
// "enabled, write:2, denyW:3, denyR:1, domains:4" or "disabled" / "".
func sandboxSummary(s map[string]any) string {
	sb, ok := s["sandbox"].(map[string]any)
	if !ok {
		return ""
	}
	enabled, _ := sb["enabled"].(bool)
	if !enabled {
		return "disabled"
	}
	parts := []string{"enabled"}
	if fs, ok := sb["filesystem"].(map[string]any); ok {
		if aw, ok := fs["allowWrite"].([]any); ok && len(aw) > 0 {
			parts = append(parts, fmt.Sprintf("write:%d", len(aw)))
		}
		if dw, ok := fs["denyWrite"].([]any); ok && len(dw) > 0 {
			parts = append(parts, fmt.Sprintf("denyW:%d", len(dw)))
		}
		if dr, ok := fs["denyRead"].([]any); ok && len(dr) > 0 {
			parts = append(parts, fmt.Sprintf("denyR:%d", len(dr)))
		}
	}
	if net, ok := sb["network"].(map[string]any); ok {
		if ad, ok := net["allowedDomains"].([]any); ok && len(ad) > 0 {
			parts = append(parts, fmt.Sprintf("domains:%d", len(ad)))
		}
	}
	return strings.Join(parts, ", ")
}

// statusLineCommand returns settings.statusLine.command, the most useful
// field for telling at a glance what the bottom bar will render.
func statusLineCommand(s map[string]any) string {
	sl, ok := s["statusLine"].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := sl["command"].(string)
	return cmd
}

// mapKeyList returns a comma-separated, sorted list of the keys of the map at
// settings[key], or "" if the key is absent or not a map.
func mapKeyList(s map[string]any, key string) string {
	m, ok := s[key].(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func sortedKeysOf(m map[string]ServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
