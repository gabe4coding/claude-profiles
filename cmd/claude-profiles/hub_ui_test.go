package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/muesli/termenv"
)

// hubUITestSetup forces lipgloss to render with no ANSI escapes so View()
// output is stable across CI runners with different TERM / NO_COLOR values.
// Called once via init() since lipgloss's default renderer is process-wide.
func init() {
	lipgloss.SetColorProfile(termenv.Ascii)
}

// newTestHubModel builds a hubModel populated with the given profile
// locations and otherwise-empty maps. Mirrors the construction in runHub()
// but skips disk-backed sources (pins, recents, running, prefs) so the test
// doesn't depend on CLAUDE_PROFILES_ROOT or filesystem state. Returned by
// value so it matches the value-receiver Update/View methods.
func newTestHubModel(locs []ProfileLocation) hubModel {
	ti := textinput.New()
	ti.Focus()

	row := list.NewDefaultDelegate()
	row.SetSpacing(1)
	row.ShowDescription = true
	del := hubDelegate{rowDelegate: row}

	m := hubModel{
		input:      ti,
		delegate:   del,
		locs:       locs,
		pinMap:     map[string]PinEntry{},
		runningMap: map[string][]RunningWrapper{},
		bgMap:      map[string][]BackgroundedSession{},
		prefsStore: ProfilePrefsStore{},
	}
	m.rebuildIndex()

	l := list.New(m.fullItems, del, 0, 0)
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false)
	l.SetShowStatusBar(false)
	if idx := firstSelectableIndex(m.fullItems); idx >= 0 {
		l.Select(idx)
	}
	m.list = l
	return m
}

func sampleLocs() []ProfileLocation {
	return []ProfileLocation{
		{Name: "alpha", QualifiedID: "alpha", JSONPath: "/tmp/np-test/alpha/profile.json"},
		{Name: "beta", QualifiedID: "beta", JSONPath: "/tmp/np-test/beta/profile.json"},
	}
}

// TestHubRendersProfileNames: with two user-level profiles, the rendered
// view contains both names; Esc on empty input quits with actQuit.
func TestHubRendersProfileNames(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha")) && bytes.Contains(b, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actQuit {
		t.Fatalf("expected actQuit on Esc, got %q", fm.result.action)
	}
}

// TestHubFilterByTyping: typing into the input narrows the visible list to
// matching profiles. "alph" must keep alpha visible and hide beta.
func TestHubFilterByTyping(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	// Wait for first render so the typing isn't dropped on the floor.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha")) && bytes.Contains(b, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Type("alph")

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		// Output() is cumulative: we need beta to disappear from the LATEST
		// rendered frame, not from the buffer. Reading the tail covers that —
		// bubbletea redraws the full View on every update, so the most-recent
		// frame dominates the buffer once a few frames have flushed.
		// The shortcut: check the last 4 KiB only.
		tail := b
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return bytes.Contains(tail, []byte("alpha")) && !bytes.Contains(tail, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	// Esc clears the filter rather than quitting (input is non-empty).
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	// Second Esc with cleared input quits.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actQuit {
		t.Fatalf("expected actQuit after clear+quit, got %q", fm.result.action)
	}
}

// TestHubPaletteNewAction: Tab opens the action palette; pressing 'n' inside
// the palette quits the hub with actNew so the outer dispatcher creates a
// new profile.
func TestHubPaletteNewAction(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	// Wait for first render before opening palette.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	// Wait for the palette overlay to render (it shows the bracketed action
	// hotkeys — picking the New entry's hotkey letter as the signal).
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		tail := b
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		// Palette overlay renders entries like "n   new profile". Match the
		// uniquely-palette text without locking in to colour codes.
		return bytes.Contains(tail, []byte("new profile"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actNew {
		t.Fatalf("expected actNew from palette 'n', got %q", fm.result.action)
	}
}
