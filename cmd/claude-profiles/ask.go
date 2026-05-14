package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
)

// cmdAsk classifies a free-form user request and launches the best-fitting
// profile (or plain claude on "none") with the request as the initial prompt.
// If userPrompt is empty, the user is prompted for one — used when cmdAsk is
// invoked from a non-hub context. The hub passes the text it already gathered.
func cmdAsk(userPrompt string) {
	if userPrompt == "" {
		userPrompt = readAskPrompt()
	}
	if userPrompt == "" {
		return
	}

	choice, err := classifyProfile(userPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Classifier failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Falling back to plain claude...")
		_ = appendAskHistory(userPrompt, "")
		execClaudeBare(userPrompt)
		return
	}

	if choice == "" {
		info("→ No matching profile — launching plain claude with your prompt")
		_ = appendAskHistory(userPrompt, "")
		execClaudeBare(userPrompt)
		return
	}

	loc, err := resolveProfileLocation(choice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Classifier suggested %q but it doesn't resolve: %v\n", choice, err)
		_ = appendAskHistory(userPrompt, "")
		execClaudeBare(userPrompt)
		return
	}
	success("→ Using profile: %s", loc.QualifiedID)
	_ = appendAskHistory(userPrompt, loc.QualifiedID)
	cmdRun([]string{loc.QualifiedID, userPrompt})
}

// readAskPrompt asks the user for a multi-line prompt via huh.
func readAskPrompt() string {
	if !isTTY() {
		return promptLine("Prompt: ")
	}
	var out string
	err := runField(huh.NewText().
		Title("Ask").
		Description("Describe what you want to do. We'll pick the best profile and launch with this as the initial prompt.").
		Value(&out).
		CharLimit(4000))
	if err != nil {
		handleAbort(err)
		return ""
	}
	return strings.TrimSpace(out)
}

// classifyProfile asks haiku to pick a profile for the given user prompt.
// Returns an empty string when no profile is a good fit (the classifier
// answered "none" or returned something we don't recognise).
func classifyProfile(userPrompt string) (string, error) {
	locs, err := listAllLocations()
	if err != nil {
		return "", err
	}
	if len(locs) == 0 {
		return "", nil // No profiles to choose from
	}

	classifier := buildClassifierPrompt(locs, userPrompt)

	info("Classifying with haiku…")
	answer, err := runClassifier(classifier)
	if err != nil {
		return "", err
	}

	answer = strings.TrimSpace(answer)
	// Strip common wrappers ("foo" or `foo`)
	answer = strings.Trim(answer, "\"'`")
	if strings.EqualFold(answer, "none") {
		return "", nil
	}
	for _, loc := range locs {
		if loc.QualifiedID == answer {
			return answer, nil
		}
	}
	return "", nil
}

func buildClassifierPrompt(locs []ProfileLocation, userPrompt string) string {
	var sb strings.Builder
	sb.WriteString("You route a user request to the most appropriate Claude Code profile.\n\n")
	sb.WriteString("Available profiles:\n")
	for _, loc := range locs {
		p, _ := loadProfileAt(loc.JSONPath)
		desc := ""
		if p != nil && p.Description != "" {
			desc = " — " + p.Description
		}
		sb.WriteString(fmt.Sprintf("- %s%s\n", loc.QualifiedID, desc))
	}
	sb.WriteString("\nUser request:\n")
	sb.WriteString(userPrompt)
	sb.WriteString("\n\nRespond with ONLY the exact profile ID from the list, or \"none\" if no profile is a clear fit. No quotes, no explanation, no markdown.\n")
	return sb.String()
}

func runClassifier(classifierPrompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Skip loading the user's MCP servers, hooks, etc. — the classifier just
	// needs to read text and return a label.
	emptyMCP := filepath.Join(os.TempDir(), "claude-profiles-classifier-mcp.json")
	if err := os.WriteFile(emptyMCP, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "claude",
		"-p", "--model", "haiku",
		"--strict-mcp-config", "--mcp-config", emptyMCP,
		"--", classifierPrompt)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// execClaudeBare replaces the current process with `claude "<prompt>"` —
// no profile, no MCP filtering, just the user's normal global config.
func execClaudeBare(prompt string) {
	binary, err := exec.LookPath("claude")
	if err != nil {
		fatal(fmt.Errorf("claude not found in PATH"))
	}
	if err := syscall.Exec(binary, []string{"claude", "--", prompt}, os.Environ()); err != nil {
		fatal(err)
	}
}
