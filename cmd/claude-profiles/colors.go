package main

import "github.com/charmbracelet/lipgloss"

// Claude design system palette (terminal-adjusted, light/dark adaptive).
//
//   coral  — signature Claude orange. Selection, hotkeys, primary accents.
//   cream  — warm off-white. Active backgrounds, title chips.
//   ink    — primary text. Adapts to terminal background.
//   muted  — secondary text, hints, separators.
//   sage   — success (green tinted toward warm).
//   amber  — warnings and read-only markers.
//   rust   — destructive states.
var (
	cdsCoral = lipgloss.AdaptiveColor{Light: "#C96442", Dark: "#E8896A"}
	cdsCream = lipgloss.AdaptiveColor{Light: "#F0EEE6", Dark: "#3A332C"}
	cdsInk   = lipgloss.AdaptiveColor{Light: "#1F1F1F", Dark: "#F0EEE6"}
	cdsMuted = lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#A8A29E"}
	cdsSage  = lipgloss.AdaptiveColor{Light: "#5B7C5B", Dark: "#83A98C"}
	cdsAmber = lipgloss.AdaptiveColor{Light: "#A56825", Dark: "#E8B86E"}
	cdsRust  = lipgloss.AdaptiveColor{Light: "#B23A48", Dark: "#E27D6F"}
)
